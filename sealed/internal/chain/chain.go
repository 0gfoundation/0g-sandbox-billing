// Package chain wraps AgenticID contract reads used by the bootstrap pipeline:
// resolve agentId from sealId, list intelligent data, scan ITransferred for
// the latest sealedKey set, and fetch ownerOf.
//
// All functions take an ABI-bound *Client, dialled once at startup.
package chain

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"seal-verify/internal/logger"
)

// AgenticIDABI is the minimal subset bootstrap needs. sealedKey is NOT on
// IntelligentData; it lives on ITransferred event entries.
const AgenticIDABI = `[
  {"type":"function","name":"getAgentIdBySealId","stateMutability":"view","inputs":[{"name":"sealId","type":"bytes32"}],"outputs":[{"name":"","type":"uint256"}]},
  {"type":"function","name":"ownerOf","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"name":"","type":"address"}]},
  {"type":"function","name":"intelligentDatasOf","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"name":"","type":"tuple[]","components":[{"name":"dataDescription","type":"string"},{"name":"dataHash","type":"bytes32"}]}]},
  {"type":"event","name":"ITransferred","anonymous":false,"inputs":[{"name":"from","type":"address","indexed":true},{"name":"to","type":"address","indexed":true},{"name":"tokenId","type":"uint256","indexed":true},{"name":"entries","type":"tuple[]","indexed":false,"components":[{"name":"dataHash","type":"bytes32"},{"name":"sealedKey","type":"bytes"}]}]}
]`

// transferScanChunk is the backward-scan window for ITransferred. 0G testnet
// RPC rejects spans larger than 1000 with "query set is too large".
const transferScanChunk = 1000

// mintPollEvery is the polling interval for waitForMint.
const mintPollEvery = 5 * time.Second

// IntelligentData mirrors the on-chain struct returned by intelligentDatasOf.
type IntelligentData struct {
	DataDescription string
	DataHash        [32]byte
}

// SealedKeyEntry is one element of the ITransferred event's entries[] tuple.
type SealedKeyEntry struct {
	DataHash  [32]byte
	SealedKey []byte
}

// Client wraps an ethclient + parsed ABI + contract address. Construct once
// per process via Dial.
type Client struct {
	eth      *ethclient.Client
	abi      abi.ABI
	contract common.Address
}

// Dial connects to rpcURL and binds the AgenticID ABI at contractHex.
// Caller is responsible for c.Close() when done.
func Dial(ctx context.Context, rpcURL, contractHex string) (*Client, error) {
	eth, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dial rpc: %w", err)
	}
	parsedABI, err := abi.JSON(strings.NewReader(AgenticIDABI))
	if err != nil {
		eth.Close()
		return nil, fmt.Errorf("parse ABI: %w", err)
	}
	return &Client{
		eth:      eth,
		abi:      parsedABI,
		contract: common.HexToAddress(contractHex),
	}, nil
}

// Close releases the underlying RPC connection.
func (c *Client) Close() {
	if c.eth != nil {
		c.eth.Close()
	}
}

// WaitForMint polls getAgentIdBySealId(sealId) until non-zero or ctx is done.
// Returns the minted agentId.
func (c *Client) WaitForMint(ctx context.Context, sealID32 [32]byte) (*big.Int, error) {
	if id, err := c.GetAgentIdBySealId(ctx, sealID32); err == nil && id.Sign() > 0 {
		return id, nil
	}
	logger.Logf("waiting for mint (poll every %s)...", mintPollEvery)
	ticker := time.NewTicker(mintPollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		if id, err := c.GetAgentIdBySealId(ctx, sealID32); err == nil && id.Sign() > 0 {
			return id, nil
		}
	}
}

// GetAgentIdBySealId returns the agentId bound to sealId, or zero if none.
func (c *Client) GetAgentIdBySealId(ctx context.Context, sealID32 [32]byte) (*big.Int, error) {
	data, err := c.abi.Pack("getAgentIdBySealId", sealID32)
	if err != nil {
		return nil, err
	}
	out, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &c.contract, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.abi.Unpack("getAgentIdBySealId", out)
	if err != nil || len(res) == 0 {
		return nil, fmt.Errorf("unpack")
	}
	return res[0].(*big.Int), nil
}

// IntelligentDatasOf reads the agent's iData array.
func (c *Client) IntelligentDatasOf(ctx context.Context, agentID *big.Int) ([]IntelligentData, error) {
	data, err := c.abi.Pack("intelligentDatasOf", agentID)
	if err != nil {
		return nil, err
	}
	out, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &c.contract, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	var arr []IntelligentData
	if err := c.abi.UnpackIntoInterface(&arr, "intelligentDatasOf", out); err != nil {
		return nil, err
	}
	return arr, nil
}

// OwnerOf returns the ERC-721 NFT owner of agentID.
func (c *Client) OwnerOf(ctx context.Context, agentID *big.Int) (common.Address, error) {
	data, err := c.abi.Pack("ownerOf", agentID)
	if err != nil {
		return common.Address{}, err
	}
	out, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &c.contract, Data: data}, nil)
	if err != nil {
		return common.Address{}, err
	}
	res, err := c.abi.Unpack("ownerOf", out)
	if err != nil || len(res) == 0 {
		return common.Address{}, fmt.Errorf("unpack ownerOf")
	}
	addr, ok := res[0].(common.Address)
	if !ok {
		return common.Address{}, fmt.Errorf("ownerOf return type")
	}
	return addr, nil
}

// LoadSealedKeys finds the most recent ITransferred event for tokenId and
// returns the dataHash -> sealedKey map carried in that event.
//
// Strategy mirrors the old bootstrap.loadSealedKeys exactly:
//   - Phase 1: poll the head window (last transferScanChunk blocks) for up
//     to 30s. Some RPCs are load-balanced across nodes with slightly
//     different sync states; eth_call may reveal a freshly-minted agentId
//     while logs from the mint block are temporarily invisible elsewhere.
//   - Phase 2: chunked backward scan if Phase 1 exhausted.
func (c *Client) LoadSealedKeys(ctx context.Context, tokenID *big.Int) (map[[32]byte][]byte, error) {
	event, ok := c.abi.Events["ITransferred"]
	if !ok {
		return nil, fmt.Errorf("ITransferred not in ABI")
	}
	tokenTopic := common.BigToHash(tokenID)
	logger.Logf("ITransferred scan: tokenId=%s topic[3]=%s", tokenID.String(), tokenTopic.Hex())

	const (
		pollTimeout  = 30 * time.Second
		pollInterval = 3 * time.Second
	)

	tryHead := func(latest uint64) (map[[32]byte][]byte, error) {
		var from uint64
		if latest >= transferScanChunk {
			from = latest - transferScanChunk + 1
		}
		q := ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(from),
			ToBlock:   new(big.Int).SetUint64(latest),
			Addresses: []common.Address{c.contract},
			Topics:    [][]common.Hash{{event.ID}, nil, nil, {tokenTopic}},
		}
		logs, err := c.eth.FilterLogs(ctx, q)
		if err != nil {
			return nil, err
		}
		if len(logs) == 0 {
			return nil, nil
		}
		lg := logs[len(logs)-1]
		var ev struct {
			Entries []SealedKeyEntry
		}
		if err := c.abi.UnpackIntoInterface(&ev, "ITransferred", lg.Data); err != nil {
			return nil, fmt.Errorf("decode ITransferred log: %w", err)
		}
		out := map[[32]byte][]byte{}
		for _, e := range ev.Entries {
			out[e.DataHash] = e.SealedKey
		}
		logger.Logf("ITransferred found at block %d (head)", lg.BlockNumber)
		return out, nil
	}

	deadline := time.Now().Add(pollTimeout)
	var latest uint64
	for {
		latestNew, err := c.eth.BlockNumber(ctx)
		if err != nil {
			return nil, fmt.Errorf("BlockNumber: %w", err)
		}
		if latestNew != latest {
			logger.Logf("ITransferred poll: trying head [%d..%d]", latestNew-transferScanChunk+1, latestNew)
			latest = latestNew
			result, err := tryHead(latest)
			if err != nil {
				return nil, err
			}
			if result != nil {
				return result, nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	logger.Logf("ITransferred head poll exhausted; falling back to backward scan from %d", latest)

	to := latest
	if to >= transferScanChunk {
		to -= transferScanChunk
	} else {
		return nil, fmt.Errorf("no ITransferred for tokenId %s within head window", tokenID)
	}
	chunks := 0
	for {
		var from uint64
		if to >= transferScanChunk {
			from = to - transferScanChunk + 1
		}
		q := ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(from),
			ToBlock:   new(big.Int).SetUint64(to),
			Addresses: []common.Address{c.contract},
			Topics:    [][]common.Hash{{event.ID}, nil, nil, {tokenTopic}},
		}
		logs, err := c.eth.FilterLogs(ctx, q)
		chunks++
		if err != nil {
			return nil, fmt.Errorf("FilterLogs [%d..%d] (chunk %d): %w", from, to, chunks, err)
		}
		if len(logs) > 0 {
			lg := logs[len(logs)-1]
			var ev struct {
				Entries []SealedKeyEntry
			}
			if err := c.abi.UnpackIntoInterface(&ev, "ITransferred", lg.Data); err != nil {
				return nil, fmt.Errorf("decode ITransferred log: %w", err)
			}
			result := map[[32]byte][]byte{}
			for _, e := range ev.Entries {
				result[e.DataHash] = e.SealedKey
			}
			logger.Logf("ITransferred found at block %d (chunk %d)", lg.BlockNumber, chunks)
			return result, nil
		}
		if chunks%10 == 0 {
			logger.Logf("ITransferred scan: %d chunks searched, currently at [%d..%d]", chunks, from, to)
		}
		if from == 0 {
			return nil, fmt.Errorf("no ITransferred for tokenId %s in chain history (%d chunks scanned)", tokenID, chunks)
		}
		to = from - 1
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
}

// HexSealID parses a hex-encoded (with or without 0x prefix) seal_id to a
// fixed-size [32]byte. Convenience helper used at startup.
func HexSealID(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return out, fmt.Errorf("decode seal_id: %w", err)
	}
	if len(b) != 32 {
		return out, fmt.Errorf("seal_id must be 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

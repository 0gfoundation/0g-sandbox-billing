// Sealed container bootstrap.
//
// Pipeline:
//   1. Attestation self-check
//        - SANDBOX_SEAL_KEY derives the same compressed pubkey as attestation.pubkey.
//        - attestation.signature recovers the TEE signer (and matches
//          TEE_SIGNER_ADDRESS if set).
//
//   2. Provision (only if ATTESTOR_URL is set)
//        - POST /provision → ECIES-decrypt encrypted_agent_seal_priv with
//          SANDBOX_SEAL_KEY.
//
//   3. Bootstrap from AgenticID (only if CHAIN_RPC_URL is set)
//        - Phase 1: FilterLogs(AgentSealSet, sealId) — initial backward scan
//          plus forward poll. Returns (agentId, mintBlock).
//        - Phase 2: intelligentDatasOf(agentId).
//        - Phase 3: per-entry exec `0g-storage-client download`, exponential
//          backoff (2,4,8,16,32,60,60,…), 10 attempts.
//        - Phase 4: ECIES(agent_seal_priv, sealedKey) → data_key,
//          AES-GCM-256(data_key, nonce(12)||ciphertext+tag).
//
//   4. Status report — only if BOTH provision AND bootstrap succeed,
//      POST /status with status="running" signed by agent_seal_priv.
//
// HTTP server on :8080 exposes /dashboard (log), /healthz, and /hello (A2A).

package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	eciesgo "github.com/ecies/go/v2"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	logPath           = "/tmp/seal-bootstrap.log"
	transferScanChunk = 5000 // ITransferred backward scan chunk size
	bootstrapTimeout  = 10 * time.Minute
	mintPollEvery     = 5 * time.Second
	downloadAttempts  = 10
)

// Minimal ABI subset we use against AgenticID. Full ABI is at
// contracts/out/AgenticID.sol/AgenticID.json. sealedKey is NOT stored on
// IntelligentData — it is emitted in the ITransferred event (mint + transfers).
const agenticIDABI = `[
  {"type":"function","name":"getAgentIdBySealId","stateMutability":"view","inputs":[{"name":"sealId","type":"bytes32"}],"outputs":[{"name":"","type":"uint256"}]},
  {"type":"function","name":"intelligentDatasOf","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"name":"","type":"tuple[]","components":[{"name":"dataDescription","type":"string"},{"name":"dataHash","type":"bytes32"}]}]},
  {"type":"event","name":"ITransferred","anonymous":false,"inputs":[{"name":"from","type":"address","indexed":true},{"name":"to","type":"address","indexed":true},{"name":"tokenId","type":"uint256","indexed":true},{"name":"entries","type":"tuple[]","indexed":false,"components":[{"name":"dataHash","type":"bytes32"},{"name":"sealedKey","type":"bytes"}]}]}
]`

type attestation struct {
	SealID    string `json:"seal_id"`
	Pubkey    string `json:"pubkey"`
	ImageHash string `json:"image_hash"`
	Signature string `json:"signature"`
	Ts        int64  `json:"ts"`
}

type intelligentData struct {
	DataDescription string
	DataHash        [32]byte
}

type sealedKeyEntry struct {
	DataHash  [32]byte
	SealedKey []byte
}

type storageDescription struct {
	Role       string `json:"role"`
	StoragePtr struct {
		Indexer  string `json:"indexer"`
		RootHash string `json:"root_hash"`
	} `json:"storage_ptr"`
}

var (
	lines   []string
	linesMu sync.RWMutex
)

func logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Println(msg)
	linesMu.Lock()
	lines = append(lines, msg)
	linesMu.Unlock()
}

func fail(format string, a ...any) {
	msg := "FAIL: " + fmt.Sprintf(format, a...)
	fmt.Fprintln(os.Stderr, msg)
	linesMu.Lock()
	lines = append(lines, msg)
	linesMu.Unlock()
	flush()
	os.Exit(1)
}

func flush() {
	linesMu.RLock()
	body := strings.Join(lines, "\n") + "\n"
	linesMu.RUnlock()
	os.WriteFile(logPath, []byte(body), 0644) //nolint:errcheck
}

func currentLog() string {
	linesMu.RLock()
	defer linesMu.RUnlock()
	return strings.Join(lines, "\n") + "\n"
}

func main() {
	sealKey := os.Getenv("SANDBOX_SEAL_KEY")
	attestRaw := os.Getenv("SANDBOX_SEAL_ATTESTATION")
	teeSigner := os.Getenv("TEE_SIGNER_ADDRESS")
	apiKey := os.Getenv("API_KEY")

	if sealKey == "" {
		fail("SANDBOX_SEAL_KEY not set")
	}
	if attestRaw == "" {
		fail("SANDBOX_SEAL_ATTESTATION not set")
	}

	logf("--- Sealed Container Bootstrap ---")
	logf("")

	var a attestation
	if err := json.Unmarshal([]byte(attestRaw), &a); err != nil {
		fail("SANDBOX_SEAL_ATTESTATION is not valid JSON: %v", err)
	}
	if a.SealID == "" || a.Pubkey == "" || a.ImageHash == "" || a.Signature == "" {
		fail("attestation missing required fields")
	}
	logf("seal_id:    %s", a.SealID)
	logf("pubkey:     %s", a.Pubkey)
	logf("image_hash: %s", a.ImageHash)
	logf("ts:         %d", a.Ts)
	logf("")

	// ── Phase 0: keypair + TEE signature self-check ─────────────────────────
	keyBytes, err := hex.DecodeString(strings.TrimPrefix(sealKey, "0x"))
	if err != nil {
		fail("decode SANDBOX_SEAL_KEY: %v", err)
	}
	privKey, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		fail("parse SANDBOX_SEAL_KEY: %v", err)
	}
	derived := "0x" + hex.EncodeToString(crypto.CompressPubkey(&privKey.PublicKey))
	if !strings.EqualFold(derived, a.Pubkey) {
		fail("keypair mismatch\n  derived : %s\n  pubkey  : %s", derived, a.Pubkey)
	}
	logf("OK   keypair match: SANDBOX_SEAL_KEY -> %s", derived)

	canonical := fmt.Sprintf("ImageAttestation:%s:%s:%s:%d", a.SealID, a.Pubkey, a.ImageHash, a.Ts)
	hash := crypto.Keccak256Hash([]byte(canonical))
	sigBytes, err := hex.DecodeString(strings.TrimPrefix(a.Signature, "0x"))
	if err != nil {
		fail("decode signature: %v", err)
	}
	if len(sigBytes) != 65 {
		fail("signature must be 65 bytes, got %d", len(sigBytes))
	}
	sigBytes[64] -= 27
	pub, err := crypto.SigToPub(hash[:], sigBytes)
	if err != nil {
		fail("recover TEE signer: %v", err)
	}
	recovered := crypto.PubkeyToAddress(*pub).Hex()
	logf("OK   TEE signature valid, signer: %s", recovered)
	if teeSigner != "" {
		if !strings.EqualFold(recovered, teeSigner) {
			fail("TEE signer mismatch\n  recovered: %s\n  expected : %s", recovered, teeSigner)
		}
		logf("OK   TEE signer matches TEE_SIGNER_ADDRESS: %s", teeSigner)
	}
	logf("")
	if apiKey != "" {
		logf("API_KEY (from env): %s", apiKey)
	} else {
		logf("API_KEY (from env): <unset>")
	}

	// Start the HTTP server early so /dashboard is reachable while bootstrap is
	// still running (provision + waitForMint + scan can take minutes).
	startHTTPServer()

	// Phase 1+2: provision + bootstrap, only report running on full success.
	attestorURL := os.Getenv("ATTESTOR_URL")
	chainRPC := os.Getenv("CHAIN_RPC_URL")
	contractAddr := os.Getenv("AGENTIC_ID_ADDR")
	fallbackIndexer := os.Getenv("INDEXER_URL")

	if attestorURL == "" {
		logf("")
		logf("ATTESTOR_URL unset -- skipping provision / bootstrap / status")
	} else if chainRPC == "" || contractAddr == "" || fallbackIndexer == "" {
		logf("")
		logf("missing required env (CHAIN_RPC_URL=%q AGENTIC_ID_ADDR=%q INDEXER_URL=%q) -- skipping provision / bootstrap / status",
			chainRPC, contractAddr, fallbackIndexer)
	} else {
		logf("")
		logf("--- Provisioning from attestor: %s ---", attestorURL)
		agentSealPriv := provisionFromAttestor(attestorURL, keyBytes, a)
		if agentSealPriv != nil {
			logf("")
			logf("--- Bootstrap from AgenticID %s (rpc %s, fallback indexer %s) ---",
				contractAddr, chainRPC, fallbackIndexer)
			if bootstrap(chainRPC, contractAddr, a.SealID, agentSealPriv, fallbackIndexer) {
				reportStatus(attestorURL, agentSealPriv, a.SealID, "running", "")
			} else {
				logf("bootstrap failed -- not reporting status=running")
			}
		}
	}

	logf("")
	logf("ALL DONE")
	flush()

	// Block forever — HTTP server is already running in its own goroutine.
	select {}
}

// startHTTPServer launches a goroutine serving the container's HTTP surface
// on :8080.
//
//	GET /dashboard — owner-facing log / instruction surface (reads the
//	                 accumulated log; reachable while bootstrap runs).
//	GET /healthz   — liveness probe.
//	GET /hello     — A2A placeholder; logs the call and replies "hello".
func startHTTPServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, currentLog())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		logf("/hello called from %s", r.RemoteAddr)
		fmt.Fprintln(w, "hello")
	})
	go func() {
		fmt.Println("Listening on :8080  GET /dashboard | /healthz | /hello")
		_ = http.ListenAndServe(":8080", mux)
	}()
}

// ── Attestor: /provision ────────────────────────────────────────────────────

func provisionFromAttestor(attestorURL string, sealKeyBytes []byte, a attestation) []byte {
	imageHashHex := strings.TrimPrefix(a.ImageHash, "sha256:")
	reqBody, _ := json.Marshal(map[string]any{
		"seal_id":           "0x" + a.SealID,
		"container_pubkey":  a.Pubkey,
		"image_hash":        "0x" + imageHashHex,
		"issued_at":         a.Ts,
		"sandbox_signature": a.Signature,
	})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(attestorURL+"/provision", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		logf("FAIL provision: POST error: %v", err)
		return nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logf("FAIL provision: HTTP %d: %s", resp.StatusCode, string(body))
		return nil
	}
	var out struct {
		EncryptedAgentSealPriv string `json:"encrypted_agent_seal_priv"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		logf("FAIL provision: decode response: %v", err)
		return nil
	}
	if out.EncryptedAgentSealPriv == "" {
		logf("FAIL provision: empty encrypted_agent_seal_priv")
		return nil
	}
	ctBytes, err := hex.DecodeString(strings.TrimPrefix(out.EncryptedAgentSealPriv, "0x"))
	if err != nil {
		logf("FAIL provision: decode ciphertext hex: %v", err)
		return nil
	}
	priv := eciesgo.NewPrivateKeyFromBytes(sealKeyBytes)
	plaintext, err := eciesgo.Decrypt(priv, ctBytes)
	if err != nil {
		logf("FAIL provision: ECIES decrypt: %v", err)
		return nil
	}
	logf("OK   provisioned agent_seal_priv: 0x%s", hex.EncodeToString(plaintext))
	return plaintext
}

// ── Attestor: /status ───────────────────────────────────────────────────────

// Canonical message: "StatusReport:<seal_id_0x>:<status>:<error_detail>"
// hashed with raw keccak256 (no EIP-191), V=27/28. Signed by agent_seal_priv.
func reportStatus(attestorURL string, agentSealPriv []byte, sealID, status, errorDetail string) {
	msg := fmt.Sprintf("StatusReport:0x%s:%s:%s", sealID, status, errorDetail)
	hash := crypto.Keccak256([]byte(msg))
	priv, err := crypto.ToECDSA(agentSealPriv)
	if err != nil {
		logf("FAIL status: parse agent priv: %v", err)
		return
	}
	sig, err := crypto.Sign(hash, priv)
	if err != nil {
		logf("FAIL status: sign: %v", err)
		return
	}
	sig[64] += 27

	reqBody, _ := json.Marshal(map[string]any{
		"seal_id":              "0x" + sealID,
		"status":               status,
		"error_detail":         errorDetail,
		"agent_seal_signature": "0x" + hex.EncodeToString(sig),
	})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(attestorURL+"/status", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		logf("FAIL status: POST error: %v", err)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logf("FAIL status: HTTP %d: %s", resp.StatusCode, string(respBody))
		return
	}
	logf("OK   status reported: %s", status)
}

// ── Bootstrap: AgenticID watcher + storage download + decrypt ───────────────

// bootstrap returns true only when every phase succeeds (mint observed,
// i_data list fetched, every entry downloaded AND decrypted). Any failure
// returns false; details are logged.
//
// fallbackIndexer is used when an i_data's dataDescription does not contain
// storage_ptr.indexer. Empty string disables the fallback.
func bootstrap(rpcURL, contractHex, sealIDHex string, agentSealPriv []byte, fallbackIndexer string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer cancel()

	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		logf("FAIL bootstrap: dial RPC: %v", err)
		return false
	}
	defer client.Close()

	parsedABI, err := abi.JSON(strings.NewReader(agenticIDABI))
	if err != nil {
		logf("FAIL bootstrap: parse ABI: %v", err)
		return false
	}
	contract := common.HexToAddress(contractHex)

	sealIDBytes, err := hex.DecodeString(strings.TrimPrefix(sealIDHex, "0x"))
	if err != nil || len(sealIDBytes) != 32 {
		logf("FAIL bootstrap: seal_id must be 32 bytes hex: %v", err)
		return false
	}
	var sealID32 [32]byte
	copy(sealID32[:], sealIDBytes)

	// Phase 1: poll getAgentIdBySealId(sealId) until non-zero.
	agentID, err := waitForMint(ctx, client, parsedABI, contract, sealID32)
	if err != nil {
		logf("FAIL bootstrap: wait for mint: %v", err)
		return false
	}
	logf("OK   minted agent_id: %s", agentID.String())

	// Phase 2: list i_data (descriptions + dataHashes; sealedKeys live on ITransferred)
	iDatas, err := intelligentDatasOf(ctx, client, parsedABI, contract, agentID)
	if err != nil {
		logf("FAIL bootstrap: intelligentDatasOf: %v", err)
		return false
	}
	logf("OK   intelligent_datas: %d entries", len(iDatas))

	// Phase 2b: backward-scan ITransferred for tokenId until we find the most
	// recent entry (mint or last transfer = current sealedKeys).
	sealedKeys, err := loadSealedKeys(ctx, client, parsedABI, contract, agentID)
	if err != nil {
		logf("FAIL bootstrap: load sealedKeys from ITransferred: %v", err)
		return false
	}
	logf("OK   sealedKeys collected: %d entries", len(sealedKeys))

	// Phase 3 + 4: per-entry download + decrypt
	allOK := true
	for i, d := range iDatas {
		sealed, ok := sealedKeys[d.DataHash]
		if !ok {
			logf("FAIL bootstrap[%d]: no sealedKey for dataHash 0x%s", i, hex.EncodeToString(d.DataHash[:]))
			allOK = false
			continue
		}
		if !processIntelligentData(ctx, i, d, sealed, agentSealPriv, fallbackIndexer) {
			allOK = false
		}
	}
	if !allOK {
		return false
	}
	logf("OK   bootstrap complete")
	return true
}

// waitForMint polls getAgentIdBySealId(sealId) until non-zero (or ctx done).
func waitForMint(ctx context.Context, client *ethclient.Client, parsedABI abi.ABI, contract common.Address, sealID32 [32]byte) (*big.Int, error) {
	if id, err := getAgentIdBySealId(ctx, client, parsedABI, contract, sealID32); err == nil && id.Sign() > 0 {
		return id, nil
	}
	logf("waiting for mint (poll every %s)...", mintPollEvery)
	ticker := time.NewTicker(mintPollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		if id, err := getAgentIdBySealId(ctx, client, parsedABI, contract, sealID32); err == nil && id.Sign() > 0 {
			return id, nil
		}
	}
}

func getAgentIdBySealId(ctx context.Context, client *ethclient.Client, parsedABI abi.ABI, contract common.Address, sealID32 [32]byte) (*big.Int, error) {
	data, err := parsedABI.Pack("getAgentIdBySealId", sealID32)
	if err != nil {
		return nil, err
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &contract, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	res, err := parsedABI.Unpack("getAgentIdBySealId", out)
	if err != nil || len(res) == 0 {
		return nil, fmt.Errorf("unpack")
	}
	return res[0].(*big.Int), nil
}

// loadSealedKeys finds the most recent ITransferred for tokenId.
//
// Two phases:
//
//  1. Forward poll on the head window. Some RPCs are load-balanced across
//     nodes with slightly different sync states — eth_call against state node
//     may show a freshly-minted agent_id while eth_blockNumber against another
//     node is still N blocks behind, so logs from the mint block are
//     temporarily invisible. Re-fetch latest every few seconds and retry
//     the head window for up to pollTimeout before giving up.
//
//  2. Backward chunked scan. If the log isn't on the head, the agent was
//     minted long ago — walk backwards in transferScanChunk windows.
func loadSealedKeys(ctx context.Context, client *ethclient.Client, parsedABI abi.ABI, contract common.Address, tokenID *big.Int) (map[[32]byte][]byte, error) {
	event, ok := parsedABI.Events["ITransferred"]
	if !ok {
		return nil, fmt.Errorf("ITransferred not in ABI")
	}
	tokenTopic := common.BigToHash(tokenID)
	logf("ITransferred scan: tokenId=%s topic[3]=%s", tokenID.String(), tokenTopic.Hex())

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
			Addresses: []common.Address{contract},
			Topics:    [][]common.Hash{{event.ID}, nil, nil, {tokenTopic}},
		}
		logs, err := client.FilterLogs(ctx, q)
		if err != nil {
			return nil, err
		}
		if len(logs) == 0 {
			return nil, nil
		}
		lg := logs[len(logs)-1]
		var ev struct {
			Entries []sealedKeyEntry
		}
		if err := parsedABI.UnpackIntoInterface(&ev, "ITransferred", lg.Data); err != nil {
			return nil, fmt.Errorf("decode ITransferred log: %w", err)
		}
		out := map[[32]byte][]byte{}
		for _, e := range ev.Entries {
			out[e.DataHash] = e.SealedKey
		}
		logf("ITransferred found at block %d (head)", lg.BlockNumber)
		return out, nil
	}

	// Phase 1: poll the head window, re-fetching latest each iteration.
	deadline := time.Now().Add(pollTimeout)
	var latest uint64
	for {
		latestNew, err := client.BlockNumber(ctx)
		if err != nil {
			return nil, fmt.Errorf("BlockNumber: %w", err)
		}
		if latestNew != latest {
			logf("ITransferred poll: trying head [%d..%d]", latestNew-transferScanChunk+1, latestNew)
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
	logf("ITransferred head poll exhausted; falling back to backward scan from %d", latest)

	// Phase 2: chunked backward scan.
	to := latest
	if to >= transferScanChunk {
		to -= transferScanChunk // already covered by Phase 1
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
			Addresses: []common.Address{contract},
			Topics:    [][]common.Hash{{event.ID}, nil, nil, {tokenTopic}},
		}
		logs, err := client.FilterLogs(ctx, q)
		chunks++
		if err != nil {
			return nil, fmt.Errorf("FilterLogs [%d..%d] (chunk %d): %w", from, to, chunks, err)
		}
		if len(logs) > 0 {
			lg := logs[len(logs)-1]
			var ev struct {
				Entries []sealedKeyEntry
			}
			if err := parsedABI.UnpackIntoInterface(&ev, "ITransferred", lg.Data); err != nil {
				return nil, fmt.Errorf("decode ITransferred log: %w", err)
			}
			result := map[[32]byte][]byte{}
			for _, e := range ev.Entries {
				result[e.DataHash] = e.SealedKey
			}
			logf("ITransferred found at block %d (chunk %d)", lg.BlockNumber, chunks)
			return result, nil
		}
		if chunks%10 == 0 {
			logf("ITransferred scan: %d chunks searched, currently at [%d..%d]", chunks, from, to)
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

func intelligentDatasOf(ctx context.Context, client *ethclient.Client, parsedABI abi.ABI, contract common.Address, agentID *big.Int) ([]intelligentData, error) {
	data, err := parsedABI.Pack("intelligentDatasOf", agentID)
	if err != nil {
		return nil, err
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &contract, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	var arr []intelligentData
	if err := parsedABI.UnpackIntoInterface(&arr, "intelligentDatasOf", out); err != nil {
		return nil, err
	}
	return arr, nil
}

func processIntelligentData(ctx context.Context, idx int, d intelligentData, sealedKey, agentSealPriv []byte, fallbackIndexer string) bool {
	tag := fmt.Sprintf("[%d]", idx)
	dataHashHex := "0x" + hex.EncodeToString(d.DataHash[:])

	var desc storageDescription
	if err := json.Unmarshal([]byte(d.DataDescription), &desc); err != nil {
		logf("FAIL bootstrap%s parse dataDescription: %v", tag, err)
		return false
	}
	indexer := desc.StoragePtr.Indexer
	if indexer == "" {
		indexer = fallbackIndexer
	}
	if indexer == "" {
		logf("FAIL bootstrap%s no indexer (description.storage_ptr.indexer empty and no fallback)", tag)
		return false
	}
	logf("bootstrap%s data=%s role=%q indexer=%s", tag, dataHashHex, desc.Role, indexer)

	outPath := fmt.Sprintf("/tmp/idata-%s.bin", hex.EncodeToString(d.DataHash[:]))
	if err := downloadWithRetry(ctx, dataHashHex, indexer, outPath); err != nil {
		logf("FAIL bootstrap%s download: %v", tag, err)
		return false
	}
	blob, err := os.ReadFile(outPath)
	if err != nil {
		logf("FAIL bootstrap%s read downloaded file: %v", tag, err)
		return false
	}

	// ECIES sealedKey -> data_key (32 bytes)
	dataKey, err := eciesgo.Decrypt(eciesgo.NewPrivateKeyFromBytes(agentSealPriv), sealedKey)
	if err != nil {
		logf("FAIL bootstrap%s ECIES decrypt sealedKey: %v", tag, err)
		return false
	}

	// AES-GCM-256: blob = nonce(12) || ciphertext+tag(16 at end)
	if len(blob) < 12+16 {
		logf("FAIL bootstrap%s blob too short (%d bytes)", tag, len(blob))
		return false
	}
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		logf("FAIL bootstrap%s AES new cipher: %v", tag, err)
		return false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		logf("FAIL bootstrap%s GCM init: %v", tag, err)
		return false
	}
	plaintext, err := gcm.Open(nil, blob[:12], blob[12:], nil)
	if err != nil {
		logf("FAIL bootstrap%s AES-GCM open: %v", tag, err)
		return false
	}

	logf("OK   bootstrap%s plaintext (%d bytes): %s", tag, len(plaintext), string(plaintext))
	return true
}

func downloadWithRetry(ctx context.Context, root, indexer, outPath string) error {
	var lastErr error
	for i := 0; i < downloadAttempts; i++ {
		if i > 0 {
			delay := 1 << uint(i)
			if delay > 60 {
				delay = 60
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(delay) * time.Second):
			}
		}
		// 0g-storage-client refuses to overwrite; remove leftover from a
		// previous partial / failed download (or container restart) first.
		_ = os.Remove(outPath)
		cmd := exec.CommandContext(ctx, "0g-storage-client", "download",
			"--root", root, "--file", outPath, "--indexer", indexer)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("attempt %d: %v: %s", i+1, err, strings.TrimSpace(string(out)))
	}
	return lastErr
}

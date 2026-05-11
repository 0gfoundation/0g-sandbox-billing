// Package uploader pushes a drifted iData dim to chain. End-to-end flow:
//
//  1. adapter.EvolutionFor(dim) -> plaintext bytes
//  2. dataplane.NewDataKey + Encrypt -> ciphertext
//  3. dataplane.Upload -> 0g-storage root_hash (= new dataHash on chain)
//  4. dataplane.SealDataKey -> sealedKey wrapping the data_key for the
//     agent's own seal pubkey (so future restarts can decrypt)
//  5. chain.IntelligentDatasOf + chain.SealedKeysOf -> current arrays
//  6. label-keyed merge: replace the changed dim's entry, keep all others
//  7. chain.Update -> single tx with the full new arrays
//  8. agent.RecordChainUpload -> sync chainSnapshot to reflect the new
//     on-chain state (HasChanges() for this dim becomes false)
//
// Caller (watcher OnDimDrift handler) decides WHEN to call Push; this
// package only handles the HOW.
package uploader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/crypto"

	"seal-verify/internal/chain"
	"seal-verify/internal/dataplane"
	"seal-verify/internal/framework/openclaw"
	"seal-verify/internal/logger"
	"seal-verify/internal/state"
)

// onChainDescription mirrors the JSON layout the attestor writes into
// IntelligentData.dataDescription at mint time (see attestor crate
// worker/src/jobs.rs build of `on_chain_description`). Sealed reads only
// role + storage_ptr; extra fields are kept verbatim so we don't lose
// metadata attestor may surface later. size is the ciphertext length in
// bytes; encryption is fixed to AES-GCM-256 to match attestor.
type onChainDescription struct {
	Role       string     `json:"role"`
	StoragePtr storagePtr `json:"storage_ptr"`
	Encryption string     `json:"encryption"`
}

type storagePtr struct {
	RootHash string `json:"root_hash"`
	Indexer  string `json:"indexer"`
	Size     int    `json:"size"`
}

// roleOf returns the role tag inside a JSON-wrapped dataDescription.
// Falls back to the raw string if the description doesn't parse as JSON
// (e.g. legacy bare-role entries written by an older uploader build).
func roleOf(desc string) string {
	var d struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal([]byte(desc), &d); err == nil && d.Role != "" {
		return d.Role
	}
	return desc
}

// Uploader bundles the deps + identity material a Push needs. Constructed
// once at startAgent time after bootstrap has resolved agentID + the chain
// client is dialed; per-dim Pushes reuse it.
type Uploader struct {
	adapter       *openclaw.Adapter
	agent         *state.Agent
	chain         *chain.Client
	tokenID       *big.Int
	agentSealPriv []byte
	agentSealPub  []byte // 33-byte compressed secp256k1 pubkey

	rpcURL        string // for storage submit tx (separate from chain RPC client because the SDK wants its own web3 instance)
	indexerURL    string
	agentSealHex  string // hex (no 0x prefix) of agent_seal_priv — what 0g-storage SDK expects
}

// New constructs an Uploader. Returns an error if agent_seal_priv can't be
// parsed (would block every Push otherwise).
func New(
	adapter *openclaw.Adapter,
	agent *state.Agent,
	chainClient *chain.Client,
	tokenID *big.Int,
	agentSealPriv []byte,
	rpcURL, indexerURL string,
) (*Uploader, error) {
	priv, err := crypto.ToECDSA(agentSealPriv)
	if err != nil {
		return nil, fmt.Errorf("parse agent_seal_priv: %w", err)
	}
	pub := crypto.CompressPubkey(&priv.PublicKey)
	return &Uploader{
		adapter:       adapter,
		agent:         agent,
		chain:         chainClient,
		tokenID:       tokenID,
		agentSealPriv: agentSealPriv,
		agentSealPub:  pub,
		rpcURL:        rpcURL,
		indexerURL:    indexerURL,
		agentSealHex:  hex.EncodeToString(agentSealPriv),
	}, nil
}

// Push uploads the current state of `dim` to chain. Steps annotated inline.
//
// Safe to call concurrently for different dims — each Push reads the full
// current chain state and constructs an independent newEntries array; the
// last tx to land wins (later Pushes see the earlier upload's effect via
// IntelligentDatasOf). For the same dim, concurrent Pushes are a no-op
// race (both produce ~identical new dataHash if disk hasn't changed).
func (u *Uploader) Push(ctx context.Context, dim string) error {
	// 1. Read current plaintext.
	plaintext, err := u.adapter.EvolutionFor(ctx, dim)
	if err != nil {
		return fmt.Errorf("EvolutionFor[%s]: %w", dim, err)
	}
	contentSum := sha256.Sum256(plaintext)
	contentHashHex := hex.EncodeToString(contentSum[:])

	// 2-3. Encrypt + upload.
	dataKey, err := dataplane.NewDataKey()
	if err != nil {
		return fmt.Errorf("new data_key: %w", err)
	}
	ciphertext, err := dataplane.Encrypt(plaintext, dataKey)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	root, err := dataplane.Upload(ctx, ciphertext, u.indexerURL, u.rpcURL, u.agentSealHex)
	if err != nil {
		return fmt.Errorf("storage upload: %w", err)
	}

	// 4. Wrap data_key to agent_seal_pub. Future restarts read sealedKeysOf,
	//    unwrap with the same priv, decrypt the ciphertext.
	sealedKey, err := dataplane.SealDataKey(dataKey, u.agentSealPub)
	if err != nil {
		return fmt.Errorf("seal data_key: %w", err)
	}

	// 5. Read current chain state for label-keyed merge.
	chainEntries, err := u.chain.IntelligentDatasOf(ctx, u.tokenID)
	if err != nil {
		return fmt.Errorf("read chain entries: %w", err)
	}
	chainSealedKeys, err := u.chain.SealedKeysOf(ctx, u.tokenID)
	if err != nil {
		return fmt.Errorf("read sealedKeysOf: %w", err)
	}

	// 6. Build newEntries / newSealedKeys: copy all chain entries except
	//    the one we're replacing, then append the changed dim. If the dim
	//    wasn't on chain (default mint case), append becomes an "add" —
	//    the contract's update accepts variable-length so this works
	//    uniformly without explicit ADD/UPDATE branching.
	newEntries := make([]chain.IntelligentData, 0, len(chainEntries)+1)
	newSealedKeys := make([][]byte, 0, len(chainEntries)+1)
	for _, e := range chainEntries {
		if roleOf(e.DataDescription) == dim {
			continue // skip, will append new version below
		}
		sk, ok := chainSealedKeys[e.DataHash]
		if !ok {
			return fmt.Errorf("chain inconsistency: dataHash=%x has no sealedKey", e.DataHash)
		}
		newEntries = append(newEntries, e)
		newSealedKeys = append(newSealedKeys, sk)
	}

	// Mirror attestor's JSON shape so sealed's bootstrap (which json.Unmarshals
	// dataDescription) and any indexer scraping the chain see a consistent
	// format across mint + every update.
	descJSON, err := json.Marshal(onChainDescription{
		Role: dim,
		StoragePtr: storagePtr{
			RootHash: "0x" + hex.EncodeToString(root[:]),
			Indexer:  u.indexerURL,
			Size:     len(ciphertext),
		},
		Encryption: "AES-GCM-256",
	})
	if err != nil {
		return fmt.Errorf("marshal dataDescription: %w", err)
	}
	newEntries = append(newEntries, chain.IntelligentData{
		DataDescription: string(descJSON),
		DataHash:        root,
	})
	newSealedKeys = append(newSealedKeys, sealedKey)

	// 7. Submit the update tx (signed by agent_seal_priv — contract authz
	//    requires this since the seal is bound).
	logger.Logf("uploader.Push[%s]: storage root=%x content=%s sealedKey=%dB; submitting update tx (%d entries)",
		dim, root, contentHashHex[:12]+"...", len(sealedKey), len(newEntries))
	txHash, err := u.chain.Update(ctx, u.tokenID, newEntries, newSealedKeys, u.agentSealPriv)
	if err != nil {
		return fmt.Errorf("chain.Update: %w", err)
	}

	// 8. Sync state — chainSnapshot now matches what's on chain.
	dataHashHex := "0x" + hex.EncodeToString(root[:])
	u.agent.RecordChainUpload(dim, contentHashHex, dataHashHex)
	logger.Logf("uploader.Push[%s]: complete, tx=%s dataHash=%s", dim, txHash.Hex(), dataHashHex)
	return nil
}

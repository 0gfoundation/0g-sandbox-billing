// Sealed container bootstrap (orchestrator).
//
// Phase 0  attest         - parse env, verify SANDBOX_SEAL_KEY ↔ attestation.pubkey,
//                           recover TEE signer (and match TEE_SIGNER_ADDRESS if set)
// Phase 1  provision      - POST /provision → ECIES-decrypt agent_seal_priv
// Phase 2  chain bootstrap - getAgentIdBySealId + intelligentDatasOf +
//                           loadSealedKeys + per-entry download + AES-GCM decrypt
// Phase 3  framework      - adapter.Restore each decrypted entry; adapter.Start
// Phase 4  status report  - notify attestor only on full pipeline success
//
// Long-running:
//   - HTTP server on :8080 (proxy package)
//   - manager monitors agent process exit and clears shared state
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"seal-verify/internal/chain"
	"seal-verify/internal/config"
	"seal-verify/internal/dataplane"
	"seal-verify/internal/framework"
	"seal-verify/internal/framework/openclaw"
	"seal-verify/internal/logger"
	"seal-verify/internal/manager"
	"seal-verify/internal/provision"
	"seal-verify/internal/proxy"
	"seal-verify/internal/report"
	"seal-verify/internal/state"
	"seal-verify/internal/watcher"
)

const bootstrapTimeout = 10 * time.Minute

// decryptedEntry mirrors the legacy bootstrap.go decryptedEntry, used to
// pass around an iData entry's plaintext + role tag while we still operate
// on a single role="config" entry. Phase 4 will move all dispatch into the
// framework adapter.
type decryptedEntry struct {
	Role      string
	DataHash  [32]byte
	Plaintext []byte
}

// storageDescription is the JSON wrapper inside dataDescription that points
// to the encrypted blob in 0g-storage and tags this entry's role.
type storageDescription struct {
	Role       string `json:"role"`
	StoragePtr struct {
		Indexer  string `json:"indexer"`
		RootHash string `json:"root_hash"`
	} `json:"storage_ptr"`
}

func main() {
	// Register adapters (side-effect of New()).
	openclawAdapter := openclaw.New()

	// Shared agent state — read by proxy, written by main + manager.
	agent := state.New()

	// Phase 0: parse env + verify attestation. Done BEFORE starting the HTTP
	// server because we need cfg.PublicURL to construct proxy.Server.
	cfg, err := config.Load()
	if err != nil {
		logger.Fail("%v", err)
	}

	// Start the HTTP server now (after we know our public URL but before the
	// rest of bootstrap so /healthz and /log are reachable while the chain
	// scan + agent spawn are still in flight).
	proxy.New(agent, cfg.PublicURL).Listen()
	if cfg.APIKey != "" {
		logger.Logf("API_KEY (from env): <set, %d chars>", len(cfg.APIKey))
	} else {
		logger.Logf("API_KEY (from env): <unset>")
	}
	logger.Logf("")

	// Phase 1+2+3: provision + bootstrap from chain + start agent.
	// Each phase is best-effort; if any fails we report error to attestor
	// and continue to serve /healthz, /log so operators can inspect.
	if cfg.AttestorURL == "" {
		logger.Logf("ATTESTOR_URL unset — skipping provision / bootstrap / status")
	} else if cfg.ChainRPC == "" || cfg.ContractAddr == "" || cfg.FallbackIndexer == "" {
		logger.Logf("missing required env (CHAIN_RPC_URL=%q AGENTIC_ID_ADDR=%q INDEXER_URL=%q) — skipping provision / bootstrap / status",
			cfg.ChainRPC, cfg.ContractAddr, cfg.FallbackIndexer)
	} else {
		runMainPipeline(cfg, agent, openclawAdapter)
	}

	logger.Logf("")
	logger.Logf("ALL DONE")
	logger.Flush()

	// Block forever — HTTP server runs in its own goroutine.
	select {}
}

// runMainPipeline encapsulates Phases 1-4. Errors are logged and reported
// but never crash the process (main keeps the HTTP server up so /log is
// reachable even when bootstrap can't complete).
func runMainPipeline(cfg *config.Bootstrap, agent *state.Agent, adapter *openclaw.Adapter) {
	logger.Logf("--- Provisioning from attestor: %s ---", cfg.AttestorURL)
	agentSealPriv := provision.FromAttestor(cfg.AttestorURL, cfg.SealKeyBytes, cfg.Attestation)
	if agentSealPriv == nil {
		return
	}
	// SANDBOX_SEAL_KEY is consumed; scrub before any agent process spawns.
	config.ScrubProvisioningSecrets(cfg.SealKeyBytes)

	logger.Logf("")
	logger.Logf("--- Bootstrap from AgenticID %s (rpc %s, fallback indexer %s) ---",
		cfg.ContractAddr, cfg.ChainRPC, cfg.FallbackIndexer)

	res, err := chainBootstrap(cfg.ChainRPC, cfg.ContractAddr, cfg.Attestation.SealID, agentSealPriv, cfg.FallbackIndexer)
	if err != nil {
		logger.Logf("FAIL bootstrap: %v", err)
		return
	}

	logger.Logf("")
	logger.Logf("--- Starting agent ---")
	// onFailed is invoked by the manager exactly once if the supervisor
	// exhausts restart retries. Reports an "error" status to the attestor
	// so the platform can decide whether to recreate the sandbox.
	onFailed := func(err error) {
		logger.Logf("FAIL supervisor: max retries exceeded: %v", err)
		report.Status(cfg.AttestorURL, agentSealPriv, cfg.Attestation.SealID, "error", "supervisor exhausted retries: "+err.Error())
	}
	if err := startAgent(adapter, agent, res, agentSealPriv, cfg.APIKey, cfg.PublicURL, cfg.Attestation.SealID, onFailed); err != nil {
		logger.Logf("FAIL agent: %v", err)
		report.Status(cfg.AttestorURL, agentSealPriv, cfg.Attestation.SealID, "error", err.Error())
		return
	}
	logger.Logf("OK   agent ready (upstream listening, agentState armed, supervisor active)")

	// Start the iData watcher: polls adapter.EvolutionFor for each dim every
	// 30s, computes sha256, updates state.currentSnapshot when drift is
	// detected. Drift is visible in the bootstrap log via state.UpdateCurrent's
	// own log line. No upload happens here — the evaluator (Phase 4) decides
	// when chain push is appropriate.
	go watcher.New(adapter, agent, watcher.Config{}).Run(context.Background())

	report.Status(cfg.AttestorURL, agentSealPriv, cfg.Attestation.SealID, "running", "")
}

// chainBootstrapResult bundles the outputs of Phase 2.
type chainBootstrapResult struct {
	entries []decryptedEntry
	owner   string
}

// chainBootstrap executes Phase 2 (mint observation, intelligentDatasOf,
// sealedKey scan, per-entry download + decrypt). Returns nil + error on
// any unrecoverable failure.
func chainBootstrap(rpcURL, contractHex, sealIDHex string, agentSealPriv []byte, fallbackIndexer string) (*chainBootstrapResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer cancel()

	c, err := chain.Dial(ctx, rpcURL, contractHex)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	sealID32, err := chain.HexSealID(sealIDHex)
	if err != nil {
		return nil, err
	}

	agentID, err := c.WaitForMint(ctx, sealID32)
	if err != nil {
		return nil, fmt.Errorf("wait for mint: %w", err)
	}
	logger.Logf("OK   minted agent_id: %s", agentID.String())

	iDatas, err := c.IntelligentDatasOf(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("intelligentDatasOf: %w", err)
	}
	logger.Logf("OK   intelligent_datas: %d entries", len(iDatas))

	sealedKeys, err := c.LoadSealedKeys(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("load sealedKeys: %w", err)
	}
	logger.Logf("OK   sealedKeys collected: %d entries", len(sealedKeys))

	entries := make([]decryptedEntry, 0, len(iDatas))
	allOK := true
	for i, d := range iDatas {
		sealed, ok := sealedKeys[d.DataHash]
		if !ok {
			logger.Logf("FAIL bootstrap[%d]: no sealedKey for dataHash 0x%s", i, hex.EncodeToString(d.DataHash[:]))
			allOK = false
			continue
		}
		entry, ok := decryptEntry(ctx, i, d, sealed, agentSealPriv, fallbackIndexer)
		if !ok {
			allOK = false
			continue
		}
		entries = append(entries, entry)
	}
	if !allOK {
		return nil, fmt.Errorf("one or more iData entries failed to decrypt")
	}

	owner, err := c.OwnerOf(ctx, agentID)
	ownerHex := ""
	if err != nil {
		logger.Logf("warn: ownerOf(%s) failed: %v", agentID.String(), err)
	} else {
		ownerHex = owner.Hex()
		logger.Logf("OK   agent owner: %s", ownerHex)
	}

	logger.Logf("OK   bootstrap complete")
	return &chainBootstrapResult{entries: entries, owner: ownerHex}, nil
}

// decryptEntry resolves dataDescription -> indexer + role, downloads the
// ciphertext, ECIES-unseals the data_key, AES-GCM decrypts the body, and
// removes the temp file before any agent process spawns.
func decryptEntry(ctx context.Context, idx int, d chain.IntelligentData, sealedKey, agentSealPriv []byte, fallbackIndexer string) (decryptedEntry, bool) {
	tag := fmt.Sprintf("[%d]", idx)
	dataHashHex := "0x" + hex.EncodeToString(d.DataHash[:])

	var desc storageDescription
	if err := json.Unmarshal([]byte(d.DataDescription), &desc); err != nil {
		logger.Logf("FAIL bootstrap%s parse dataDescription: %v", tag, err)
		return decryptedEntry{}, false
	}
	indexer := desc.StoragePtr.Indexer
	if indexer == "" {
		indexer = fallbackIndexer
	}
	if indexer == "" {
		logger.Logf("FAIL bootstrap%s no indexer (description.storage_ptr.indexer empty and no fallback)", tag)
		return decryptedEntry{}, false
	}
	logger.Logf("bootstrap%s data=%s role=%q indexer=%s", tag, dataHashHex, desc.Role, indexer)

	outPath := fmt.Sprintf("/tmp/idata-%s.bin", hex.EncodeToString(d.DataHash[:]))
	if err := dataplane.Download(ctx, dataHashHex, indexer, outPath); err != nil {
		logger.Logf("FAIL bootstrap%s download: %v", tag, err)
		return decryptedEntry{}, false
	}
	defer removeFile(outPath)

	blob, err := readFile(outPath)
	if err != nil {
		logger.Logf("FAIL bootstrap%s read downloaded file: %v", tag, err)
		return decryptedEntry{}, false
	}

	dataKey, err := dataplane.UnsealDataKey(sealedKey, agentSealPriv)
	if err != nil {
		logger.Logf("FAIL bootstrap%s ECIES decrypt sealedKey: %v", tag, err)
		return decryptedEntry{}, false
	}
	plaintext, err := dataplane.Decrypt(blob, dataKey)
	if err != nil {
		logger.Logf("FAIL bootstrap%s AES-GCM decrypt: %v", tag, err)
		return decryptedEntry{}, false
	}

	logger.Logf("OK   bootstrap%s decrypted (%d bytes, role=%q)", tag, len(plaintext), desc.Role)
	return decryptedEntry{Role: desc.Role, DataHash: d.DataHash, Plaintext: plaintext}, true
}

// startAgent dispatches each decrypted entry to the framework adapter, seeds
// per-dim iData snapshots into shared state, then hands off to the manager
// which spawns the process and supervises it.
//
// Currently each iData entry is routed by its role string. Long-term this
// becomes label-based dispatch (EVOLUTION_DESIGN section 4) but the
// snapshot-seeding pattern is already correct: every entry contributes one
// (dim, contentHash, dataHash) tuple to both chainSnapshot and currentSnapshot.
//
// The supervisor (manager) handles process death, liveness probes, restart
// backoff, and the Failed-phase escalation. onFailed fires once if max
// retries are exhausted.
func startAgent(
	adapter *openclaw.Adapter,
	agent *state.Agent,
	res *chainBootstrapResult,
	agentSealPriv []byte,
	apiKey, publicURL, sealID string,
	onFailed func(err error),
) error {
	var configEntry *decryptedEntry
	for i := range res.entries {
		// Dim mapping: legacy role == new dim label until Phase 5 attestor
		// switches to multi-dimension mint. For role="config" that's
		// dim="config".
		dim := res.entries[i].Role
		contentHash := sha256Hex(res.entries[i].Plaintext)
		dataHash := "0x" + hex.EncodeToString(res.entries[i].DataHash[:])
		agent.SeedSnapshots(dim, contentHash, dataHash)

		if res.entries[i].Role == "config" && configEntry == nil {
			configEntry = &res.entries[i]
		}
	}
	if configEntry == nil {
		return fmt.Errorf("no intelligent-data entry with role=\"config\"")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := adapter.Restore(ctx, "config", configEntry.Plaintext); err != nil {
		return err
	}
	cfg := adapter.Cfg()
	if cfg == nil {
		return fmt.Errorf("openclaw adapter returned nil cfg after Restore")
	}

	mgr := manager.New(adapter, agent, manager.Config{
		OnFailed: onFailed,
		// LivenessProbeInterval / BackoffSeq / MaxRetries / GracefulStopTimeout
		// take defaults — see manager.Config.applyDefaults.
	})
	return mgr.Start(context.Background(), manager.StartParams{
		Runtime: framework.RuntimeContext{
			APIKey:    apiKey,
			PublicURL: publicURL,
		},
		AgentSealPriv: agentSealPriv,
		SealID:        sealID,
		Owner:         res.owner,
		AgentConfig:   cfg,
	})
}

// sha256Hex computes hex-encoded sha256 over data. Used for iData content
// hashes recorded in the snapshot pair.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
func removeFile(path string)                { _ = os.Remove(path) }

// Package state holds the global agent state shared across modules:
// identity material (agent_seal_priv, sealID, owner), runtime metadata
// (upstreamURL, gateway authToken, agentConfig), and the iData snapshot
// pair used by serve-proof and the evolution flow.
//
// Two snapshots are tracked side-by-side per dimension:
//
//	chainSnapshot   — last state confirmed on chain. Updated only after
//	                  uploader receives a tx receipt.
//	currentSnapshot — agent's actual runtime state. Updated whenever a
//	                  watcher detects an in-memory state change.
//
// Bootstrap seeds both snapshots from the same chain entry so they start
// equal. Agent self-modification (e.g. dashboard upgrade) drifts current
// ahead of chain. The evaluator periodically diffs the two snapshots and
// decides when to push current → chain via the uploader, which then
// re-syncs chainSnapshot. serve-proof always signs the current snapshot
// so responses reflect the agent's truest state.
package state

import (
	"sort"
	"sync"

	"seal-verify/internal/logger"
)

// Phase reflects where in the bootstrap/run lifecycle the agent is.
type Phase int

const (
	PhaseBootstrapping Phase = iota
	PhaseRunning
	PhaseRestarting
	PhaseEvolving
	PhaseFailed
)

// AgentConfig is the abstract config envelope decrypted from the iData entry
// labelled "config". Framework-agnostic; the framework adapter unpacks the
// relevant subset (e.g. openclaw maps Inference -> agents.defaults.model).
type AgentConfig struct {
	Framework struct {
		Name           string `json:"name"`
		Version        string `json:"version"`
		PackageVersion string `json:"package_version"`
	} `json:"framework"`
	Inference struct {
		Provider  string   `json:"provider"`
		Model     string   `json:"model"`
		Fallbacks []string `json:"fallbacks"`
	} `json:"inference"`
	Persona struct {
		SystemPrompt string `json:"system_prompt"`
	} `json:"persona"`
	Skills []any `json:"skills"`
}

// DimEntry captures a single dimension's state in either snapshot.
//
//   - ContentHash is sha256 of the dimension's plaintext (in-memory canonical
//     bytes). Used by serve-proof and by the evaluator's diff.
//   - DataHash is the 0g-storage root hash on chain. Empty in current
//     snapshot until the dim has been uploaded; equals chain's storage root
//     in chain snapshot.
type DimEntry struct {
	ContentHash string // sha256 hex of plaintext
	DataHash    string // 0g-storage root hex (chain), "" if not yet uploaded
}

// Snapshot bundles the per-dim DimEntry map plus a sorted view used for
// serve-proof's data_hashes field.
type Snapshot struct {
	PerDim map[string]DimEntry
}

// Agent is the live shared state.
//
// Framework-specific credentials (e.g. openclaw control-UI token) are NOT
// stored here — they live inside the adapter and surface via the framework
// interface's AuthResponse method.
type Agent struct {
	mu            sync.RWMutex
	phase         Phase
	agentSealPriv []byte
	upstreamURL   string
	sealID        string
	owner         string
	cfg           *AgentConfig

	// Two snapshots; see package doc.
	chainSnapshot   Snapshot
	currentSnapshot Snapshot
}

// New constructs an Agent in PhaseBootstrapping.
func New() *Agent {
	return &Agent{
		phase:           PhaseBootstrapping,
		chainSnapshot:   Snapshot{PerDim: map[string]DimEntry{}},
		currentSnapshot: Snapshot{PerDim: map[string]DimEntry{}},
	}
}

// ── Identity / lifecycle accessors ──────────────────────────────────────────

// Snapshot returns a copy of the agent's current identity material plus the
// current sorted data hashes (for serve-proof). Callers cannot mutate the
// returned slice.
func (a *Agent) Snapshot() (priv []byte, upstream, sealID, owner string, dataHashes []string, cfg *AgentConfig) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.agentSealPriv, a.upstreamURL, a.sealID, a.owner, a.sortedCurrentLocked(), a.cfg
}

// Phase returns the current lifecycle phase.
func (a *Agent) Phase() Phase {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.phase
}

// SetPhase updates the lifecycle phase.
func (a *Agent) SetPhase(p Phase) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.phase = p
}

// Set arms identity + runtime fields (called by manager on start / restart).
// Snapshot data is NOT touched here — bootstrap seeds via SeedSnapshots and
// runtime watchers update via UpdateCurrent.
//
// Transitions phase to PhaseRunning.
func (a *Agent) Set(priv []byte, upstream, sealID, owner string, cfg *AgentConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.agentSealPriv = priv
	a.upstreamURL = upstream
	a.sealID = sealID
	a.owner = owner
	a.cfg = cfg
	a.phase = PhaseRunning
}

// Clear resets identity fields and snapshots. Used when the agent process
// exits and the proxy must stop accepting requests. Phase -> Bootstrapping.
func (a *Agent) Clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.agentSealPriv = nil
	a.upstreamURL = ""
	a.sealID = ""
	a.owner = ""
	a.cfg = nil
	a.chainSnapshot = Snapshot{PerDim: map[string]DimEntry{}}
	a.currentSnapshot = Snapshot{PerDim: map[string]DimEntry{}}
	a.phase = PhaseBootstrapping
}

// ── Snapshot management ─────────────────────────────────────────────────────

// SeedSnapshots initialises both chainSnapshot and currentSnapshot for a
// dimension. Called by bootstrap once per decrypted iData entry. Both
// snapshots start equal so currentDataHashes initially matches what's on
// chain.
//
// contentHash is sha256(plaintext). dataHash is the 0g-storage root hex
// from the iData entry on chain.
func (a *Agent) SeedSnapshots(dim, contentHash, dataHash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry := DimEntry{ContentHash: contentHash, DataHash: dataHash}
	a.chainSnapshot.PerDim[dim] = entry
	a.currentSnapshot.PerDim[dim] = entry
	logger.Logf("iData seed: dim=%s content=%s chain_root=%s",
		dim, shortHash(contentHash), shortHash(dataHash))
}

// UpdateCurrent advances the current snapshot for a dimension. Called by
// the watcher when adapter.EvolutionFor reveals a content hash that
// differs from currentSnapshot's last value.
//
// chain snapshot is intentionally NOT touched — only RecordChainUpload
// can move it forward, and only after a confirmed tx.
func (a *Agent) UpdateCurrent(dim, contentHash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev := a.currentSnapshot.PerDim[dim]
	if prev.ContentHash == contentHash {
		return // no change
	}
	a.currentSnapshot.PerDim[dim] = DimEntry{
		ContentHash: contentHash,
		// DataHash carries forward; it'll be replaced once this contentHash
		// gets uploaded and chain confirms a new storage root.
		DataHash: prev.DataHash,
	}
	logger.Logf("iData current changed: dim=%s content=%s -> %s (chain still at %s; awaiting evolution upload)",
		dim, shortHash(prev.ContentHash), shortHash(contentHash), shortHash(prev.DataHash))
}

// RecordChainUpload syncs the chain snapshot for a dimension. Called by the
// uploader after a sealUpdate tx receipt confirms.
//
// Updates BOTH snapshots: chainSnapshot reflects the new on-chain state,
// and currentSnapshot's DataHash is also bumped (current's ContentHash
// already matches what was uploaded; we just attach the new storage root).
func (a *Agent) RecordChainUpload(dim, contentHash, dataHash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev := a.chainSnapshot.PerDim[dim]
	a.chainSnapshot.PerDim[dim] = DimEntry{ContentHash: contentHash, DataHash: dataHash}
	if cur, ok := a.currentSnapshot.PerDim[dim]; ok && cur.ContentHash == contentHash {
		a.currentSnapshot.PerDim[dim] = DimEntry{ContentHash: contentHash, DataHash: dataHash}
	}
	logger.Logf("iData chain uploaded: dim=%s content=%s chain_root=%s -> %s",
		dim, shortHash(contentHash), shortHash(prev.DataHash), shortHash(dataHash))
}

// CurrentDataHashes returns the sorted hex content-hashes of the current
// snapshot, for serve-proof's data_hashes field. Hashes reflect agent's
// truest in-memory state at the moment of the call.
func (a *Agent) CurrentDataHashes() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sortedCurrentLocked()
}

// ChainDataHashes returns the sorted hex content-hashes that are confirmed
// on chain. Used by the evaluator's diff and for diagnostics.
func (a *Agent) ChainDataHashes() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.chainSnapshot.PerDim))
	for _, e := range a.chainSnapshot.PerDim {
		out = append(out, e.ContentHash)
	}
	sort.Strings(out)
	return out
}

// HasChanges reports whether currentSnapshot differs from chainSnapshot in
// any dimension. Used by the evaluator as a fast pre-check before any
// strategy evaluation.
func (a *Agent) HasChanges() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.currentSnapshot.PerDim) != len(a.chainSnapshot.PerDim) {
		return true
	}
	for dim, cur := range a.currentSnapshot.PerDim {
		ch, ok := a.chainSnapshot.PerDim[dim]
		if !ok || ch.ContentHash != cur.ContentHash {
			return true
		}
	}
	return false
}

// ── Internal helpers ────────────────────────────────────────────────────────

// sortedCurrentLocked must be called with a.mu held (read or write).
func (a *Agent) sortedCurrentLocked() []string {
	out := make([]string, 0, len(a.currentSnapshot.PerDim))
	for _, e := range a.currentSnapshot.PerDim {
		out = append(out, e.ContentHash)
	}
	sort.Strings(out)
	return out
}

// shortHash returns the first 10 chars of a hex string (with optional 0x
// prefix preserved) for log-friendly output. Empty input -> "(none)".
func shortHash(h string) string {
	if h == "" {
		return "(none)"
	}
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

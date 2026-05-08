// Package state holds the global agent state that multiple modules read from.
//
// In the current single-config implementation, this is just a thread-safe
// container for the post-bootstrap identity (agent_seal_priv, upstream URL,
// owner address, dataHashes, etc.) that the proxy and report modules
// consume. The State enum and event channel will grow as Phase 4 (evolution)
// and Phase 5 (manager-driven restarts) come online.
package state

import (
	"sort"
	"sync"
)

// Phase reflects where in the bootstrap/run lifecycle the agent is. Used
// indirectly via the readiness of agentSealPriv/upstreamURL today; will gate
// /hello, proxy serving, and reporter output once the state machine lands.
type Phase int

const (
	PhaseBootstrapping Phase = iota
	PhaseRunning
	PhaseRestarting
	PhaseEvolving
	PhaseFailed
)

// AgentConfig is the abstract config envelope decrypted from the iData entry
// labelled "config". It is framework-agnostic; the framework adapter unpacks
// the relevant subset (e.g. openclaw maps Inference -> agents.defaults.model).
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

// Agent is the live shared state between bootstrap, framework adapter, manager,
// proxy, and report modules. Read via Snapshot(), written via Set() / Clear().
type Agent struct {
	mu            sync.RWMutex
	phase         Phase
	agentSealPriv []byte
	upstreamURL   string
	sealID        string
	owner         string
	authToken     string
	dataHashes    []string
	cfg           *AgentConfig
}

// New constructs an Agent in PhaseBootstrapping with no identity loaded.
func New() *Agent {
	return &Agent{phase: PhaseBootstrapping}
}

// Snapshot returns a copy of the current agent state. Returned slices are
// copies; mutating them does not affect the underlying state.
func (a *Agent) Snapshot() (priv []byte, upstream, sealID, owner, token string, dataHashes []string, cfg *AgentConfig) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	dh := make([]string, len(a.dataHashes))
	copy(dh, a.dataHashes)
	return a.agentSealPriv, a.upstreamURL, a.sealID, a.owner, a.authToken, dh, a.cfg
}

// Phase returns the current lifecycle phase.
func (a *Agent) Phase() Phase {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.phase
}

// SetPhase updates the lifecycle phase only; identity fields are untouched.
func (a *Agent) SetPhase(p Phase) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.phase = p
}

// Set arms the agent with all post-bootstrap identity fields. dataHashes
// are stored sorted lex-ascending. Transitions phase to PhaseRunning.
func (a *Agent) Set(priv []byte, upstream, sealID, owner, token string, dataHashes []string, cfg *AgentConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.agentSealPriv = priv
	a.upstreamURL = upstream
	a.sealID = sealID
	a.owner = owner
	a.authToken = token
	dh := make([]string, len(dataHashes))
	copy(dh, dataHashes)
	sort.Strings(dh)
	a.dataHashes = dh
	a.cfg = cfg
	a.phase = PhaseRunning
}

// Clear resets all identity fields back to zero values. Transitions phase
// to PhaseBootstrapping. Used when the agent process exits and the proxy
// must stop accepting requests.
func (a *Agent) Clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.agentSealPriv = nil
	a.upstreamURL = ""
	a.sealID = ""
	a.owner = ""
	a.authToken = ""
	a.dataHashes = nil
	a.cfg = nil
	a.phase = PhaseBootstrapping
}

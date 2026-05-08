// Package manager owns the agent process lifecycle.
//
// Current minimal implementation: spawn once via Adapter.Start, watch for
// process exit, and clear shared state when it dies. Restart with backoff
// and crash-loop threshold are deferred to Phase 3.
package manager

import (
	"context"

	"seal-verify/internal/framework"
	"seal-verify/internal/logger"
	"seal-verify/internal/state"
)

// Adapter is the subset of framework.Framework the manager needs. The
// openclaw adapter additionally exposes MonitorExit to hook process exit;
// other adapters can satisfy this with a small wrapper.
type Adapter interface {
	framework.Framework
	MonitorExit(onExit func(err error))
}

// Manager wires an Adapter to the shared agent state.
type Manager struct {
	adapter Adapter
	agent   *state.Agent
}

// New constructs a Manager. The Adapter must already have its Restore calls
// completed (config in place) before Manager.Start runs.
func New(adapter Adapter, agent *state.Agent) *Manager {
	return &Manager{adapter: adapter, agent: agent}
}

// Start launches the agent process and arms shared state. On process exit
// (whether clean or crashed), Clear is called on shared state so the proxy
// stops accepting requests.
//
// Returns the start result (upstream URL + secret) so callers can pass them
// into state.Set with whatever extra context they have (sealID, owner,
// dataHashes, cfg).
func (m *Manager) Start(ctx context.Context, rt framework.RuntimeContext) (framework.StartResult, error) {
	res, err := m.adapter.Start(ctx, rt)
	if err != nil {
		return framework.StartResult{}, err
	}

	// Hook process exit so shared state gets cleared the moment openclaw dies.
	m.adapter.MonitorExit(func(_ error) {
		logger.Logf("manager: agent process exited; clearing agent state")
		m.agent.Clear()
	})

	return res, nil
}

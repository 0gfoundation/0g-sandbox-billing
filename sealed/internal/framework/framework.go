// Package framework defines the adapter contract that abstracts agent
// frameworks (openclaw, eliza, ...) behind a uniform interface used by the
// rest of the sealed bootstrap pipeline.
//
// See sealed/EVOLUTION_DESIGN.md section 7 for the full contract specification.
//
// In the current single-config implementation the only meaningful dimension
// is "config" (mapped from the existing iData role); knowledge / skills / ops
// are scaffolded for forward compatibility but not wired through yet.
package framework

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Framework is the adapter interface every agent framework must implement.
type Framework interface {
	// Name returns the static framework identifier, e.g. "openclaw".
	Name() string

	// Version returns the runtime-detected framework version (best-effort,
	// may exec a CLI to probe). Used in serve-proof metadata and reporter
	// status payloads.
	Version(ctx context.Context) (string, error)

	// Dimensions returns the labels this adapter understands, NOT including
	// the protocol-level "framework" entry. Order is informational only.
	Dimensions() []string

	// Restore applies the plaintext bytes for a single dimension to the
	// adapter's in-memory composed state. Multiple Restore calls must
	// commute and be idempotent (see EVOLUTION_DESIGN.md 7.2).
	Restore(ctx context.Context, dim string, plaintext []byte) error

	// Start spawns the agent process based on the previously-Restored state.
	// Returns the upstream URL the proxy should forward to, plus an opaque
	// secret (e.g. openclaw token) for the /_seal/auth flow.
	Start(ctx context.Context, rt RuntimeContext) (StartResult, error)

	// Stop gracefully terminates the agent process. SIGTERM-then-SIGKILL
	// pattern is acceptable; honour gracefulTimeout before escalating.
	Stop(ctx context.Context, gracefulTimeout time.Duration) error

	// Liveness reports whether the agent process is alive and listening.
	// Non-nil error means the manager should consider restarting.
	Liveness(ctx context.Context) error

	// Readiness reports whether the agent is ready to handle requests
	// (process up AND initialised). Non-nil error means /hello / proxy
	// should return 503 even though the process is alive.
	Readiness(ctx context.Context) error
}

// Reloadable is an optional interface adapters may implement to enable
// hot-reload semantics during evolution updates. Manager will prefer
// Reload over Stop+Start when available.
type Reloadable interface {
	Reload(ctx context.Context, changedDim string) error
}

// RuntimeContext is the per-Start environment passed to adapters. Owners of
// secrets (API keys etc.) populate it before calling Start.
type RuntimeContext struct {
	APIKey    string // inference provider API key from env (e.g. ANTHROPIC_API_KEY)
	PublicURL string // externally-reachable URL prefix for this sandbox; empty in local dev
}

// StartResult is what an adapter returns when its agent process is up and
// listening. Bootstrap arms state.Agent with these values.
type StartResult struct {
	Upstream string // e.g. "http://127.0.0.1:3284"
	Secret   string // framework-specific access credential (openclaw token)
	PID      int
}

// ── Registry ─────────────────────────────────────────────────────────────────
//
// Adapter packages register themselves via init() side-effect. Bootstrap
// resolves "openclaw" -> *openclawAdapter via Get().

var (
	registryMu sync.RWMutex
	registry   = map[string]Framework{}
)

// Register makes adapter retrievable by name. Adapters call this from their
// own init() function. A second registration for the same name overwrites
// (callers are expected to register exactly once at process start).
func Register(name string, fw Framework) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = fw
}

// Get retrieves a previously-registered adapter by name. Returns an error
// when no matching adapter is registered.
func Get(name string) (Framework, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	fw, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("framework not registered: %q", name)
	}
	return fw, nil
}

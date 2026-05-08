// Package watcher periodically polls the framework adapter for each
// dimension's current state, hashes the result, and updates
// state.currentSnapshot when drift is detected.
//
// This is the "agent → state" half of the evolution pipeline:
//
//	watcher (this) → state.UpdateCurrent → (proxy /hello picks up new hash)
//	                                        ↓
//	                                    evaluator decides upload
//	                                        ↓
//	                                    uploader → state.RecordChainUpload
//
// Watcher does NOT decide whether to upload — that's the evaluator's job.
// Watcher's only mutation is state.UpdateCurrent. Logging in state's
// UpdateCurrent makes drift visible without watcher needing to log.
package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"seal-verify/internal/framework"
	"seal-verify/internal/logger"
	"seal-verify/internal/state"
)

// Default polling interval. 30s is a balance between detection latency
// (typical evolution event = dashboard click → ~5-30s for filesystem
// changes to settle) and adapter call overhead (npm version probe + reads).
const DefaultInterval = 30 * time.Second

// Config tunes watcher behaviour. Zero-value is replaced with defaults.
type Config struct {
	// Interval between poll cycles. Default: 30s.
	Interval time.Duration
}

func (c *Config) applyDefaults() {
	if c.Interval == 0 {
		c.Interval = DefaultInterval
	}
}

// Watcher polls the framework adapter on a tick and feeds drift into state.
type Watcher struct {
	adapter framework.Framework
	agent   *state.Agent
	cfg     Config

	stopCh chan struct{}
	once   sync.Once
}

// New constructs a Watcher. cfg is normalized to defaults internally.
func New(adapter framework.Framework, agent *state.Agent, cfg Config) *Watcher {
	cfg.applyDefaults()
	return &Watcher{
		adapter: adapter,
		agent:   agent,
		cfg:     cfg,
		stopCh:  make(chan struct{}),
	}
}

// Run blocks until ctx is cancelled or Stop is called. Spawn it in a
// goroutine; main.go does this once after the agent is up.
func (w *Watcher) Run(ctx context.Context) {
	logger.Logf("watcher: started (interval=%s, dims=%v)", w.cfg.Interval, w.adapter.Dimensions())
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Logf("watcher: context cancelled, stopping")
			return
		case <-w.stopCh:
			logger.Logf("watcher: stop requested, stopping")
			return
		case <-ticker.C:
		}
		w.tick(ctx)
	}
}

// Stop signals Run to exit. Idempotent.
func (w *Watcher) Stop() {
	w.once.Do(func() { close(w.stopCh) })
}

// tick runs a single poll cycle: for each dim, ask adapter for current
// canonical bytes, sha256, push to state if drifted. Errors are logged
// per-dim and don't abort the cycle.
func (w *Watcher) tick(ctx context.Context) {
	for _, dim := range w.adapter.Dimensions() {
		bytes, err := w.adapter.EvolutionFor(ctx, dim)
		if err != nil {
			if errors.Is(err, framework.ErrUnsupportedDim) {
				continue // adapter says it doesn't track this dim
			}
			logger.Logf("watcher: EvolutionFor[%s] error: %v", dim, err)
			continue
		}
		hash := sha256Hex(bytes)
		w.agent.UpdateCurrent(dim, hash)
		// state.UpdateCurrent logs only when drift is detected; no log here
		// when nothing changed (avoids 30s/min spam in normal steady state).
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

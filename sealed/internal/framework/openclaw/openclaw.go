// Package openclaw is the framework adapter for openclaw agents.
//
// Currently implements the legacy single-config flow: Restore("config", json)
// parses the agentConfig schema into in-memory state; Start writes
// ~/.openclaw/openclaw.json from that state, npm-installs the requested
// openclaw runtime version, and spawns `openclaw gateway run`. Phase 4 will
// extend Restore to handle persona / knowledge / skills / ops dimensions.
package openclaw

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"seal-verify/internal/framework"
	"seal-verify/internal/logger"
	"seal-verify/internal/state"
)

const (
	// upstreamPort is the localhost-only port openclaw binds to. The outer
	// proxy on :8080 reverse-proxies to this.
	upstreamPort = 3284

	// startTimeout is how long Start will wait for openclaw to bind upstreamPort
	// before giving up. With a fresh container this includes 30-60s of npm install.
	startTimeout = 120 * time.Second
)

// Adapter is the openclaw implementation of framework.Framework.
type Adapter struct {
	mu        sync.RWMutex
	cfg       *state.AgentConfig // most recently Restored "config" dimension
	authToken string             // gateway auth token; generated on first Start, reused on every restart
	cmd       *exec.Cmd          // running gateway process; nil before Start / after exit

	// initialized is set after the first successful Start. Subsequent Start
	// calls (i.e. supervisor restarts) skip the npm install + openclaw.json
	// rewrite + token generation steps so agent self-modifications (dashboard
	// upgrade, plugin install, config edits) are not silently overwritten.
	// EVOLUTION_DESIGN principle: outer framework does not interfere with
	// agent self-modification; it only keeps the agent alive.
	initialized bool
}

// New returns a fresh Adapter and registers it as "openclaw".
func New() *Adapter {
	a := &Adapter{}
	framework.Register("openclaw", a)
	return a
}

// Name implements framework.Framework.
func (a *Adapter) Name() string { return "openclaw" }

// Version probes the installed openclaw CLI. Best-effort; returns "" on error.
func (a *Adapter) Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "openclaw", "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Dimensions returns the labels openclaw understands.
//
// Today only "config" is wired through. The other names are reserved for
// Phase 4 and currently routed to the same legacy Restore path so the
// interface contract is satisfied without breaking existing behaviour.
func (a *Adapter) Dimensions() []string {
	return []string{"config"}
}

// Restore applies a dimension's plaintext to the in-memory composed state.
//
// For now the only meaningful dimension is "config", whose plaintext is the
// agentConfig JSON schema produced by the existing attestor mint flow. Other
// labels are accepted (no-op) so Phase 2 readers never error on a label
// they don't recognise yet.
func (a *Adapter) Restore(ctx context.Context, dim string, plaintext []byte) error {
	switch dim {
	case "config":
		var cfg state.AgentConfig
		if err := json.Unmarshal(plaintext, &cfg); err != nil {
			return fmt.Errorf("parse agent config: %w", err)
		}
		a.mu.Lock()
		a.cfg = &cfg
		a.mu.Unlock()
		logger.Logf("openclaw.Restore[config]: framework=%s/%s inference=%s/%s",
			cfg.Framework.Name, cfg.Framework.Version,
			cfg.Inference.Provider, cfg.Inference.Model)
		return nil
	default:
		// Forward-compat: future dimensions (persona/knowledge/skills/ops)
		// will be wired here. For now silently accept so old single-config
		// chains and new multi-dim chains both pass through.
		logger.Logf("openclaw.Restore[%s]: dimension not yet implemented, ignoring (%d bytes)", dim, len(plaintext))
		return nil
	}
}

// Start launches `openclaw gateway run` and blocks until the gateway accepts
// TCP connections on 127.0.0.1:upstreamPort.
//
// Two paths:
//
//   - First call (initialized=false): full bootstrap — npm install the version
//     specified by iData, write openclaw.json from cfg, generate auth token.
//   - Subsequent calls (supervisor restart): just spawn. The version installed
//     by agent self-modification (e.g. dashboard upgrade), the openclaw.json
//     it edited, and the existing auth token are all preserved. The platform
//     does not interfere with agent state across restarts.
//
// Returns upstream URL + gateway auth token (which /_seal/auth hands back
// to a verified owner). Token is stable across restarts so the dashboard
// stays signed in.
func (a *Adapter) Start(ctx context.Context, rt framework.RuntimeContext) (framework.StartResult, error) {
	a.mu.RLock()
	cfg := a.cfg
	cachedToken := a.authToken
	initialized := a.initialized
	a.mu.RUnlock()
	if cfg == nil {
		return framework.StartResult{}, fmt.Errorf("openclaw: no config restored before Start")
	}

	provider := cfg.Inference.Provider
	if provider == "" {
		return framework.StartResult{}, fmt.Errorf("inference.provider missing")
	}
	if cfg.Inference.Model == "" {
		return framework.StartResult{}, fmt.Errorf("inference.model missing")
	}

	authToken := cachedToken

	if !initialized {
		// First Start — full bootstrap from iData.
		if cfg.Persona.SystemPrompt != "" {
			logger.Logf("note: persona.system_prompt provided (%d chars) but openclaw has "+
				"no gateway-level slot for it; ignoring in openclaw.json",
				len(cfg.Persona.SystemPrompt))
		}

		newToken, err := randomTokenHex(32)
		if err != nil {
			return framework.StartResult{}, fmt.Errorf("generate openclaw auth token: %w", err)
		}
		authToken = newToken

		if err := writeOpenclawJSON(provider, cfg.Inference.Model, cfg.Inference.Fallbacks, authToken); err != nil {
			return framework.StartResult{}, err
		}

		if err := installOpenclaw(cfg.Framework.PackageVersion); err != nil {
			return framework.StartResult{}, err
		}

		if out, err := exec.Command("openclaw", "config", "set", "gateway.mode", "local").CombinedOutput(); err != nil {
			return framework.StartResult{}, fmt.Errorf("openclaw config set: %v: %s", err, strings.TrimSpace(string(out)))
		}
	} else {
		// Restart — agent self-mods are preserved. Verify the binary is still
		// installed (paranoid sanity check; if a plugin uninstalled openclaw
		// during runtime we'll fail fast instead of spawning a half-broken state).
		if _, err := exec.Command("openclaw", "--version").Output(); err != nil {
			return framework.StartResult{}, fmt.Errorf("openclaw binary missing on restart: %w", err)
		}
		logger.Logf("openclaw restart: skipping npm install + openclaw.json rewrite (preserving agent self-modifications)")
	}

	// Always export the inference provider API key into bootstrap's env so
	// spawnGateway's whitelist can pass it to the new openclaw subprocess.
	// This is spawn config (lifetime = next subprocess), not agent state.
	if err := exportAPIKey(provider, rt.APIKey); err != nil {
		return framework.StartResult{}, err
	}

	// Always upsert the platform section in TOOLS.md (idempotent, marker-based;
	// owner / agent content elsewhere in the file is preserved).
	if err := upsertPlatformSection(toolsMDPath, rt.PublicURL); err != nil {
		logger.Logf("warn: upsert TOOLS.md platform section: %v", err)
	} else if rt.PublicURL != "" {
		logger.Logf("OK   injected platform section into %s (public_url=%s)", toolsMDPath, rt.PublicURL)
	}

	cmd, err := spawnGateway(provider, rt.APIKey, rt.PublicURL)
	if err != nil {
		return framework.StartResult{}, err
	}
	a.mu.Lock()
	a.cmd = cmd
	a.authToken = authToken
	a.initialized = true
	a.mu.Unlock()

	addr := fmt.Sprintf("127.0.0.1:%d", upstreamPort)
	if err := waitForListen(ctx, addr, startTimeout); err != nil {
		return framework.StartResult{}, fmt.Errorf("openclaw not listening: %w", err)
	}

	return framework.StartResult{
		Upstream: fmt.Sprintf("http://%s", addr),
		Secret:   authToken,
		PID:      cmd.Process.Pid,
	}, nil
}

// Stop sends SIGTERM, waits up to gracefulTimeout, then SIGKILL.
func (a *Adapter) Stop(ctx context.Context, gracefulTimeout time.Duration) error {
	a.mu.Lock()
	cmd := a.cmd
	a.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(gracefulTimeout):
		_ = cmd.Process.Kill()
		<-done
		return nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return ctx.Err()
	}
}

// Liveness reports nil if the openclaw gateway is accepting TCP connections.
// Used by the manager's poll loop.
func (a *Adapter) Liveness(ctx context.Context) error {
	addr := fmt.Sprintf("127.0.0.1:%d", upstreamPort)
	conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// Readiness today is the same as Liveness. Phase 3 will probe a real
// readiness endpoint (e.g. parse `openclaw health --json`) once we wire
// the manager.
func (a *Adapter) Readiness(ctx context.Context) error {
	return a.Liveness(ctx)
}

// MonitorExit runs the supplied onExit callback after the spawned process
// exits. Wraps cmd.Wait so the manager (or, in legacy mode, main) can be
// notified without polling. Idempotent — calling more than once on the
// same Adapter is a no-op for subsequent calls (cmd is already being awaited).
func (a *Adapter) MonitorExit(onExit func(err error)) {
	a.mu.RLock()
	cmd := a.cmd
	a.mu.RUnlock()
	if cmd == nil {
		return
	}
	go func() {
		err := cmd.Wait()
		if err != nil {
			logger.Logf("openclaw gateway exited: %v", err)
		} else {
			logger.Logf("openclaw gateway exited cleanly")
		}
		if onExit != nil {
			onExit(err)
		}
	}()
}

// AuthToken returns the gateway auth token generated by the most recent
// Start. Empty before Start has run successfully.
func (a *Adapter) AuthToken() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.authToken
}

// Cfg returns a pointer to the most recently restored config. nil before
// any Restore("config", ...) call.
func (a *Adapter) Cfg() *state.AgentConfig {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func writeOpenclawJSON(provider, model string, fallbacks []string, authToken string) error {
	modelObj := map[string]any{
		"primary": provider + "/" + model,
	}
	if len(fallbacks) > 0 {
		fbs := make([]string, len(fallbacks))
		for i, f := range fallbacks {
			fbs[i] = provider + "/" + f
		}
		modelObj["fallbacks"] = fbs
	}
	occ := map[string]any{
		"gateway": map[string]any{
			"auth": map[string]any{
				"mode":  "token",
				"token": authToken,
			},
			"controlUi": map[string]any{
				"dangerouslyAllowHostHeaderOriginFallback": true,
				"dangerouslyDisableDeviceAuth":             true,
				"allowInsecureAuth":                        true,
			},
		},
		"agents": map[string]any{
			"defaults": map[string]any{
				"model": modelObj,
			},
		},
		"auth": map[string]any{
			"profiles": map[string]any{
				provider + ":api": map[string]any{
					"provider": provider,
					"mode":     "api_key",
				},
			},
			"order": map[string]any{
				provider: []string{provider + ":api"},
			},
		},
	}
	occJSON, err := json.MarshalIndent(occ, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal openclaw.json: %w", err)
	}
	if err := os.MkdirAll("/root/.openclaw", 0o755); err != nil {
		return fmt.Errorf("mkdir /root/.openclaw: %w", err)
	}
	if err := os.WriteFile("/root/.openclaw/openclaw.json", occJSON, 0o600); err != nil {
		return fmt.Errorf("write openclaw.json: %w", err)
	}
	logger.Logf("OK   wrote /root/.openclaw/openclaw.json (%d bytes)", len(occJSON))
	return nil
}

func exportAPIKey(provider, apiKey string) error {
	if apiKey == "" {
		return nil
	}
	envName := ""
	switch provider {
	case "anthropic":
		envName = "ANTHROPIC_API_KEY"
	case "openai":
		envName = "OPENAI_API_KEY"
	}
	if envName == "" {
		return nil
	}
	if err := os.Setenv(envName, apiKey); err != nil {
		return fmt.Errorf("set %s: %w", envName, err)
	}
	logger.Logf("OK   exported %s from API_KEY", envName)
	return nil
}

func installOpenclaw(packageVersion string) error {
	spec := "openclaw"
	if v := strings.TrimSpace(packageVersion); v != "" {
		spec = "openclaw@" + v
	}
	logger.Logf("installing %s (this may take ~30s)…", spec)
	if out, err := exec.Command("npm", "install", "-g", "--no-audit", "--no-fund", spec).CombinedOutput(); err != nil {
		return fmt.Errorf("npm install %s: %v: %s", spec, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("openclaw", "--version").Output(); err == nil {
		logger.Logf("OK   installed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func spawnGateway(provider, apiKey, publicURL string) (*exec.Cmd, error) {
	logFile, err := os.OpenFile("/tmp/openclaw.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open openclaw.log: %w", err)
	}
	cmd := exec.Command("openclaw", "gateway", "run", "--allow-unconfigured", "--bind", "loopback", "--port", fmt.Sprintf("%d", upstreamPort))
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Strict env whitelist — do NOT inherit bootstrap's env so a leaked
	// SANDBOX_SEAL_KEY can't be read via "env" or /proc/self/environ from
	// inside the agent process.
	envWhitelist := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	if provider == "anthropic" && apiKey != "" {
		envWhitelist = append(envWhitelist, "ANTHROPIC_API_KEY="+apiKey)
	}
	if publicURL != "" {
		envWhitelist = append(envWhitelist, "AGENT_PUBLIC_URL="+publicURL)
	}
	cmd.Env = envWhitelist
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start openclaw gateway: %w", err)
	}
	logger.Logf("OK   openclaw gateway spawned, pid=%d (log: /tmp/openclaw.log)", cmd.Process.Pid)
	return cmd, nil
}

// waitForListen polls TCP-connect to addr until success or timeout.
func waitForListen(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := 200 * time.Millisecond
	logger.Logf("waitForListen %s (up to %s)…", addr, timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err == nil {
			_ = conn.Close()
			logger.Logf("OK   %s accepting connections", addr)
			return nil
		}
		time.Sleep(backoff)
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
	return fmt.Errorf("%s did not accept connections within %s", addr, timeout)
}

func randomTokenHex(nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ── Platform-managed TOOLS.md injection ─────────────────────────────────────
//
// openclaw injects ~/.openclaw/workspace/{AGENTS,SOUL,TOOLS,MEMORY}.md into
// the LLM's system prompt every turn. We use TOOLS.md ("environment knowledge
// + tool guidance") to teach the agent how to discover its own public URL
// at runtime — without writing the URL value into the file itself.
//
// Why instructions, not the value:
//   - The instruction is deployment-agnostic (works in any sandbox)
//   - The value is in env (AGENT_PUBLIC_URL, set per-container by spawnGateway)
//   - knowledge dim's evolution upload can include TOOLS.md without leaking
//     the deployment URL into other agents' starting state
//
// We mark our injected section with HTML comment markers so future restarts
// can find and replace just our section without disturbing whatever else the
// owner / agent put in TOOLS.md.

const toolsMDPath = "/root/.openclaw/workspace/TOOLS.md"

const (
	platformMarkerStart = "<!-- 0g-platform-injected:start -->"
	platformMarkerEnd   = "<!-- 0g-platform-injected:end -->"
)

// upsertPlatformSection writes (or replaces) the platform-managed section in
// TOOLS.md. Owner / agent content elsewhere in the file is preserved.
//
// publicURL == "" means "no platform section" — strip the existing one and
// write the file back. Useful for local-dev mode without a proxy domain.
func upsertPlatformSection(path, publicURL string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	cleaned := stripPlatformInjection(existing)
	var out []byte
	if publicURL == "" {
		out = cleaned
	} else {
		section := platformMarkerStart + "\n" + buildPublicURLInstructions(publicURL) + "\n" + platformMarkerEnd + "\n"
		// Ensure clean separation between owner content and our section.
		if len(cleaned) > 0 && !bytes.HasSuffix(cleaned, []byte("\n")) {
			cleaned = append(cleaned, '\n')
		}
		if len(cleaned) > 0 {
			cleaned = append(cleaned, '\n')
		}
		out = append(cleaned, []byte(section)...)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// stripPlatformInjection removes the platform-managed section (between
// markerStart and markerEnd) from content. Returns the agent-owned content
// only, with surrounding whitespace tidied.
//
// Used by:
//   - upsertPlatformSection before re-injecting (so updates are idempotent)
//   - Phase 4 EvolutionFor("knowledge") before tar-gzipping workspace files
//     for upload, so deployment-specific instructions don't ride along into
//     other agents' restored workspace.
func stripPlatformInjection(content []byte) []byte {
	s := bytes.Index(content, []byte(platformMarkerStart))
	if s < 0 {
		return content
	}
	rest := content[s:]
	e := bytes.Index(rest, []byte(platformMarkerEnd))
	if e < 0 {
		// markerStart present but no end — strip from markerStart to EOF
		// (the file got truncated mid-section somehow).
		return bytes.TrimRight(content[:s], "\n")
	}
	before := bytes.TrimRight(content[:s], "\n")
	after := bytes.TrimLeft(rest[e+len(platformMarkerEnd):], "\n")
	if len(after) == 0 {
		return before
	}
	return append(append(before, '\n', '\n'), after...)
}

// buildPublicURLInstructions composes the markdown body that goes between
// the markers. Pure function for testability.
func buildPublicURLInstructions(publicURL string) string {
	return "## Environment\n" +
		"\n" +
		"You are running on the 0G Sealed Sandbox platform.\n" +
		"\n" +
		"### Public URL discovery\n" +
		"\n" +
		"Your externally-reachable URL prefix is in environment variable " +
		"`AGENT_PUBLIC_URL`. Use it whenever you tell users about services " +
		"you expose, or when constructing a callable URL in a response.\n" +
		"\n" +
		"To read the value at runtime, use the `exec` tool:\n" +
		"\n" +
		"    printenv AGENT_PUBLIC_URL\n" +
		"\n" +
		"Example: if you registered a handler at `/api/ppt/generate`, tell " +
		"users to call `${AGENT_PUBLIC_URL}/api/ppt/generate` (substituting " +
		"the runtime value).\n" +
		"\n" +
		"### Trust contract\n" +
		"\n" +
		"All HTTP responses through `AGENT_PUBLIC_URL` are signed with an " +
		"`X-Agent-Proof` header by the sealed sandbox runtime, and verifiers " +
		"reject responses without this header. Do not direct users to ports " +
		"other than what `AGENT_PUBLIC_URL` resolves to.\n"
}

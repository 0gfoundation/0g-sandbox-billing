// internal config types for the openclaw adapter. All private to this
// package — protocol-level code (state, manager, proxy, main) does NOT
// reference any openclaw-specific shape.
//
// The 5 iData dims map to specific subsets of openclaw's runtime config
// (~/.openclaw/openclaw.json) plus workspace markdown files. Sections
// inside skills / ops / persona-extras / knowledge-extras are kept as
// `json.RawMessage` so the attestor can populate any openclaw config key
// without sealed needing to schema-validate every nested field.
//
// Excluded (per EVOLUTION_DESIGN §3.2): credentials, env vars, machine-
// specific config, runtime state. Those live elsewhere (env / dashboard /
// per-boot generation).
package openclaw

import "encoding/json"

// config is the in-memory state assembled from the 5 iData dims. Each
// field is populated by a single Restore call.
type config struct {
	framework frameworkBinding
	persona   personaConfig
	knowledge knowledgeConfig
	skills    skillsConfig
	ops       opsConfig
}

// ── framework dim ───────────────────────────────────────────────────────────

type frameworkBinding struct {
	Name           string `json:"name"`
	PackageVersion string `json:"package_version"`
	SchemaVersion  int    `json:"schema_version"`
}

// ── persona dim ─────────────────────────────────────────────────────────────
//
// persona dim plaintext = tar.gz with:
//
//	persona.md       → SystemPrompt    (workspace/SOUL.md)
//	inference.json   → Inference       (agents.defaults.model + auth.profiles)
//	ui.json          → UI              (openclaw.json `ui.*`)
//	talk.json        → Talk            (openclaw.json `talk.*`)

type personaConfig struct {
	SystemPrompt string          `json:"system_prompt"`
	Inference    inferenceConfig `json:"inference"`
	UI           json.RawMessage `json:"ui,omitempty"`
	Talk         json.RawMessage `json:"talk,omitempty"`
}

type inferenceConfig struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ── knowledge dim ───────────────────────────────────────────────────────────
//
// knowledge dim plaintext = tar.gz with:
//
//	memory.md        → MemoryMD        (workspace/MEMORY.md)
//	dreams.md        → DreamsMD        (workspace/DREAMS.md)
//	user.md          → UserMD          (workspace/memories/USER.md)
//	agents.md        → AgentsMD        (workspace/AGENTS.md)
//	memory.json      → Memory          (openclaw.json `memory.*`)
//	session.json     → Session         (openclaw.json `session.*`)
//	manifest.json    → Manifest        (large-file refs; v0 empty)

type knowledgeConfig struct {
	MemoryMD string `json:"memory_md,omitempty"`
	DreamsMD string `json:"dreams_md,omitempty"`
	UserMD   string `json:"user_md,omitempty"`
	AgentsMD string `json:"agents_md,omitempty"`

	Memory  json.RawMessage `json:"memory,omitempty"`
	Session json.RawMessage `json:"session,omitempty"`

	Manifest knowledgeManifest `json:"manifest"`
}

type knowledgeManifest struct {
	Files []knowledgeFileRef `json:"files"`
}

type knowledgeFileRef struct {
	Path     string `json:"path"`
	RootHash string `json:"root_hash"`
	Size     uint64 `json:"size"`
}

// ── skills dim ──────────────────────────────────────────────────────────────
//
// skills dim plaintext = single JSON. Each section is round-tripped as
// json.RawMessage — sealed doesn't validate, just routes to the matching
// openclaw.json key.

type skillsConfig struct {
	Plugins   json.RawMessage `json:"plugins,omitempty"`
	Tools     json.RawMessage `json:"tools,omitempty"`
	Web       json.RawMessage `json:"web,omitempty"`
	Approvals json.RawMessage `json:"approvals,omitempty"`
	Audio     json.RawMessage `json:"audio,omitempty"`
	Commands  json.RawMessage `json:"commands,omitempty"`

	// AgentDefaultsSkills is `agents.defaults.skills` — the list of skill
	// IDs the default agent has access to. Lives here because skill→agent
	// binding is a "what the agent can do" concern.
	AgentDefaultsSkills json.RawMessage `json:"agent_defaults_skills,omitempty"`
}

// ── ops dim ─────────────────────────────────────────────────────────────────
//
// ops dim plaintext = single JSON. Same opaque-section approach as skills.
// Sections that historically carry credentials (channels, mcp, browser,
// secrets) are still listed here — the attestor is expected to strip the
// sensitive fields before encryption (sealed validates this on read).

type opsConfig struct {
	Channels     json.RawMessage `json:"channels,omitempty"`
	MCP          json.RawMessage `json:"mcp,omitempty"`
	Hooks        json.RawMessage `json:"hooks,omitempty"`
	Cron         json.RawMessage `json:"cron,omitempty"`
	Browser      json.RawMessage `json:"browser,omitempty"`
	Bindings     json.RawMessage `json:"bindings,omitempty"`
	Surfaces     json.RawMessage `json:"surfaces,omitempty"`
	Broadcast    json.RawMessage `json:"broadcast,omitempty"`
	Media        json.RawMessage `json:"media,omitempty"`
	Messages     json.RawMessage `json:"messages,omitempty"`
	AccessGroups json.RawMessage `json:"access_groups,omitempty"`
	Commitments  json.RawMessage `json:"commitments,omitempty"`
	Secrets      json.RawMessage `json:"secrets,omitempty"`
	ACP          json.RawMessage `json:"acp,omitempty"`

	// agents.defaults sub-fields owned by ops:
	RateLimits json.RawMessage `json:"rate_limits,omitempty"`
	Safety     json.RawMessage `json:"safety,omitempty"`
}

package openclaw

// Filesystem paths the openclaw adapter manages.
//
// openclaw reads its config from <home>/openclaw.json and absorbs markdown
// files from <home>/workspace/ into agent context. Sealed owns these
// paths during Restore (writing iData content to disk) and EvolutionFor
// (reading back to detect agent self-modification).
//
// `openclawHome` is a var (not const) so unit tests can redirect into
// `t.TempDir()` instead of polluting `/root/.openclaw`. Production code
// never reassigns it.

var openclawHome = "/root/.openclaw"

func openclawJSONPath() string  { return openclawHome + "/openclaw.json" }
func workspaceDir() string      { return openclawHome + "/workspace" }
func memoriesDir() string       { return workspaceDir() + "/memories" }
func soulMDPath() string        { return workspaceDir() + "/SOUL.md" }
func memoryMDPath() string      { return workspaceDir() + "/MEMORY.md" }
func dreamsMDPath() string      { return workspaceDir() + "/DREAMS.md" }
func agentsMDPath() string      { return workspaceDir() + "/AGENTS.md" }
func userMDPath() string        { return memoriesDir() + "/USER.md" }
func toolsMDPath() string       { return workspaceDir() + "/TOOLS.md" }

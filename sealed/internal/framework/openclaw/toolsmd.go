package openclaw

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// Platform-managed TOOLS.md injection.
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
//     the deployment URL into other agents' restored workspace
//
// We mark the injected section with HTML comment markers so future restarts
// can find and replace just our section without disturbing whatever else
// the owner / agent put in TOOLS.md.

const (
	platformMarkerStart = "<!-- 0g-platform-injected:start -->"
	platformMarkerEnd   = "<!-- 0g-platform-injected:end -->"
)

// upsertPlatformSection writes (or replaces) the platform-managed section
// in TOOLS.md. Owner / agent content elsewhere in the file is preserved.
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
//   - knowledge dim's EvolutionFor before tar-gzipping workspace files for
//     upload, so deployment-specific instructions don't ride along into
//     other agents' restored workspace
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

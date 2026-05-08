package openclaw

import (
	"os"
	"strings"
	"testing"
)

func TestStripPlatformInjection_NoMarker(t *testing.T) {
	in := []byte("# TOOLS\n\nOwner content here.\n")
	out := stripPlatformInjection(in)
	if string(out) != string(in) {
		t.Errorf("expected unchanged, got: %q", string(out))
	}
}

func TestStripPlatformInjection_FullSection(t *testing.T) {
	in := []byte("# TOOLS\n\nOwner content.\n\n" +
		platformMarkerStart + "\n" +
		"## Environment\n" +
		"injected stuff\n" +
		platformMarkerEnd + "\n")
	out := stripPlatformInjection(in)
	want := "# TOOLS\n\nOwner content."
	if string(out) != want {
		t.Errorf("strip mismatch\n want: %q\n  got: %q", want, string(out))
	}
}

func TestStripPlatformInjection_SectionWithFollowing(t *testing.T) {
	in := []byte("# TOOLS\n\n" +
		platformMarkerStart + "\n" +
		"injected\n" +
		platformMarkerEnd + "\n\n" +
		"## Owner section after\n" +
		"more owner content\n")
	out := stripPlatformInjection(in)
	want := "# TOOLS\n\n## Owner section after\nmore owner content\n"
	if string(out) != want {
		t.Errorf("strip mismatch\n want: %q\n  got: %q", want, string(out))
	}
}

func TestStripPlatformInjection_MissingEndMarker(t *testing.T) {
	in := []byte("# TOOLS\n\nOwner content.\n\n" +
		platformMarkerStart + "\n" +
		"injected stuff with no end\n")
	out := stripPlatformInjection(in)
	want := "# TOOLS\n\nOwner content."
	if string(out) != want {
		t.Errorf("truncated strip mismatch\n want: %q\n  got: %q", want, string(out))
	}
}

func TestUpsertPlatformSection_FreshFile(t *testing.T) {
	tmp := t.TempDir() + "/TOOLS.md"
	if err := upsertPlatformSection(tmp, "http://8080-x.example.com:4000"); err != nil {
		t.Fatalf("upsert err: %v", err)
	}
	body := mustRead(t, tmp)
	if !strings.Contains(body, platformMarkerStart) || !strings.Contains(body, platformMarkerEnd) {
		t.Errorf("missing markers: %q", body)
	}
	if !strings.Contains(body, "AGENT_PUBLIC_URL") {
		t.Errorf("missing AGENT_PUBLIC_URL mention: %q", body)
	}
	if !strings.Contains(body, "X-Agent-Proof") {
		t.Errorf("missing trust contract: %q", body)
	}
}

func TestUpsertPlatformSection_PreservesOwnerContent(t *testing.T) {
	tmp := t.TempDir() + "/TOOLS.md"
	owner := "# TOOLS\n\nOwner-defined tool guidance.\n"
	if err := os.WriteFile(tmp, []byte(owner), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := upsertPlatformSection(tmp, "http://x.example.com"); err != nil {
		t.Fatalf("upsert err: %v", err)
	}
	body := mustRead(t, tmp)
	if !strings.Contains(body, "Owner-defined tool guidance.") {
		t.Errorf("owner content lost: %q", body)
	}
	if !strings.Contains(body, "AGENT_PUBLIC_URL") {
		t.Errorf("platform section missing: %q", body)
	}
}

func TestUpsertPlatformSection_Idempotent(t *testing.T) {
	tmp := t.TempDir() + "/TOOLS.md"
	owner := "# TOOLS\n\nOwner content.\n"
	if err := os.WriteFile(tmp, []byte(owner), 0o644); err != nil {
		t.Fatal(err)
	}
	url := "http://8080-test.example.com"
	for i := 0; i < 3; i++ {
		if err := upsertPlatformSection(tmp, url); err != nil {
			t.Fatalf("upsert iter %d: %v", i, err)
		}
	}
	body := mustRead(t, tmp)
	if c := strings.Count(body, platformMarkerStart); c != 1 {
		t.Errorf("expected 1 markerStart, got %d in %q", c, body)
	}
	if c := strings.Count(body, platformMarkerEnd); c != 1 {
		t.Errorf("expected 1 markerEnd, got %d in %q", c, body)
	}
}

func TestUpsertPlatformSection_EmptyURLStripsSection(t *testing.T) {
	tmp := t.TempDir() + "/TOOLS.md"
	if err := upsertPlatformSection(tmp, "http://x.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := upsertPlatformSection(tmp, ""); err != nil {
		t.Fatalf("upsert with empty url: %v", err)
	}
	body := mustRead(t, tmp)
	if strings.Contains(body, platformMarkerStart) || strings.Contains(body, "AGENT_PUBLIC_URL") {
		t.Errorf("expected platform section stripped, got: %q", body)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

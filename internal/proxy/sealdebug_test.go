//go:build !sealdebug

package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/0gfoundation/0g-sandbox/internal/daytona"
)

// These tests verify that sealed sandboxes block SSH and toolbox access in
// production builds. They are excluded from sealdebug builds where this
// restriction is intentionally relaxed.

func TestSealedSandbox_SSHBlocked(t *testing.T) {
	sealedSB := daytona.Sandbox{
		ID:     "sb-sealed",
		Labels: map[string]string{ownerLabel: "0xOWNER", sealedLabel: "true"},
	}
	srv := mockDaytonaWithSSH(t, []daytona.Sandbox{sealedSB})
	dtona := daytona.NewClient(srv.URL, "key")
	r := newTestEngine(dtona, &mockBilling{}, "0xOWNER")

	req := httptest.NewRequest(http.MethodPost, "/api/sandbox/sb-sealed/ssh-access", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("sealed sandbox SSH: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSealedSandbox_ToolboxBlocked(t *testing.T) {
	sealedSB := daytona.Sandbox{
		ID:     "sb-sealed",
		Labels: map[string]string{ownerLabel: "0xOWNER", sealedLabel: "true"},
	}
	srv := mockDaytonaWithSSH(t, []daytona.Sandbox{sealedSB})
	dtona := daytona.NewClient(srv.URL, "key")
	r := newTestEngine(dtona, &mockBilling{}, "0xOWNER")

	req := httptest.NewRequest(http.MethodPost, "/api/toolbox/sb-sealed/process/execute", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("sealed sandbox toolbox: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

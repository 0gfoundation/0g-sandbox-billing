package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestStringField(t *testing.T) {
	t.Parallel()

	got, ok := stringField(map[string]any{"id": "sb-1"}, "id")
	if !ok || got != "sb-1" {
		t.Fatalf("stringField() = %q, %v; want sb-1, true", got, ok)
	}

	if _, ok := stringField(map[string]any{"id": 123}, "id"); ok {
		t.Fatal("stringField() accepted non-string value")
	}

	if _, ok := stringField(map[string]any{"id": ""}, "id"); ok {
		t.Fatal("stringField() accepted empty string")
	}
}

func TestWaitForSandboxStartedPollsUntilStarted(t *testing.T) {
	oldPoll := sandboxWaitPollInterval
	sandboxWaitPollInterval = time.Millisecond
	defer func() { sandboxWaitPollInterval = oldPoll }()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s; want GET", r.Method)
		}
		if r.URL.Path != "/api/sandbox/sb-1" {
			t.Errorf("path = %s; want /api/sandbox/sb-1", r.URL.Path)
		}
		for _, header := range []string{"X-Wallet-Address", "X-Signed-Message", "X-Wallet-Signature"} {
			if r.Header.Get(header) == "" {
				t.Errorf("missing header %s", header)
			}
		}

		state := "creating"
		if atomic.AddInt32(&calls, 1) >= 2 {
			state = "started"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "sb-1",
			"state": state,
		})
	}))
	defer srv.Close()

	got := waitForSandboxStarted(key, srv.URL, "sb-1", time.Second)
	if state, _ := stringField(got, "state"); state != "started" {
		t.Fatalf("state = %q; want started", state)
	}
	if calls != 2 {
		t.Fatalf("calls = %d; want 2", calls)
	}
}

func TestDownloadedFileContent(t *testing.T) {
	t.Parallel()

	encoded := base64.StdEncoding.EncodeToString([]byte("hello\n"))
	got, err := downloadedFileContent([]byte(`{"content":"` + encoded + `"}`))
	if err != nil {
		t.Fatalf("downloadedFileContent base64: %v", err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("base64 content = %q; want hello newline", got)
	}

	got, err = downloadedFileContent([]byte(`{"content":"plain text","encoding":"utf-8"}`))
	if err != nil {
		t.Fatalf("downloadedFileContent text: %v", err)
	}
	if string(got) != "plain text" {
		t.Fatalf("text content = %q; want plain text", got)
	}

	got, err = downloadedFileContent([]byte("raw bytes"))
	if err != nil {
		t.Fatalf("downloadedFileContent raw: %v", err)
	}
	if string(got) != "raw bytes" {
		t.Fatalf("raw content = %q; want raw bytes", got)
	}
}

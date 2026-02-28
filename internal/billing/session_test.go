package billing

import (
	"context"
	"sort"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return rdb, mr
}

var testSession = Session{
	SandboxID:     "sb-test-001",
	Owner:         "0xABCDEF1234567890ABCDEF1234567890ABCDEF12",
	Provider:      "0x1111111111111111111111111111111111111111",
	StartTime:     1_700_000_000,
	LastVoucherAt: 1_700_000_000,
}

// ── CreateSession / GetSession ───────────────────────────────────────────────

func TestCreateSession_GetSession(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ctx := context.Background()

	if err := CreateSession(ctx, rdb, testSession); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := GetSession(ctx, rdb, testSession.SandboxID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got == nil {
		t.Fatal("expected session, got nil")
	}

	if got.SandboxID != testSession.SandboxID {
		t.Errorf("SandboxID: got %q want %q", got.SandboxID, testSession.SandboxID)
	}
	if got.Owner != testSession.Owner {
		t.Errorf("Owner: got %q want %q", got.Owner, testSession.Owner)
	}
	if got.Provider != testSession.Provider {
		t.Errorf("Provider: got %q want %q", got.Provider, testSession.Provider)
	}
	if got.StartTime != testSession.StartTime {
		t.Errorf("StartTime: got %d want %d", got.StartTime, testSession.StartTime)
	}
	if got.LastVoucherAt != testSession.LastVoucherAt {
		t.Errorf("LastVoucherAt: got %d want %d", got.LastVoucherAt, testSession.LastVoucherAt)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ctx := context.Background()

	got, err := GetSession(ctx, rdb, "sb-missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// ── UpdateLastVoucherAt ───────────────────────────────────────────────────────

func TestUpdateLastVoucherAt(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ctx := context.Background()

	if err := CreateSession(ctx, rdb, testSession); err != nil {
		t.Fatal(err)
	}

	newTs := int64(1_700_003_600)
	if err := UpdateLastVoucherAt(ctx, rdb, testSession.SandboxID, newTs); err != nil {
		t.Fatalf("UpdateLastVoucherAt: %v", err)
	}

	got, _ := GetSession(ctx, rdb, testSession.SandboxID)
	if got.LastVoucherAt != newTs {
		t.Errorf("LastVoucherAt: got %d want %d", got.LastVoucherAt, newTs)
	}
	// Other fields must be unchanged
	if got.Owner != testSession.Owner {
		t.Errorf("Owner changed unexpectedly: %q", got.Owner)
	}
	if got.StartTime != testSession.StartTime {
		t.Errorf("StartTime changed unexpectedly: %d", got.StartTime)
	}
}

// ── DeleteSession ─────────────────────────────────────────────────────────────

func TestDeleteSession(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ctx := context.Background()

	CreateSession(ctx, rdb, testSession) //nolint:errcheck

	if err := DeleteSession(ctx, rdb, testSession.SandboxID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	got, err := GetSession(ctx, rdb, testSession.SandboxID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil after delete")
	}
}

func TestDeleteSession_Idempotent(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ctx := context.Background()

	// Deleting a non-existent session should not error
	if err := DeleteSession(ctx, rdb, "sb-nonexistent"); err != nil {
		t.Fatalf("DeleteSession on missing key: %v", err)
	}
}

// ── ScanAllSessions ───────────────────────────────────────────────────────────

func TestScanAllSessions_Empty(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ctx := context.Background()

	sessions, err := ScanAllSessions(ctx, rdb)
	if err != nil {
		t.Fatalf("ScanAllSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestScanAllSessions_Multiple(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ctx := context.Background()

	sess := []Session{
		{SandboxID: "sb-001", Owner: "0xAAAA", Provider: "0xPPPP", StartTime: 1000, LastVoucherAt: 1000},
		{SandboxID: "sb-002", Owner: "0xBBBB", Provider: "0xPPPP", StartTime: 2000, LastVoucherAt: 2000},
		{SandboxID: "sb-003", Owner: "0xCCCC", Provider: "0xPPPP", StartTime: 3000, LastVoucherAt: 3000},
	}
	for _, s := range sess {
		CreateSession(ctx, rdb, s) //nolint:errcheck
	}

	got, err := ScanAllSessions(ctx, rdb)
	if err != nil {
		t.Fatalf("ScanAllSessions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(got))
	}

	// Sort both slices by SandboxID for stable comparison
	sort.Slice(got, func(i, j int) bool { return got[i].SandboxID < got[j].SandboxID })
	sort.Slice(sess, func(i, j int) bool { return sess[i].SandboxID < sess[j].SandboxID })

	for i := range sess {
		if got[i].SandboxID != sess[i].SandboxID {
			t.Errorf("[%d] SandboxID: got %q want %q", i, got[i].SandboxID, sess[i].SandboxID)
		}
		if got[i].Owner != sess[i].Owner {
			t.Errorf("[%d] Owner: got %q want %q", i, got[i].Owner, sess[i].Owner)
		}
		if got[i].StartTime != sess[i].StartTime {
			t.Errorf("[%d] StartTime: got %d want %d", i, got[i].StartTime, sess[i].StartTime)
		}
	}
}

func TestScanAllSessions_IgnoresUnrelatedKeys(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ctx := context.Background()

	// Write unrelated keys
	rdb.Set(ctx, "nonce:abc", 1, 0)             //nolint:errcheck
	rdb.Set(ctx, "stop:sandbox:sb-x", "reason", 0) //nolint:errcheck
	rdb.HSet(ctx, "voucher:queue:0xPROV", "field", "val") //nolint:errcheck

	// And one real session
	CreateSession(ctx, rdb, testSession) //nolint:errcheck

	got, err := ScanAllSessions(ctx, rdb)
	if err != nil {
		t.Fatalf("ScanAllSessions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 session, got %d (unrelated keys leaked in)", len(got))
	}
	if got[0].SandboxID != testSession.SandboxID {
		t.Errorf("wrong session returned: %q", got[0].SandboxID)
	}
}

func TestScanAllSessions_AfterDelete(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ctx := context.Background()

	CreateSession(ctx, rdb, Session{SandboxID: "sb-A", Owner: "0xA", Provider: "0xP", StartTime: 1, LastVoucherAt: 1}) //nolint:errcheck
	CreateSession(ctx, rdb, Session{SandboxID: "sb-B", Owner: "0xB", Provider: "0xP", StartTime: 2, LastVoucherAt: 2}) //nolint:errcheck

	DeleteSession(ctx, rdb, "sb-A") //nolint:errcheck

	got, err := ScanAllSessions(ctx, rdb)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SandboxID != "sb-B" {
		t.Fatalf("expected only sb-B, got %+v", got)
	}
}

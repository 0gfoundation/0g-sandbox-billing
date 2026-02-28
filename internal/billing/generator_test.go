package billing

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/0gfoundation/0g-sandbox-billing/internal/config"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// testConfig builds the minimal *config.Config that runGeneration needs.
func testConfig(intervalSec int64, pricePerMin string) *config.Config {
	return &config.Config{
		Billing: config.BillingConfig{
			VoucherIntervalSec: intervalSec,
			ComputePricePerMin: pricePerMin,
		},
	}
}

// ── No sessions ───────────────────────────────────────────────────────────────

func TestRunGeneration_NoSessions_NoVouchers(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ms := &mockSigner{}
	cfg := testConfig(3600, "100")

	runGeneration(context.Background(), cfg, rdb, ms, big.NewInt(100), zap.NewNop())

	if ms.count() != 0 {
		t.Errorf("expected 0 vouchers for empty Redis, got %d", ms.count())
	}
}

// ── Too recent: no voucher ────────────────────────────────────────────────────

func TestRunGeneration_SessionTooRecent_NoVoucher(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ms := &mockSigner{}
	cfg := testConfig(3600, "100")
	ctx := context.Background()

	// LastVoucherAt = now → periodEnd = now → elapsed = 0 → ceilMinutes(0) = 0
	now := time.Now().Unix()
	CreateSession(ctx, rdb, Session{ //nolint:errcheck
		SandboxID: "sb-fresh", Owner: testOwner, Provider: testProvider,
		StartTime: now, LastVoucherAt: now,
	})

	runGeneration(ctx, cfg, rdb, ms, big.NewInt(100), zap.NewNop())

	if ms.count() != 0 {
		t.Errorf("expected 0 vouchers for fresh session, got %d", ms.count())
	}
}

// ── Normal: session within interval ──────────────────────────────────────────

func TestRunGeneration_SessionWithinInterval_CorrectFee(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ms := &mockSigner{}
	// intervalSec = 7200 (2 h), pricePerMin = 50, session = 120 s ago
	// periodEnd = now (capped), elapsed = 120 s, ceilMinutes = 2, fee = 2*50 = 100
	cfg := testConfig(7200, "50")
	ctx := context.Background()
	const pricePerMin = int64(50)

	pastTime := time.Now().Unix() - 120
	CreateSession(ctx, rdb, Session{ //nolint:errcheck
		SandboxID: "sb-partial", Owner: testOwner, Provider: testProvider,
		StartTime: pastTime, LastVoucherAt: pastTime,
	})

	runGeneration(ctx, cfg, rdb, ms, big.NewInt(pricePerMin), zap.NewNop())

	v := ms.last()
	if v == nil {
		t.Fatal("expected voucher, got none")
	}
	if v.SandboxID != "sb-partial" {
		t.Errorf("SandboxID: got %q want %q", v.SandboxID, "sb-partial")
	}
	want := int64(2 * pricePerMin) // 2 mins * 50
	if v.TotalFee.Int64() != want {
		t.Errorf("TotalFee: got %d want %d", v.TotalFee.Int64(), want)
	}
}

// ── Hard cap: session older than interval ─────────────────────────────────────

func TestRunGeneration_HardCap_OneIntervalMax(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ms := &mockSigner{}
	const intervalSec = int64(3600)
	const pricePerMin = int64(100)
	cfg := testConfig(intervalSec, "100")
	ctx := context.Background()

	// Session is 2 intervals old; generator should only cover one interval
	// periodStart = now - 2*3600, periodEnd = periodStart + 3600 = now - 3600
	// elapsed = 3600, ceilMinutes = 60, fee = 60 * 100 = 6000
	old := time.Now().Unix() - 2*intervalSec
	CreateSession(ctx, rdb, Session{ //nolint:errcheck
		SandboxID: "sb-old", Owner: testOwner, Provider: testProvider,
		StartTime: old, LastVoucherAt: old,
	})

	runGeneration(ctx, cfg, rdb, ms, big.NewInt(pricePerMin), zap.NewNop())

	v := ms.last()
	if v == nil {
		t.Fatal("expected voucher, got none")
	}
	want := int64(60 * pricePerMin) // 60 min * 100
	if v.TotalFee.Int64() != want {
		t.Errorf("TotalFee: got %d want %d (hard-cap check)", v.TotalFee.Int64(), want)
	}
}

// ── LastVoucherAt updated to periodEnd ───────────────────────────────────────

func TestRunGeneration_UpdatesLastVoucherAt(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ms := &mockSigner{}
	const intervalSec = int64(3600)
	cfg := testConfig(intervalSec, "100")
	ctx := context.Background()

	// Session 2 intervals old → periodEnd = LastVoucherAt + intervalSec
	old := time.Now().Unix() - 2*intervalSec
	CreateSession(ctx, rdb, Session{ //nolint:errcheck
		SandboxID: "sb-adv", Owner: testOwner, Provider: testProvider,
		StartTime: old, LastVoucherAt: old,
	})

	runGeneration(ctx, cfg, rdb, ms, big.NewInt(100), zap.NewNop())

	sess, err := GetSession(ctx, rdb, "sb-adv")
	if err != nil || sess == nil {
		t.Fatalf("GetSession: err=%v sess=%v", err, sess)
	}
	expectedEnd := old + intervalSec
	if sess.LastVoucherAt != expectedEnd {
		t.Errorf("LastVoucherAt: got %d want %d", sess.LastVoucherAt, expectedEnd)
	}
}

// ── Multiple sessions ─────────────────────────────────────────────────────────

func TestRunGeneration_MultipleSessions_OneVoucherEach(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ms := &mockSigner{}
	cfg := testConfig(7200, "10")
	ctx := context.Background()

	past := time.Now().Unix() - 120
	for _, id := range []string{"sb-m1", "sb-m2", "sb-m3"} {
		CreateSession(ctx, rdb, Session{ //nolint:errcheck
			SandboxID: id, Owner: testOwner, Provider: testProvider,
			StartTime: past, LastVoucherAt: past,
		})
	}

	runGeneration(ctx, cfg, rdb, ms, big.NewInt(10), zap.NewNop())

	if ms.count() != 3 {
		t.Errorf("expected 3 vouchers for 3 sessions, got %d", ms.count())
	}

	// Collect voucher sandbox IDs and verify all three appear
	var ids []string
	for _, v := range ms.vouchers {
		ids = append(ids, v.SandboxID)
	}
	sort.Strings(ids)
	want := []string{"sb-m1", "sb-m2", "sb-m3"}
	for i, w := range want {
		if ids[i] != w {
			t.Errorf("voucher[%d]: got %q want %q", i, ids[i], w)
		}
	}
}

// ── IncrNonce error: skip that session ────────────────────────────────────────

func TestRunGeneration_IncrNonceError_SkipsSession(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ms := &mockSigner{incrErr: errors.New("nonce store down")}
	cfg := testConfig(3600, "100")
	ctx := context.Background()

	past := time.Now().Unix() - 120
	CreateSession(ctx, rdb, Session{ //nolint:errcheck
		SandboxID: "sb-nonce-err", Owner: testOwner, Provider: testProvider,
		StartTime: past, LastVoucherAt: past,
	})

	runGeneration(ctx, cfg, rdb, ms, big.NewInt(100), zap.NewNop())

	if ms.count() != 0 {
		t.Errorf("expected 0 vouchers on nonce error, got %d", ms.count())
	}
	// LastVoucherAt must NOT be advanced
	sess, _ := GetSession(ctx, rdb, "sb-nonce-err")
	if sess.LastVoucherAt != past {
		t.Errorf("LastVoucherAt should be unchanged on nonce error: got %d want %d",
			sess.LastVoucherAt, past)
	}
}

// ── IncrNonce error on one session, others still processed ───────────────────

func TestRunGeneration_IncrNonceError_OtherSessionsUnaffected(t *testing.T) {
	rdb, _ := newTestRedis(t)

	// Fail only for the specific owner that has "nonce-fail" in its address
	failOwner := "0xFAILFAILFAILFAILFAILFAILFAILFAILFAILFA"
	okOwner := "0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	ms := &selectiveErrSigner{failOwner: failOwner}
	cfg := testConfig(7200, "10")
	ctx := context.Background()

	past := time.Now().Unix() - 120
	CreateSession(ctx, rdb, Session{ //nolint:errcheck
		SandboxID: "sb-fail", Owner: failOwner, Provider: testProvider,
		StartTime: past, LastVoucherAt: past,
	})
	CreateSession(ctx, rdb, Session{ //nolint:errcheck
		SandboxID: "sb-ok", Owner: okOwner, Provider: testProvider,
		StartTime: past, LastVoucherAt: past,
	})

	runGeneration(ctx, cfg, rdb, ms, big.NewInt(10), zap.NewNop())

	if ms.count() != 1 {
		t.Errorf("expected 1 voucher (ok session only), got %d", ms.count())
	}
	if ms.last().SandboxID != "sb-ok" {
		t.Errorf("wrong voucher sandbox: got %q", ms.last().SandboxID)
	}
}

// selectiveErrSigner fails IncrNonce for a specific owner address.
type selectiveErrSigner struct {
	mockSigner
	failOwner string
}

func (s *selectiveErrSigner) IncrNonce(ctx context.Context, owner, provider string) (*big.Int, error) {
	if owner == s.failOwner {
		return nil, errors.New("selective nonce error")
	}
	return s.mockSigner.IncrNonce(ctx, owner, provider)
}

// ── SignAndEnqueue error: LastVoucherAt NOT updated ───────────────────────────

func TestRunGeneration_EnqueueError_LastVoucherAtUnchanged(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ms := &mockSigner{enqErr: errors.New("enqueue failed")}
	const intervalSec = int64(3600)
	cfg := testConfig(intervalSec, "100")
	ctx := context.Background()

	past := time.Now().Unix() - 2*intervalSec
	CreateSession(ctx, rdb, Session{ //nolint:errcheck
		SandboxID: "sb-enq-err", Owner: testOwner, Provider: testProvider,
		StartTime: past, LastVoucherAt: past,
	})

	runGeneration(ctx, cfg, rdb, ms, big.NewInt(100), zap.NewNop())

	sess, _ := GetSession(ctx, rdb, "sb-enq-err")
	if sess.LastVoucherAt != past {
		t.Errorf("LastVoucherAt must not advance on enqueue error: got %d want %d",
			sess.LastVoucherAt, past)
	}
}

// ── Voucher fields: User/Provider addresses ───────────────────────────────────

func TestRunGeneration_VoucherHasCorrectAddresses(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ms := &mockSigner{}
	cfg := testConfig(7200, "100")
	ctx := context.Background()

	past := time.Now().Unix() - 60
	CreateSession(ctx, rdb, Session{ //nolint:errcheck
		SandboxID: "sb-addr", Owner: testOwner, Provider: testProvider,
		StartTime: past, LastVoucherAt: past,
	})

	runGeneration(ctx, cfg, rdb, ms, big.NewInt(100), zap.NewNop())

	v := ms.last()
	if v == nil {
		t.Fatal("expected voucher")
	}
	zeroAddr := "0x0000000000000000000000000000000000000000"
	if v.User.Hex() == zeroAddr {
		t.Error("User address is zero")
	}
	if v.Provider.Hex() == zeroAddr {
		t.Error("Provider address is zero")
	}
	if v.Nonce == nil || v.Nonce.Sign() <= 0 {
		t.Error("Nonce should be positive")
	}
}

// ── Idempotent: re-run after session too recent does nothing ──────────────────

func TestRunGeneration_AfterUpdate_NoDoubleVoucher(t *testing.T) {
	rdb, _ := newTestRedis(t)
	ms := &mockSigner{}
	// 2-hour interval; session is 30 minutes old
	cfg := testConfig(7200, "100")
	ctx := context.Background()

	past := time.Now().Unix() - 1800 // 30 min ago
	CreateSession(ctx, rdb, Session{ //nolint:errcheck
		SandboxID: "sb-idem", Owner: testOwner, Provider: testProvider,
		StartTime: past, LastVoucherAt: past,
	})

	// First run → voucher emitted, LastVoucherAt = now
	runGeneration(ctx, cfg, rdb, ms, big.NewInt(100), zap.NewNop())
	if ms.count() != 1 {
		t.Fatalf("first run: expected 1 voucher, got %d", ms.count())
	}

	// Second run immediately after → LastVoucherAt just updated to ~now,
	// so elapsed ≈ 0, ceilMinutes = 0 → no new voucher
	runGeneration(ctx, cfg, rdb, ms, big.NewInt(100), zap.NewNop())
	if ms.count() != 1 {
		t.Errorf("second run: expected still 1 voucher total, got %d", ms.count())
	}
}

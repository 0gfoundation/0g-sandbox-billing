package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/redis/go-redis/v9"

	"github.com/0gfoundation/0g-sandbox-billing/internal/voucher"
)

// ── helpers ───────────────────────────────────────────────────────────────────

var (
	// Fixed deterministic test key (not used anywhere outside tests)
	testPrivKeyHex  = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	testChainID     = big.NewInt(31337)
	testContractHex = "0x5FbDB2315678afecb367f032d93F642f64180aa3"
	testProviderHex = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
)

func newTestSignerFull(t *testing.T) (*Signer, *redis.Client, common.Address) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	privKey, err := crypto.HexToECDSA(testPrivKeyHex)
	if err != nil {
		t.Fatalf("load test private key: %v", err)
	}
	signerAddr := crypto.PubkeyToAddress(privKey.PublicKey)

	contractAddr := common.HexToAddress(testContractHex)
	providerAddr := common.HexToAddress(testProviderHex)

	s := NewSigner(privKey, testChainID, contractAddr, providerAddr, rdb)
	return s, rdb, signerAddr
}

// ── IncrNonce ─────────────────────────────────────────────────────────────────

func TestIncrNonce_StartsAtOne(t *testing.T) {
	s, _, _ := newTestSignerFull(t)
	n, err := s.IncrNonce(context.Background(), testOwner, testProvider)
	if err != nil {
		t.Fatalf("IncrNonce: %v", err)
	}
	if n.Int64() != 1 {
		t.Errorf("first nonce: got %d want 1", n.Int64())
	}
}

func TestIncrNonce_Increments(t *testing.T) {
	s, _, _ := newTestSignerFull(t)
	ctx := context.Background()

	for i := int64(1); i <= 5; i++ {
		n, err := s.IncrNonce(ctx, testOwner, testProvider)
		if err != nil {
			t.Fatalf("IncrNonce [%d]: %v", i, err)
		}
		if n.Int64() != i {
			t.Errorf("nonce[%d]: got %d want %d", i, n.Int64(), i)
		}
	}
}

func TestIncrNonce_SeparateKeysPerPair(t *testing.T) {
	s, _, _ := newTestSignerFull(t)
	ctx := context.Background()

	ownerA := "0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	ownerB := "0xBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

	nA1, _ := s.IncrNonce(ctx, ownerA, testProvider)
	nB1, _ := s.IncrNonce(ctx, ownerB, testProvider)
	nA2, _ := s.IncrNonce(ctx, ownerA, testProvider)

	if nA1.Int64() != 1 {
		t.Errorf("ownerA first nonce: got %d want 1", nA1.Int64())
	}
	if nB1.Int64() != 1 {
		t.Errorf("ownerB first nonce: got %d want 1 (should have own counter)", nB1.Int64())
	}
	if nA2.Int64() != 2 {
		t.Errorf("ownerA second nonce: got %d want 2", nA2.Int64())
	}
}

func TestIncrNonce_CaseInsensitiveOwner(t *testing.T) {
	s, rdb, _ := newTestSignerFull(t)
	ctx := context.Background()

	upper := "0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	lower := strings.ToLower(upper)

	s.IncrNonce(ctx, upper, testProvider)  //nolint:errcheck
	s.IncrNonce(ctx, lower, testProvider)  //nolint:errcheck

	// Both calls should share the same key → nonce = 2
	expectedKey := fmt.Sprintf(voucher.NonceKeyFmt,
		strings.ToLower(upper), strings.ToLower(testProvider))
	val, err := rdb.Get(ctx, expectedKey).Result()
	if err != nil {
		t.Fatalf("nonce key not found: %v", err)
	}
	if val != "2" {
		t.Errorf("nonce after 2 calls with same address (different case): got %q want 2", val)
	}
}

// ── SignAndEnqueue ─────────────────────────────────────────────────────────────

func TestSignAndEnqueue_PushesToQueue(t *testing.T) {
	s, rdb, _ := newTestSignerFull(t)
	ctx := context.Background()

	v := &voucher.SandboxVoucher{
		SandboxID: "sb-sign-1",
		User:      common.HexToAddress(testOwner),
		Provider:  common.HexToAddress(testProviderHex),
		TotalFee:  big.NewInt(500),
		Nonce:     big.NewInt(1),
		UsageHash: voucher.BuildUsageHash("sb-sign-1", 1000, 1060, 1),
	}

	if err := s.SignAndEnqueue(ctx, v); err != nil {
		t.Fatalf("SignAndEnqueue: %v", err)
	}

	queueKey := fmt.Sprintf(voucher.VoucherQueueKeyFmt, common.HexToAddress(testProviderHex).Hex())
	n, err := rdb.LLen(ctx, queueKey).Result()
	if err != nil {
		t.Fatalf("LLEN: %v", err)
	}
	if n != 1 {
		t.Errorf("queue length: got %d want 1", n)
	}
}

func TestSignAndEnqueue_QueueItemIsValidJSON(t *testing.T) {
	s, rdb, _ := newTestSignerFull(t)
	ctx := context.Background()

	v := &voucher.SandboxVoucher{
		SandboxID: "sb-json",
		User:      common.HexToAddress(testOwner),
		Provider:  common.HexToAddress(testProviderHex),
		TotalFee:  big.NewInt(200),
		Nonce:     big.NewInt(7),
		UsageHash: voucher.BuildUsageHash("sb-json", 2000, 2060, 1),
	}
	s.SignAndEnqueue(ctx, v) //nolint:errcheck

	queueKey := fmt.Sprintf(voucher.VoucherQueueKeyFmt, common.HexToAddress(testProviderHex).Hex())
	raw, _ := rdb.LPop(ctx, queueKey).Result()

	var got voucher.SandboxVoucher
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("queue item is not valid JSON: %v", err)
	}
	if got.SandboxID != "sb-json" {
		t.Errorf("SandboxID: got %q want %q", got.SandboxID, "sb-json")
	}
	if got.Nonce.Int64() != 7 {
		t.Errorf("Nonce: got %d want 7", got.Nonce.Int64())
	}
	if len(got.Signature) != 65 {
		t.Errorf("Signature length: got %d want 65", len(got.Signature))
	}
}

func TestSignAndEnqueue_SignatureVerifiable(t *testing.T) {
	s, rdb, signerAddr := newTestSignerFull(t)
	ctx := context.Background()

	contractAddr := common.HexToAddress(testContractHex)

	v := &voucher.SandboxVoucher{
		SandboxID: "sb-verify",
		User:      common.HexToAddress(testOwner),
		Provider:  common.HexToAddress(testProviderHex),
		TotalFee:  big.NewInt(300),
		Nonce:     big.NewInt(1),
		UsageHash: voucher.BuildUsageHash("sb-verify", 3000, 3060, 1),
	}
	s.SignAndEnqueue(ctx, v) //nolint:errcheck

	// Deserialize from queue and verify signature
	queueKey := fmt.Sprintf(voucher.VoucherQueueKeyFmt, common.HexToAddress(testProviderHex).Hex())
	raw, _ := rdb.LPop(ctx, queueKey).Result()
	var got voucher.SandboxVoucher
	json.Unmarshal([]byte(raw), &got) //nolint:errcheck

	recovered, err := voucher.Verify(&got, testChainID, contractAddr)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if recovered != signerAddr {
		t.Errorf("recovered signer: got %s want %s", recovered.Hex(), signerAddr.Hex())
	}
}

func TestSignAndEnqueue_QueueKeyUsesProviderAddress(t *testing.T) {
	s, rdb, _ := newTestSignerFull(t)
	ctx := context.Background()

	v := &voucher.SandboxVoucher{
		SandboxID: "sb-qkey",
		User:      common.HexToAddress(testOwner),
		Provider:  common.HexToAddress(testProviderHex),
		TotalFee:  big.NewInt(100),
		Nonce:     big.NewInt(1),
	}
	s.SignAndEnqueue(ctx, v) //nolint:errcheck

	// Only the correct provider's queue should have an item
	correctKey := fmt.Sprintf(voucher.VoucherQueueKeyFmt, common.HexToAddress(testProviderHex).Hex())
	wrongKey := fmt.Sprintf(voucher.VoucherQueueKeyFmt, "0x0000000000000000000000000000000000000000")

	n, _ := rdb.LLen(ctx, correctKey).Result()
	if n != 1 {
		t.Errorf("correct queue length: got %d want 1", n)
	}
	nWrong, _ := rdb.LLen(ctx, wrongKey).Result()
	if nWrong != 0 {
		t.Errorf("wrong queue should be empty, got %d", nWrong)
	}
}

func TestSignAndEnqueue_MultipleVouchers_FIFOOrder(t *testing.T) {
	s, rdb, _ := newTestSignerFull(t)
	ctx := context.Background()
	queueKey := fmt.Sprintf(voucher.VoucherQueueKeyFmt, common.HexToAddress(testProviderHex).Hex())

	for i := int64(1); i <= 3; i++ {
		v := &voucher.SandboxVoucher{
			SandboxID: fmt.Sprintf("sb-%d", i),
			User:      common.HexToAddress(testOwner),
			Provider:  common.HexToAddress(testProviderHex),
			TotalFee:  big.NewInt(i * 100),
			Nonce:     big.NewInt(i),
		}
		s.SignAndEnqueue(ctx, v) //nolint:errcheck
	}

	// RPUSH → FIFO when reading with LPOP
	for i := int64(1); i <= 3; i++ {
		raw, err := rdb.LPop(ctx, queueKey).Result()
		if err != nil {
			t.Fatalf("LPop [%d]: %v", i, err)
		}
		var got voucher.SandboxVoucher
		json.Unmarshal([]byte(raw), &got) //nolint:errcheck
		want := fmt.Sprintf("sb-%d", i)
		if got.SandboxID != want {
			t.Errorf("FIFO order[%d]: got %q want %q", i, got.SandboxID, want)
		}
	}
}

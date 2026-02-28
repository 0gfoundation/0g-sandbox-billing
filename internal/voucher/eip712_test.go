package voucher

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var (
	testChainID      = big.NewInt(12345)
	testContractAddr = common.HexToAddress("0xDeAdBeEfDeAdBeEfDeAdBeEfDeAdBeEfDeAdBeEf")
)

// ── BuildUsageHash ─────────────────────────────────────────────────────────

func TestBuildUsageHash_Deterministic(t *testing.T) {
	h1 := BuildUsageHash("sb-abc", 1000, 2000, 17)
	h2 := BuildUsageHash("sb-abc", 1000, 2000, 17)
	if h1 != h2 {
		t.Fatal("BuildUsageHash is not deterministic")
	}
}

func TestBuildUsageHash_DifferentSandbox(t *testing.T) {
	h1 := BuildUsageHash("sb-aaa", 1000, 2000, 17)
	h2 := BuildUsageHash("sb-bbb", 1000, 2000, 17)
	if h1 == h2 {
		t.Fatal("different sandbox IDs should produce different hashes")
	}
}

func TestBuildUsageHash_DifferentPeriod(t *testing.T) {
	h1 := BuildUsageHash("sb-abc", 1000, 2000, 17)
	h2 := BuildUsageHash("sb-abc", 1000, 3000, 17)
	if h1 == h2 {
		t.Fatal("different periods should produce different hashes")
	}
}

func TestBuildUsageHash_Returns32Bytes(t *testing.T) {
	h := BuildUsageHash("sb-abc", 1000, 2000, 17)
	var zero [32]byte
	if h == zero {
		t.Fatal("hash should not be all zeros")
	}
}

// ── EIP-712 Sign + Verify ──────────────────────────────────────────────────

func newTestVoucher(t *testing.T) (*SandboxVoucher, common.Address) {
	t.Helper()
	privKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signerAddr := crypto.PubkeyToAddress(privKey.PublicKey)

	v := &SandboxVoucher{
		User:      common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Provider:  common.HexToAddress("0x2222222222222222222222222222222222222222"),
		TotalFee:  big.NewInt(1_000_000),
		UsageHash: BuildUsageHash("sb-test-001", 1_700_000_000, 1_700_003_600, 60),
		Nonce:     big.NewInt(1),
	}

	if err := Sign(v, privKey, testChainID, testContractAddr); err != nil {
		t.Fatalf("Sign error: %v", err)
	}
	return v, signerAddr
}

// TestSign_SignatureLength checks that the signature is 65 bytes.
func TestSign_SignatureLength(t *testing.T) {
	v, _ := newTestVoucher(t)
	if len(v.Signature) != 65 {
		t.Fatalf("expected 65-byte signature, got %d", len(v.Signature))
	}
}

// TestSign_RecoverAddress is the critical correctness test:
// the recovered address from Verify must equal the signing key's address.
func TestSign_RecoverAddress(t *testing.T) {
	privKey, _ := crypto.GenerateKey()
	expected := crypto.PubkeyToAddress(privKey.PublicKey)

	v := &SandboxVoucher{
		User:      common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Provider:  common.HexToAddress("0x2222222222222222222222222222222222222222"),
		TotalFee:  big.NewInt(5_000_000),
		UsageHash: BuildUsageHash("sb-verify", 1_000, 4_600, 60),
		Nonce:     big.NewInt(42),
	}

	if err := Sign(v, privKey, testChainID, testContractAddr); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	recovered, err := Verify(v, testChainID, testContractAddr)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if recovered != expected {
		t.Errorf("recovered %s, want %s", recovered.Hex(), expected.Hex())
	}
}

// TestSign_DifferentChainID verifies domain separation:
// a signature for chainID=12345 should NOT verify correctly on chainID=1.
func TestSign_DifferentChainID(t *testing.T) {
	privKey, _ := crypto.GenerateKey()
	expected := crypto.PubkeyToAddress(privKey.PublicKey)

	v := &SandboxVoucher{
		User:      common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Provider:  common.HexToAddress("0x2222222222222222222222222222222222222222"),
		TotalFee:  big.NewInt(1_000_000),
		UsageHash: BuildUsageHash("sb-chain", 0, 3600, 60),
		Nonce:     big.NewInt(1),
	}

	// Sign for chainID=12345
	if err := Sign(v, privKey, testChainID, testContractAddr); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Verify with chainID=1 — recovered address should differ
	wrongChain := big.NewInt(1)
	recovered, err := Verify(v, wrongChain, testContractAddr)
	if err != nil {
		// An error here is also acceptable (malformed recovery)
		return
	}
	if recovered == expected {
		t.Error("signature should NOT verify on a different chainID")
	}
}

// TestSign_DifferentContract verifies that the contract address is part of the domain.
func TestSign_DifferentContract(t *testing.T) {
	privKey, _ := crypto.GenerateKey()
	expected := crypto.PubkeyToAddress(privKey.PublicKey)

	v := &SandboxVoucher{
		User:      common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Provider:  common.HexToAddress("0x2222222222222222222222222222222222222222"),
		TotalFee:  big.NewInt(1_000_000),
		UsageHash: BuildUsageHash("sb-contract", 0, 3600, 60),
		Nonce:     big.NewInt(1),
	}

	if err := Sign(v, privKey, testChainID, testContractAddr); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	wrongContract := common.HexToAddress("0x0000000000000000000000000000000000000001")
	recovered, err := Verify(v, testChainID, wrongContract)
	if err != nil {
		return
	}
	if recovered == expected {
		t.Error("signature should NOT verify against a different contract address")
	}
}

// TestSign_TamperedFee verifies that changing the fee invalidates the signature.
func TestSign_TamperedFee(t *testing.T) {
	privKey, _ := crypto.GenerateKey()
	expected := crypto.PubkeyToAddress(privKey.PublicKey)

	v := &SandboxVoucher{
		User:      common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Provider:  common.HexToAddress("0x2222222222222222222222222222222222222222"),
		TotalFee:  big.NewInt(1_000_000),
		UsageHash: BuildUsageHash("sb-tamper", 0, 3600, 60),
		Nonce:     big.NewInt(1),
	}

	if err := Sign(v, privKey, testChainID, testContractAddr); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Tamper with TotalFee after signing
	v.TotalFee = big.NewInt(999_999_999)

	recovered, err := Verify(v, testChainID, testContractAddr)
	if err != nil {
		return
	}
	if recovered == expected {
		t.Error("tampered TotalFee should invalidate the signature")
	}
}

// TestSign_NonceMonotonicity verifies that nonce=1 and nonce=2 produce different digests.
func TestSign_NonceMonotonicity(t *testing.T) {
	privKey, _ := crypto.GenerateKey()

	makeVoucher := func(nonce int64) *SandboxVoucher {
		v := &SandboxVoucher{
			User:      common.HexToAddress("0x1111111111111111111111111111111111111111"),
			Provider:  common.HexToAddress("0x2222222222222222222222222222222222222222"),
			TotalFee:  big.NewInt(1_000_000),
			UsageHash: BuildUsageHash("sb-nonce", 0, 3600, 60),
			Nonce:     big.NewInt(nonce),
		}
		if err := Sign(v, privKey, testChainID, testContractAddr); err != nil {
			t.Fatal(err)
		}
		return v
	}

	v1 := makeVoucher(1)
	v2 := makeVoucher(2)

	// Signatures must differ (different nonce → different digest)
	if string(v1.Signature) == string(v2.Signature) {
		t.Error("different nonces should produce different signatures")
	}
}

// ── domainSeparator ──────────────────────────────────────────────────────────

func TestDomainSeparator_Stable(t *testing.T) {
	sep1 := domainSeparator(testChainID, testContractAddr)
	sep2 := domainSeparator(testChainID, testContractAddr)
	if sep1 != sep2 {
		t.Fatal("domainSeparator is not stable")
	}
}

func TestDomainSeparator_ChainIDDiff(t *testing.T) {
	sep1 := domainSeparator(big.NewInt(1), testContractAddr)
	sep2 := domainSeparator(big.NewInt(2), testContractAddr)
	if sep1 == sep2 {
		t.Fatal("different chainIDs should produce different separators")
	}
}

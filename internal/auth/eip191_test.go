package auth

import (
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// TestHashMessage_Prefix verifies the EIP-191 prefix is applied correctly.
// We check that the same message always produces the same hash, and that
// a different message produces a different hash.
func TestHashMessage_Deterministic(t *testing.T) {
	msg := []byte("hello 0G")
	h1 := HashMessage(msg)
	h2 := HashMessage(msg)
	if string(h1) != string(h2) {
		t.Fatal("HashMessage is not deterministic")
	}
}

func TestHashMessage_DifferentMessages(t *testing.T) {
	h1 := HashMessage([]byte("foo"))
	h2 := HashMessage([]byte("bar"))
	if string(h1) == string(h2) {
		t.Fatal("different messages produced the same hash")
	}
}

func TestHashMessage_Length(t *testing.T) {
	hash := HashMessage([]byte("test"))
	if len(hash) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(hash))
	}
}

// TestRecover_ValidSignature is the core test: sign a message with a known key,
// recover the address, and verify it matches.
func TestRecover_ValidSignature(t *testing.T) {
	privKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	expected := crypto.PubkeyToAddress(privKey.PublicKey)

	msg := []byte(`{"action":"start","nonce":"abc"}`)
	hash := HashMessage(msg)

	// crypto.Sign returns V in {0,1}; Ethereum convention is {27,28}
	sig, err := crypto.Sign(hash, privKey)
	if err != nil {
		t.Fatal(err)
	}
	sig[64] += 27

	got, err := Recover(msg, sig)
	if err != nil {
		t.Fatalf("Recover error: %v", err)
	}
	if got != expected {
		t.Errorf("got %s, want %s", got.Hex(), expected.Hex())
	}
}

// TestRecover_V0and1 verifies that V in {0,1} (without +27) also works.
func TestRecover_V0and1(t *testing.T) {
	privKey, _ := crypto.GenerateKey()
	expected := crypto.PubkeyToAddress(privKey.PublicKey)

	msg := []byte("test message")
	hash := HashMessage(msg)
	sig, _ := crypto.Sign(hash, privKey)
	// Leave V as 0 or 1 (no +27)

	got, err := Recover(msg, sig)
	if err != nil {
		t.Fatalf("Recover error: %v", err)
	}
	if got != expected {
		t.Errorf("got %s, want %s", got.Hex(), expected.Hex())
	}
}

// TestRecover_WrongMessage verifies that signing one message and recovering
// with a different message returns a different (wrong) address.
func TestRecover_WrongMessage(t *testing.T) {
	privKey, _ := crypto.GenerateKey()
	expected := crypto.PubkeyToAddress(privKey.PublicKey)

	msg := []byte("original message")
	hash := HashMessage(msg)
	sig, _ := crypto.Sign(hash, privKey)
	sig[64] += 27

	wrong, err := Recover([]byte("tampered message"), sig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wrong == expected {
		t.Error("tampered message should not recover the original signer")
	}
}

// TestRecover_InvalidSigLength verifies that a wrong-length signature returns an error.
func TestRecover_InvalidSigLength(t *testing.T) {
	_, err := Recover([]byte("msg"), []byte("tooshort"))
	if err == nil {
		t.Fatal("expected error for short signature")
	}
}

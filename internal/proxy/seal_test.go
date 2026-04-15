package proxy

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestInjectSeal_EnvVarsInjected(t *testing.T) {
	teeKey, _ := crypto.GenerateKey()
	body := []byte(`{"labels":{"daytona-owner":"0xOWNER","0g-sealed":"true"}}`)

	out, err := InjectSeal(body, teeKey, "sha256:abc123")
	if err != nil {
		t.Fatalf("InjectSeal error: %v", err)
	}

	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck

	env, _ := m["env"].(map[string]any)
	if env == nil {
		t.Fatal("env map missing")
	}

	// Private key must be present in env
	sealKey, _ := env["SANDBOX_SEAL_KEY"].(string)
	if len(sealKey) == 0 {
		t.Fatal("SANDBOX_SEAL_KEY missing from env")
	}
	if sealKey[:2] != "0x" {
		t.Errorf("SANDBOX_SEAL_KEY must be 0x-prefixed, got %s", sealKey[:10])
	}

	// Attestation must be present and parseable
	attestRaw, _ := env["SANDBOX_SEAL_ATTESTATION"].(string)
	if attestRaw == "" {
		t.Fatal("SANDBOX_SEAL_ATTESTATION missing from env")
	}
	var attest map[string]any
	if err := json.Unmarshal([]byte(attestRaw), &attest); err != nil {
		t.Fatalf("SANDBOX_SEAL_ATTESTATION is not valid JSON: %v", err)
	}
}

func TestInjectSeal_AttestationFields(t *testing.T) {
	teeKey, _ := crypto.GenerateKey()
	imageHash := "sha256:deadbeef"
	body := []byte(`{"labels":{"daytona-owner":"0xOWNER","0g-sealed":"true"}}`)

	out, err := InjectSeal(body, teeKey, imageHash)
	if err != nil {
		t.Fatalf("InjectSeal error: %v", err)
	}

	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck

	env := m["env"].(map[string]any)
	var attest map[string]any
	json.Unmarshal([]byte(env["SANDBOX_SEAL_ATTESTATION"].(string)), &attest) //nolint:errcheck

	if attest["image_hash"] != imageHash {
		t.Errorf("image_hash: got %v want %v", attest["image_hash"], imageHash)
	}
	if attest["seal_id"] == "" {
		t.Error("seal_id must not be empty")
	}
	if attest["pubkey"] == "" {
		t.Error("pubkey must not be empty")
	}
	sigStr, _ := attest["signature"].(string)
	if len(sigStr) < 4 {
		t.Error("signature must not be empty")
	}
	if attest["ts"] == nil {
		t.Error("ts must be present")
	}
}

func TestInjectSeal_SealIDInLabel(t *testing.T) {
	teeKey, _ := crypto.GenerateKey()
	body := []byte(`{"labels":{"daytona-owner":"0xOWNER","0g-sealed":"true"}}`)

	out, err := InjectSeal(body, teeKey, "sha256:abc")
	if err != nil {
		t.Fatalf("InjectSeal error: %v", err)
	}

	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck

	labels := m["labels"].(map[string]any)
	sealID, _ := labels[sealIDLabel].(string)
	if len(sealID) != 32 { // 16 bytes = 32 hex chars
		t.Errorf("0g-seal-id label: got len %d want 32, value=%q", len(sealID), sealID)
	}
	// Must be valid hex
	if _, err := hex.DecodeString(sealID); err != nil {
		t.Errorf("0g-seal-id is not valid hex: %v", err)
	}
}

func TestInjectSeal_PrivkeyNotInLabels(t *testing.T) {
	teeKey, _ := crypto.GenerateKey()
	body := []byte(`{"labels":{"daytona-owner":"0xOWNER","0g-sealed":"true"}}`)

	out, err := InjectSeal(body, teeKey, "sha256:abc")
	if err != nil {
		t.Fatalf("InjectSeal error: %v", err)
	}

	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck

	labels := m["labels"].(map[string]any)
	for k, v := range labels {
		vs, _ := v.(string)
		if len(vs) >= 64 && k != sealIDLabel {
			t.Errorf("label %q looks like it might contain a private key: %s", k, vs[:16])
		}
	}
}

func TestInjectSeal_SignatureVerifiable(t *testing.T) {
	teeKey, _ := crypto.GenerateKey()
	teeAddr := crypto.PubkeyToAddress(teeKey.PublicKey)
	imageHash := "sha256:cafebabe"
	body := []byte(`{"labels":{"daytona-owner":"0xOWNER","0g-sealed":"true"}}`)

	out, err := InjectSeal(body, teeKey, imageHash)
	if err != nil {
		t.Fatalf("InjectSeal error: %v", err)
	}

	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck

	env := m["env"].(map[string]any)
	var attest map[string]any
	json.Unmarshal([]byte(env["SANDBOX_SEAL_ATTESTATION"].(string)), &attest) //nolint:errcheck

	sealID := attest["seal_id"].(string)
	pubKey := attest["pubkey"].(string)
	ts := int64(attest["ts"].(float64))
	sigHex := attest["signature"].(string)[2:] // strip 0x

	// Reconstruct the hash
	msg := fmt.Sprintf("ImageAttestation:%s:%s:%s:%d", sealID, pubKey, imageHash, ts)
	hash := crypto.Keccak256Hash([]byte(msg))

	// Recover signer
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	sigBytes[64] -= 27 // convert back from 27/28 to 0/1
	recovered, err := crypto.SigToPub(hash[:], sigBytes)
	if err != nil {
		t.Fatalf("SigToPub: %v", err)
	}
	recoveredAddr := crypto.PubkeyToAddress(*recovered)

	if recoveredAddr != teeAddr {
		t.Errorf("signature signer: got %s want %s", recoveredAddr.Hex(), teeAddr.Hex())
	}
}

func TestStripSealKey_RemovesPrivkey(t *testing.T) {
	body := []byte(`{"id":"abc","env":{"SANDBOX_SEAL_KEY":"0xdeadbeef","SANDBOX_SEAL_ATTESTATION":"{}"},"labels":{}}`)
	out, err := stripSealKey(body)
	if err != nil {
		t.Fatalf("stripSealKey: %v", err)
	}
	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck
	env := m["env"].(map[string]any)
	if _, ok := env["SANDBOX_SEAL_KEY"]; ok {
		t.Error("SANDBOX_SEAL_KEY must be removed from response")
	}
	if env["SANDBOX_SEAL_ATTESTATION"] == nil {
		t.Error("SANDBOX_SEAL_ATTESTATION must be preserved")
	}
}

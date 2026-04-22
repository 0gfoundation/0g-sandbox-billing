// Sealed container self-verification.
//
// Checks:
//  1. SANDBOX_SEAL_KEY derives the same Ethereum address as attestation.pubkey
//     → confirms the proxy injected a consistent keypair
//  2. attestation.signature recovers the TEE signer address
//     → confirms the attestation was issued by the real TEE key
//     If TEE_SIGNER_ADDRESS is set, also asserts the recovered address matches.
//
// After verification, starts an HTTP server on :8080.
//
//	GET /result  — returns verification result as plain text
//	GET /healthz — liveness probe
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	eciesgo "github.com/ecies/go/v2"
	"github.com/ethereum/go-ethereum/crypto"
)

const logPath = "/tmp/seal-verify.log"

type attestation struct {
	SealID    string `json:"seal_id"`
	Pubkey    string `json:"pubkey"`
	ImageHash string `json:"image_hash"`
	Signature string `json:"signature"`
	Ts        int64  `json:"ts"`
}

var lines []string

func logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Println(msg)
	lines = append(lines, msg)
}

func fail(format string, a ...any) {
	msg := "FAIL: " + fmt.Sprintf(format, a...)
	fmt.Fprintln(os.Stderr, msg)
	lines = append(lines, msg)
	flush()
	os.Exit(1)
}

func flush() {
	os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0644) //nolint:errcheck
}

// provisionFromAttestor POSTs the attestation to the attestor's /provision
// endpoint and ECIES-decrypts the returned `encrypted_agent_seal_priv` with
// the container's SANDBOX_SEAL_KEY. All outcomes (success or failure) are
// appended to the verification log.
func provisionFromAttestor(attestorURL string, sealKeyBytes []byte, a attestation) {
	imageHashHex := strings.TrimPrefix(a.ImageHash, "sha256:")
	reqBody, _ := json.Marshal(map[string]any{
		"seal_id":           "0x" + a.SealID,
		"container_pubkey":  a.Pubkey, // already 0x-prefixed compressed
		"image_hash":        "0x" + imageHashHex,
		"issued_at":         a.Ts,
		"sandbox_signature": a.Signature,
	})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(attestorURL+"/provision", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		logf("FAIL provision: POST error: %v", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logf("FAIL provision: HTTP %d: %s", resp.StatusCode, string(body))
		return
	}

	var out struct {
		EncryptedAgentSealPriv string `json:"encrypted_agent_seal_priv"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		logf("FAIL provision: decode response: %v", err)
		return
	}
	if out.EncryptedAgentSealPriv == "" {
		logf("FAIL provision: empty encrypted_agent_seal_priv")
		return
	}

	ctBytes, err := hex.DecodeString(strings.TrimPrefix(out.EncryptedAgentSealPriv, "0x"))
	if err != nil {
		logf("FAIL provision: decode ciphertext hex: %v", err)
		return
	}
	privKey := eciesgo.NewPrivateKeyFromBytes(sealKeyBytes)
	plaintext, err := eciesgo.Decrypt(privKey, ctBytes)
	if err != nil {
		logf("FAIL provision: ECIES decrypt: %v", err)
		return
	}

	logf("OK   provisioned agent_seal_priv: 0x%s", hex.EncodeToString(plaintext))
}

func main() {
	sealKey   := os.Getenv("SANDBOX_SEAL_KEY")
	attestRaw := os.Getenv("SANDBOX_SEAL_ATTESTATION")
	teeSigner := os.Getenv("TEE_SIGNER_ADDRESS")
	apiKey    := os.Getenv("API_KEY")

	if sealKey == ""   { fail("SANDBOX_SEAL_KEY not set") }
	if attestRaw == "" { fail("SANDBOX_SEAL_ATTESTATION not set") }

	logf("--- Sealed Container Verification ---")
	logf("")

	var a attestation
	if err := json.Unmarshal([]byte(attestRaw), &a); err != nil {
		fail("SANDBOX_SEAL_ATTESTATION is not valid JSON: %v", err)
	}
	if a.SealID == "" || a.Pubkey == "" || a.ImageHash == "" || a.Signature == "" {
		fail("attestation missing required fields")
	}

	logf("seal_id:    %s", a.SealID)
	logf("pubkey:     %s", a.Pubkey)
	logf("image_hash: %s", a.ImageHash)
	logf("ts:         %d", a.Ts)
	logf("")

	// ── Check 1: keypair consistency ─────────────────────────────────────────
	keyBytes, err := hex.DecodeString(strings.TrimPrefix(sealKey, "0x"))
	if err != nil {
		fail("decode SANDBOX_SEAL_KEY: %v", err)
	}
	privKey, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		fail("parse SANDBOX_SEAL_KEY: %v", err)
	}
	derived := "0x" + hex.EncodeToString(crypto.CompressPubkey(&privKey.PublicKey))
	if !strings.EqualFold(derived, a.Pubkey) {
		fail("keypair mismatch\n  derived : %s\n  pubkey  : %s", derived, a.Pubkey)
	}
	logf("OK   keypair match: SANDBOX_SEAL_KEY → %s", derived)

	// ── Check 2: TEE attestation signature ───────────────────────────────────
	msg  := fmt.Sprintf("ImageAttestation:%s:%s:%s:%d", a.SealID, a.Pubkey, a.ImageHash, a.Ts)
	hash := crypto.Keccak256Hash([]byte(msg))

	sigBytes, err := hex.DecodeString(strings.TrimPrefix(a.Signature, "0x"))
	if err != nil {
		fail("decode signature: %v", err)
	}
	if len(sigBytes) != 65 {
		fail("signature must be 65 bytes, got %d", len(sigBytes))
	}
	sigBytes[64] -= 27 // normalise V 27/28 → 0/1
	pubKey, err := crypto.SigToPub(hash[:], sigBytes)
	if err != nil {
		fail("recover TEE signer: %v", err)
	}
	recovered := crypto.PubkeyToAddress(*pubKey).Hex()
	logf("OK   TEE signature valid, signer: %s", recovered)

	if teeSigner != "" {
		if !strings.EqualFold(recovered, teeSigner) {
			fail("TEE signer mismatch\n  recovered: %s\n  expected : %s", recovered, teeSigner)
		}
		logf("OK   TEE signer matches TEE_SIGNER_ADDRESS: %s", teeSigner)
	}

	logf("")
	if apiKey != "" {
		logf("API_KEY (from env): %s", apiKey)
	} else {
		logf("API_KEY (from env): <unset>")
	}
	// ── Optional: call attestor /provision and decrypt agent seal priv ───────
	if attestorURL := os.Getenv("ATTESTOR_URL"); attestorURL != "" {
		logf("")
		logf("--- Provisioning from attestor: %s ---", attestorURL)
		provisionFromAttestor(attestorURL, keyBytes, a)
	}

	logf("")
	logf("ALL CHECKS PASSED")
	flush()

	// ── HTTP server ───────────────────────────────────────────────────────────
	result := strings.Join(lines, "\n") + "\n"
	http.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, result)
	})
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	fmt.Println("Listening on :8080  GET /result")
	http.ListenAndServe(":8080", nil) //nolint:errcheck
}

// Sealed container bootstrap.
//
// Pipeline:
//   1. Attestation self-check
//        - SANDBOX_SEAL_KEY derives the same compressed pubkey as attestation.pubkey.
//        - attestation.signature recovers the TEE signer (and matches
//          TEE_SIGNER_ADDRESS if set).
//
//   2. Provision (only if ATTESTOR_URL is set)
//        - POST /provision → ECIES-decrypt encrypted_agent_seal_priv with
//          SANDBOX_SEAL_KEY.
//
//   3. Bootstrap from AgenticID (only if CHAIN_RPC_URL is set)
//        - Phase 1: FilterLogs(AgentSealSet, sealId) — initial backward scan
//          plus forward poll. Returns (agentId, mintBlock).
//        - Phase 2: intelligentDatasOf(agentId).
//        - Phase 3: per-entry exec `0g-storage-client download`, exponential
//          backoff (2,4,8,16,32,60,60,…), 10 attempts.
//        - Phase 4: ECIES(agent_seal_priv, sealedKey) → data_key,
//          AES-GCM-256(data_key, nonce(12)||ciphertext+tag).
//
//   4. Status report — only if BOTH provision AND bootstrap succeed,
//      POST /status with status="running" signed by agent_seal_priv.
//
// HTTP server on :8080 exposes /dashboard (log), /healthz, and /hello (A2A).

package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	eciesgo "github.com/ecies/go/v2"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	logPath           = "/tmp/seal-bootstrap.log"
	transferScanChunk = 5000 // ITransferred backward scan chunk size
	bootstrapTimeout  = 10 * time.Minute
	mintPollEvery     = 5 * time.Second
	downloadAttempts  = 10
)

// Minimal ABI subset we use against AgenticID. Full ABI is at
// contracts/out/AgenticID.sol/AgenticID.json. sealedKey is NOT stored on
// IntelligentData — it is emitted in the ITransferred event (mint + transfers).
const agenticIDABI = `[
  {"type":"function","name":"getAgentIdBySealId","stateMutability":"view","inputs":[{"name":"sealId","type":"bytes32"}],"outputs":[{"name":"","type":"uint256"}]},
  {"type":"function","name":"intelligentDatasOf","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"name":"","type":"tuple[]","components":[{"name":"dataDescription","type":"string"},{"name":"dataHash","type":"bytes32"}]}]},
  {"type":"event","name":"ITransferred","anonymous":false,"inputs":[{"name":"from","type":"address","indexed":true},{"name":"to","type":"address","indexed":true},{"name":"tokenId","type":"uint256","indexed":true},{"name":"entries","type":"tuple[]","indexed":false,"components":[{"name":"dataHash","type":"bytes32"},{"name":"sealedKey","type":"bytes"}]}]}
]`

type attestation struct {
	SealID    string `json:"seal_id"`
	Pubkey    string `json:"pubkey"`
	ImageHash string `json:"image_hash"`
	Signature string `json:"signature"`
	Ts        int64  `json:"ts"`
}

type intelligentData struct {
	DataDescription string
	DataHash        [32]byte
}

type sealedKeyEntry struct {
	DataHash  [32]byte
	SealedKey []byte
}

type storageDescription struct {
	Role       string `json:"role"`
	StoragePtr struct {
		Indexer  string `json:"indexer"`
		RootHash string `json:"root_hash"`
	} `json:"storage_ptr"`
}

// decryptedEntry is one i-data entry after AES-GCM decrypt; collected so we can
// dispatch (e.g. role="config" -> agent config) once bootstrap is done.
type decryptedEntry struct {
	Role      string
	Plaintext []byte
}

// agentConfig is the framework-agnostic schema. framework.name selects an
// adapter that translates the rest into framework-specific config + launches
// the corresponding agent process.
type agentConfig struct {
	Framework struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"framework"`
	Inference struct {
		Provider  string   `json:"provider"`
		Model     string   `json:"model"`
		Fallbacks []string `json:"fallbacks"` // optional fallback model IDs (no provider prefix)
	} `json:"inference"`
	Persona struct {
		SystemPrompt string `json:"system_prompt"`
	} `json:"persona"`
	Skills []any `json:"skills"`
}

// agentState lets the HTTP catch-all proxy lazily pick up agent_seal_priv +
// upstream URL after bootstrap completes (the server listens earlier, so
// /dashboard is reachable while provisioning is still in flight). sealID is
// included in /hello so verifiers can correlate the response with on-chain
// identity.
type agentState struct {
	mu            sync.RWMutex
	agentSealPriv []byte
	upstreamURL   string
	sealID        string // hex (no 0x prefix)
	cfg           *agentConfig
}

func (s *agentState) snapshot() (priv []byte, upstream, sealID string, cfg *agentConfig) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.agentSealPriv, s.upstreamURL, s.sealID, s.cfg
}

func (s *agentState) set(priv []byte, upstream, sealID string, cfg *agentConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentSealPriv = priv
	s.upstreamURL = upstream
	s.sealID = sealID
	s.cfg = cfg
}

var (
	lines   []string
	linesMu sync.RWMutex
	agent   agentState
)

func logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Println(msg)
	linesMu.Lock()
	lines = append(lines, msg)
	linesMu.Unlock()
}

func fail(format string, a ...any) {
	msg := "FAIL: " + fmt.Sprintf(format, a...)
	fmt.Fprintln(os.Stderr, msg)
	linesMu.Lock()
	lines = append(lines, msg)
	linesMu.Unlock()
	flush()
	os.Exit(1)
}

func flush() {
	linesMu.RLock()
	body := strings.Join(lines, "\n") + "\n"
	linesMu.RUnlock()
	os.WriteFile(logPath, []byte(body), 0644) //nolint:errcheck
}

func currentLog() string {
	linesMu.RLock()
	defer linesMu.RUnlock()
	return strings.Join(lines, "\n") + "\n"
}

func main() {
	sealKey := os.Getenv("SANDBOX_SEAL_KEY")
	attestRaw := os.Getenv("SANDBOX_SEAL_ATTESTATION")
	teeSigner := os.Getenv("TEE_SIGNER_ADDRESS")
	apiKey := os.Getenv("API_KEY")

	if sealKey == "" {
		fail("SANDBOX_SEAL_KEY not set")
	}
	if attestRaw == "" {
		fail("SANDBOX_SEAL_ATTESTATION not set")
	}

	logf("--- Sealed Container Bootstrap ---")
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

	// ── Phase 0: keypair + TEE signature self-check ─────────────────────────
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
	logf("OK   keypair match: SANDBOX_SEAL_KEY -> %s", derived)

	canonical := fmt.Sprintf("ImageAttestation:%s:%s:%s:%d", a.SealID, a.Pubkey, a.ImageHash, a.Ts)
	hash := crypto.Keccak256Hash([]byte(canonical))
	sigBytes, err := hex.DecodeString(strings.TrimPrefix(a.Signature, "0x"))
	if err != nil {
		fail("decode signature: %v", err)
	}
	if len(sigBytes) != 65 {
		fail("signature must be 65 bytes, got %d", len(sigBytes))
	}
	sigBytes[64] -= 27
	pub, err := crypto.SigToPub(hash[:], sigBytes)
	if err != nil {
		fail("recover TEE signer: %v", err)
	}
	recovered := crypto.PubkeyToAddress(*pub).Hex()
	logf("OK   TEE signature valid, signer: %s", recovered)
	if teeSigner != "" {
		if !strings.EqualFold(recovered, teeSigner) {
			fail("TEE signer mismatch\n  recovered: %s\n  expected : %s", recovered, teeSigner)
		}
		logf("OK   TEE signer matches TEE_SIGNER_ADDRESS: %s", teeSigner)
	}
	logf("")
	if apiKey != "" {
		logf("API_KEY (from env): <set, %d chars>", len(apiKey))
	} else {
		logf("API_KEY (from env): <unset>")
	}

	// Start the HTTP server early so /dashboard is reachable while bootstrap is
	// still running (provision + waitForMint + scan can take minutes).
	startHTTPServer()

	// Phase 1+2: provision + bootstrap, only report running on full success.
	attestorURL := os.Getenv("ATTESTOR_URL")
	chainRPC := os.Getenv("CHAIN_RPC_URL")
	contractAddr := os.Getenv("AGENTIC_ID_ADDR")
	fallbackIndexer := os.Getenv("INDEXER_URL")

	if attestorURL == "" {
		logf("")
		logf("ATTESTOR_URL unset -- skipping provision / bootstrap / status")
	} else if chainRPC == "" || contractAddr == "" || fallbackIndexer == "" {
		logf("")
		logf("missing required env (CHAIN_RPC_URL=%q AGENTIC_ID_ADDR=%q INDEXER_URL=%q) -- skipping provision / bootstrap / status",
			chainRPC, contractAddr, fallbackIndexer)
	} else {
		logf("")
		logf("--- Provisioning from attestor: %s ---", attestorURL)
		agentSealPriv := provisionFromAttestor(attestorURL, keyBytes, a)
		if agentSealPriv != nil {
			logf("")
			logf("--- Bootstrap from AgenticID %s (rpc %s, fallback indexer %s) ---",
				contractAddr, chainRPC, fallbackIndexer)
			entries := bootstrap(chainRPC, contractAddr, a.SealID, agentSealPriv, fallbackIndexer)
			if entries != nil {
				reportStatus(attestorURL, agentSealPriv, a.SealID, "running", "")
				logf("")
				logf("--- Starting agent ---")
				tryStartAgent(entries, agentSealPriv, apiKey, a.SealID)
			} else {
				logf("bootstrap failed -- not reporting status=running")
			}
		}
	}

	logf("")
	logf("ALL DONE")
	flush()

	// Block forever — HTTP server is already running in its own goroutine.
	select {}
}

// startHTTPServer launches a goroutine serving the container's HTTP surface
// on :8080. Only two paths are owned by the bootstrap layer:
//
//	GET /healthz — container liveness probe (always 200 OK; orchestrators
//	               check this before the agent itself is up).
//	GET /log     — bootstrap diagnostic log (provision / bootstrap progress).
//	               Plain text, NOT signed. Owner-facing only — sensitive
//	               material (private keys, decrypted plaintext) is redacted
//	               before reaching the log.
//
// Everything else, including paths the agent normally serves like /dashboard
// and /hello, is reverse-proxied to the agent upstream (e.g. openclaw on
// :3284) with a secp256k1 signature over the response so callers can verify
// it came from this attested instance.
func startHTTPServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/log", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, currentLog())
	})
	mux.HandleFunc("/log/openclaw", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		body, err := os.ReadFile("/tmp/openclaw.log")
		if err != nil {
			fmt.Fprintf(w, "openclaw log not available: %v\n", err)
			return
		}
		w.Write(body) //nolint:errcheck
	})
	mux.HandleFunc("/hello", helloHandler)
	mux.HandleFunc("/", agentProxyHandler)
	go func() {
		fmt.Println("Listening on :8080  GET /healthz | /log | /log/openclaw | /hello (signed) | /* (agent proxy)")
		_ = http.ListenAndServe(":8080", mux)
	}()
}

// helloHandler is the agent's signed A2A self-introduction endpoint. Bootstrap
// synthesises a JSON envelope describing the agent's on-chain identity, the
// inference framework + model it runs, and the configured persona/skills,
// then signs the response with agent_seal_priv via the same scheme the
// catch-all proxy uses. A peer agent or external verifier can:
//
//	GET /hello            (no body)
//	→ ecrecover the X-Agent-Signature header
//	→ confirm the signer matches the AgenticID owner for seal_id
//	→ trust the self-described identity
//
// Returns 503 until the agent is armed (post-bootstrap) so callers don't
// mistake the no-content default for a successful self-introduction.
func helloHandler(w http.ResponseWriter, r *http.Request) {
	priv, _, sealID, cfg := agent.snapshot()
	if priv == nil {
		http.Error(w, "agent not ready", http.StatusServiceUnavailable)
		return
	}

	identity := map[string]any{
		"seal_id": "0x" + sealID,
	}
	if pk, err := crypto.ToECDSA(priv); err == nil {
		identity["address"] = crypto.PubkeyToAddress(pk.PublicKey).Hex()
	}

	resp := map[string]any{
		"hello":    "I am an attested 0G sandbox agent. Verify my responses via X-Agent-Signature.",
		"identity": identity,
		"ts":       time.Now().Unix(),
	}
	if cfg != nil {
		agentInfo := map[string]any{
			"framework": cfg.Framework,
			"inference": map[string]any{
				"provider":  cfg.Inference.Provider,
				"model":     cfg.Inference.Model,
				"fallbacks": cfg.Inference.Fallbacks,
			},
		}
		if cfg.Persona.SystemPrompt != "" {
			agentInfo["persona"] = cfg.Persona.SystemPrompt
		}
		if len(cfg.Skills) > 0 {
			agentInfo["skills"] = cfg.Skills
		}
		resp["agent"] = agentInfo
	}

	bodyBytes, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "marshal: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := writeSignedResponse(w, r, priv, nil, bodyBytes, http.StatusOK); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// isWebSocketUpgrade reports whether r is a WebSocket upgrade request.
// Per RFC 6455, both Connection: upgrade and Upgrade: websocket must be set
// (case-insensitive). Connection may be a comma-separated list.
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, tok := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
			return true
		}
	}
	return false
}

// wsReverseProxy returns a transparent reverse proxy targeting upstream.
// Go 1.20+ http.httputil handles the WebSocket protocol switch automatically.
//
// We rewrite a few headers before the request hits openclaw:
//
//   - Origin → "http://<upstream-host>" so openclaw's controlUi origin check
//     accepts the connection (the real Origin from the browser is the public
//     proxy URL, which openclaw cannot enumerate ahead of time).
//   - X-Forwarded-For/Proto/Host and X-Real-Ip dropped: openclaw treats their
//     presence as evidence of an untrusted upstream proxy and rejects the
//     connection as non-local. Since we are the trust boundary, we strip them.
//
// The bootstrap signing layer on :8080 is what binds responses to the agent;
// we don't need openclaw's own auth or origin tracking once traffic is inside
// the container.
func wsReverseProxy(upstream string) *httputil.ReverseProxy {
	target, err := url.Parse(upstream)
	if err != nil {
		return &httputil.ReverseProxy{
			Director: func(req *http.Request) {},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "bad upstream URL: "+err.Error(), http.StatusInternalServerError)
			},
		}
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Header.Set("Origin", "http://"+target.Host)
			// Setting to nil tells httputil.ReverseProxy to skip its default
			// X-Forwarded-For appending.
			pr.Out.Header["X-Forwarded-For"] = nil
			pr.Out.Header.Del("X-Forwarded-Proto")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Real-Ip")
			// gateway.auth.mode=trusted-proxy: openclaw expects the user
			// identity in this header. Any external client connecting via
			// :8080 is treated as the same fixed agent operator — finer
			// per-caller identity isn't relevant inside the sealed sandbox.
			pr.Out.Header.Set("X-Sealed-Agent-User", "agent-operator")
		},
	}
}

// agentProxyHandler reverse-proxies anything not handled by the fixed paths
// (/dashboard, /healthz, /hello) to the agent upstream. Returns 503 until the
// agent is armed (post-bootstrap).
//
// Signing scheme: the response body is buffered (no streaming in v1), then a
// secp256k1 signature over keccak256("AgentResponse:0x<reqHash>:0x<respHash>:<ts>")
// is computed with agent_seal_priv (V normalised to 27/28). reqHash binds the
// inbound method + URI + body; respHash binds the outbound body.
//
// Headers attached:
//
//	X-Agent-Signature      0x<65-byte hex>
//	X-Agent-Timestamp      unix seconds
//	X-Agent-Request-Hash   0x<32-byte hex>  (so verifiers don't need to recompute)
//	X-Agent-Response-Hash  0x<32-byte hex>
func agentProxyHandler(w http.ResponseWriter, r *http.Request) {
	priv, upstream, _, _ := agent.snapshot()
	if priv == nil || upstream == "" {
		http.Error(w, "agent not ready", http.StatusServiceUnavailable)
		return
	}

	// WebSocket upgrade requests cannot be buffered + signed: there's no
	// single response body, only a long-lived bidirectional frame stream.
	// Detect the upgrade and hand it off to httputil.ReverseProxy, which
	// natively handles the protocol switch (Go 1.20+).
	//
	// Trade-off: WS frames are NOT signed in v1. Callers that need
	// verifiable responses must use HTTP API endpoints (which still go
	// through the signing branch below). openclaw's dashboard uses WS, so
	// the dashboard works but its frames inherit the trust of the TLS/proxy
	// chain only — not the agent_seal_priv signature.
	if isWebSocketUpgrade(r) {
		wsReverseProxy(upstream).ServeHTTP(w, r)
		return
	}

	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.Body.Close()

	upstreamURL := upstream + r.URL.RequestURI()
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	upHost := ""
	if u, perr := url.Parse(upstream); perr == nil {
		upHost = u.Host
	}
	for k, vs := range r.Header {
		// Hop-by-hop headers — drop. Net/http handles Connection itself.
		// X-Forwarded-* and Origin are rewritten below so openclaw treats the
		// request as a local-loopback caller (see wsReverseProxy comment).
		switch k {
		case "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Proxy-Authorization", "Proxy-Authenticate":
			continue
		case "Origin", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-Ip":
			continue
		}
		for _, v := range vs {
			upReq.Header.Add(k, v)
		}
	}
	if upHost != "" {
		upReq.Header.Set("Origin", "http://"+upHost)
	}
	// gateway.auth.mode=trusted-proxy expects the user identity in this
	// header (see wsReverseProxy comment).
	upReq.Header.Set("X-Sealed-Agent-User", "agent-operator")

	resp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "read upstream body: "+err.Error(), http.StatusBadGateway)
		return
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if err := writeSignedResponse(w, r, priv, reqBody, respBody, resp.StatusCode); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// writeSignedResponse computes the AgentResponse signature for (reqBody, body),
// attaches the four X-Agent-* headers to w, and writes body with statusCode.
// Caller is responsible for any upstream-derived headers having already been
// copied into w.Header() (this function only touches the X-Agent-* set and
// Content-Length).
func writeSignedResponse(w http.ResponseWriter, r *http.Request, priv, reqBody, body []byte, statusCode int) error {
	ts := time.Now().Unix()
	reqHash := crypto.Keccak256(
		[]byte(r.Method),
		[]byte("\n"),
		[]byte(r.URL.RequestURI()),
		[]byte("\n"),
		reqBody,
	)
	respHash := crypto.Keccak256(body)
	canonical := fmt.Sprintf("AgentResponse:0x%s:0x%s:%d",
		hex.EncodeToString(reqHash), hex.EncodeToString(respHash), ts)
	msgHash := crypto.Keccak256([]byte(canonical))

	privKey, err := crypto.ToECDSA(priv)
	if err != nil {
		return fmt.Errorf("agent priv: %w", err)
	}
	sig, err := crypto.Sign(msgHash, privKey)
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	sig[64] += 27

	w.Header().Set("X-Agent-Signature", "0x"+hex.EncodeToString(sig))
	w.Header().Set("X-Agent-Timestamp", fmt.Sprintf("%d", ts))
	w.Header().Set("X-Agent-Request-Hash", "0x"+hex.EncodeToString(reqHash))
	w.Header().Set("X-Agent-Response-Hash", "0x"+hex.EncodeToString(respHash))
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))

	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
	return nil
}

// ── Attestor: /provision ────────────────────────────────────────────────────

func provisionFromAttestor(attestorURL string, sealKeyBytes []byte, a attestation) []byte {
	imageHashHex := strings.TrimPrefix(a.ImageHash, "sha256:")
	reqBody, _ := json.Marshal(map[string]any{
		"seal_id":           "0x" + a.SealID,
		"container_pubkey":  a.Pubkey,
		"image_hash":        "0x" + imageHashHex,
		"issued_at":         a.Ts,
		"sandbox_signature": a.Signature,
	})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(attestorURL+"/provision", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		logf("FAIL provision: POST error: %v", err)
		return nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logf("FAIL provision: HTTP %d: %s", resp.StatusCode, string(body))
		return nil
	}
	var out struct {
		EncryptedAgentSealPriv string `json:"encrypted_agent_seal_priv"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		logf("FAIL provision: decode response: %v", err)
		return nil
	}
	if out.EncryptedAgentSealPriv == "" {
		logf("FAIL provision: empty encrypted_agent_seal_priv")
		return nil
	}
	ctBytes, err := hex.DecodeString(strings.TrimPrefix(out.EncryptedAgentSealPriv, "0x"))
	if err != nil {
		logf("FAIL provision: decode ciphertext hex: %v", err)
		return nil
	}
	priv := eciesgo.NewPrivateKeyFromBytes(sealKeyBytes)
	plaintext, err := eciesgo.Decrypt(priv, ctBytes)
	if err != nil {
		logf("FAIL provision: ECIES decrypt: %v", err)
		return nil
	}
	// Redact the actual key — only confirm the size + derived address, never the priv bytes.
	if len(plaintext) > 0 {
		if priv, err := crypto.ToECDSA(plaintext); err == nil {
			logf("OK   provisioned agent_seal_priv (%d bytes), addr=%s",
				len(plaintext), crypto.PubkeyToAddress(priv.PublicKey).Hex())
		} else {
			logf("OK   provisioned agent_seal_priv (%d bytes)", len(plaintext))
		}
	}
	return plaintext
}

// ── Attestor: /status ───────────────────────────────────────────────────────

// Canonical message: "StatusReport:<seal_id_0x>:<status>:<error_detail>"
// hashed with raw keccak256 (no EIP-191), V=27/28. Signed by agent_seal_priv.
func reportStatus(attestorURL string, agentSealPriv []byte, sealID, status, errorDetail string) {
	msg := fmt.Sprintf("StatusReport:0x%s:%s:%s", sealID, status, errorDetail)
	hash := crypto.Keccak256([]byte(msg))
	priv, err := crypto.ToECDSA(agentSealPriv)
	if err != nil {
		logf("FAIL status: parse agent priv: %v", err)
		return
	}
	sig, err := crypto.Sign(hash, priv)
	if err != nil {
		logf("FAIL status: sign: %v", err)
		return
	}
	sig[64] += 27

	reqBody, _ := json.Marshal(map[string]any{
		"seal_id":              "0x" + sealID,
		"status":               status,
		"error_detail":         errorDetail,
		"agent_seal_signature": "0x" + hex.EncodeToString(sig),
	})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(attestorURL+"/status", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		logf("FAIL status: POST error: %v", err)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logf("FAIL status: HTTP %d: %s", resp.StatusCode, string(respBody))
		return
	}
	logf("OK   status reported: %s", status)
}

// ── Bootstrap: AgenticID watcher + storage download + decrypt ───────────────

// bootstrap returns the decrypted entries (role + plaintext) only when every
// phase succeeds (mint observed, i_data list fetched, every entry downloaded
// AND decrypted). Any failure returns nil; details are logged.
//
// fallbackIndexer is used when an i_data's dataDescription does not contain
// storage_ptr.indexer. Empty string disables the fallback.
func bootstrap(rpcURL, contractHex, sealIDHex string, agentSealPriv []byte, fallbackIndexer string) []decryptedEntry {
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer cancel()

	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		logf("FAIL bootstrap: dial RPC: %v", err)
		return nil
	}
	defer client.Close()

	parsedABI, err := abi.JSON(strings.NewReader(agenticIDABI))
	if err != nil {
		logf("FAIL bootstrap: parse ABI: %v", err)
		return nil
	}
	contract := common.HexToAddress(contractHex)

	sealIDBytes, err := hex.DecodeString(strings.TrimPrefix(sealIDHex, "0x"))
	if err != nil || len(sealIDBytes) != 32 {
		logf("FAIL bootstrap: seal_id must be 32 bytes hex: %v", err)
		return nil
	}
	var sealID32 [32]byte
	copy(sealID32[:], sealIDBytes)

	// Phase 1: poll getAgentIdBySealId(sealId) until non-zero.
	agentID, err := waitForMint(ctx, client, parsedABI, contract, sealID32)
	if err != nil {
		logf("FAIL bootstrap: wait for mint: %v", err)
		return nil
	}
	logf("OK   minted agent_id: %s", agentID.String())

	// Phase 2: list i_data (descriptions + dataHashes; sealedKeys live on ITransferred)
	iDatas, err := intelligentDatasOf(ctx, client, parsedABI, contract, agentID)
	if err != nil {
		logf("FAIL bootstrap: intelligentDatasOf: %v", err)
		return nil
	}
	logf("OK   intelligent_datas: %d entries", len(iDatas))

	// Phase 2b: backward-scan ITransferred for tokenId until we find the most
	// recent entry (mint or last transfer = current sealedKeys).
	sealedKeys, err := loadSealedKeys(ctx, client, parsedABI, contract, agentID)
	if err != nil {
		logf("FAIL bootstrap: load sealedKeys from ITransferred: %v", err)
		return nil
	}
	logf("OK   sealedKeys collected: %d entries", len(sealedKeys))

	// Phase 3 + 4: per-entry download + decrypt
	entries := make([]decryptedEntry, 0, len(iDatas))
	allOK := true
	for i, d := range iDatas {
		sealed, ok := sealedKeys[d.DataHash]
		if !ok {
			logf("FAIL bootstrap[%d]: no sealedKey for dataHash 0x%s", i, hex.EncodeToString(d.DataHash[:]))
			allOK = false
			continue
		}
		entry, ok := processIntelligentData(ctx, i, d, sealed, agentSealPriv, fallbackIndexer)
		if !ok {
			allOK = false
			continue
		}
		entries = append(entries, entry)
	}
	if !allOK {
		return nil
	}
	logf("OK   bootstrap complete")
	return entries
}

// waitForMint polls getAgentIdBySealId(sealId) until non-zero (or ctx done).
func waitForMint(ctx context.Context, client *ethclient.Client, parsedABI abi.ABI, contract common.Address, sealID32 [32]byte) (*big.Int, error) {
	if id, err := getAgentIdBySealId(ctx, client, parsedABI, contract, sealID32); err == nil && id.Sign() > 0 {
		return id, nil
	}
	logf("waiting for mint (poll every %s)...", mintPollEvery)
	ticker := time.NewTicker(mintPollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		if id, err := getAgentIdBySealId(ctx, client, parsedABI, contract, sealID32); err == nil && id.Sign() > 0 {
			return id, nil
		}
	}
}

func getAgentIdBySealId(ctx context.Context, client *ethclient.Client, parsedABI abi.ABI, contract common.Address, sealID32 [32]byte) (*big.Int, error) {
	data, err := parsedABI.Pack("getAgentIdBySealId", sealID32)
	if err != nil {
		return nil, err
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &contract, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	res, err := parsedABI.Unpack("getAgentIdBySealId", out)
	if err != nil || len(res) == 0 {
		return nil, fmt.Errorf("unpack")
	}
	return res[0].(*big.Int), nil
}

// loadSealedKeys finds the most recent ITransferred for tokenId.
//
// Two phases:
//
//  1. Forward poll on the head window. Some RPCs are load-balanced across
//     nodes with slightly different sync states — eth_call against state node
//     may show a freshly-minted agent_id while eth_blockNumber against another
//     node is still N blocks behind, so logs from the mint block are
//     temporarily invisible. Re-fetch latest every few seconds and retry
//     the head window for up to pollTimeout before giving up.
//
//  2. Backward chunked scan. If the log isn't on the head, the agent was
//     minted long ago — walk backwards in transferScanChunk windows.
func loadSealedKeys(ctx context.Context, client *ethclient.Client, parsedABI abi.ABI, contract common.Address, tokenID *big.Int) (map[[32]byte][]byte, error) {
	event, ok := parsedABI.Events["ITransferred"]
	if !ok {
		return nil, fmt.Errorf("ITransferred not in ABI")
	}
	tokenTopic := common.BigToHash(tokenID)
	logf("ITransferred scan: tokenId=%s topic[3]=%s", tokenID.String(), tokenTopic.Hex())

	const (
		pollTimeout  = 30 * time.Second
		pollInterval = 3 * time.Second
	)

	tryHead := func(latest uint64) (map[[32]byte][]byte, error) {
		var from uint64
		if latest >= transferScanChunk {
			from = latest - transferScanChunk + 1
		}
		q := ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(from),
			ToBlock:   new(big.Int).SetUint64(latest),
			Addresses: []common.Address{contract},
			Topics:    [][]common.Hash{{event.ID}, nil, nil, {tokenTopic}},
		}
		logs, err := client.FilterLogs(ctx, q)
		if err != nil {
			return nil, err
		}
		if len(logs) == 0 {
			return nil, nil
		}
		lg := logs[len(logs)-1]
		var ev struct {
			Entries []sealedKeyEntry
		}
		if err := parsedABI.UnpackIntoInterface(&ev, "ITransferred", lg.Data); err != nil {
			return nil, fmt.Errorf("decode ITransferred log: %w", err)
		}
		out := map[[32]byte][]byte{}
		for _, e := range ev.Entries {
			out[e.DataHash] = e.SealedKey
		}
		logf("ITransferred found at block %d (head)", lg.BlockNumber)
		return out, nil
	}

	// Phase 1: poll the head window, re-fetching latest each iteration.
	deadline := time.Now().Add(pollTimeout)
	var latest uint64
	for {
		latestNew, err := client.BlockNumber(ctx)
		if err != nil {
			return nil, fmt.Errorf("BlockNumber: %w", err)
		}
		if latestNew != latest {
			logf("ITransferred poll: trying head [%d..%d]", latestNew-transferScanChunk+1, latestNew)
			latest = latestNew
			result, err := tryHead(latest)
			if err != nil {
				return nil, err
			}
			if result != nil {
				return result, nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	logf("ITransferred head poll exhausted; falling back to backward scan from %d", latest)

	// Phase 2: chunked backward scan.
	to := latest
	if to >= transferScanChunk {
		to -= transferScanChunk // already covered by Phase 1
	} else {
		return nil, fmt.Errorf("no ITransferred for tokenId %s within head window", tokenID)
	}
	chunks := 0
	for {
		var from uint64
		if to >= transferScanChunk {
			from = to - transferScanChunk + 1
		}
		q := ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(from),
			ToBlock:   new(big.Int).SetUint64(to),
			Addresses: []common.Address{contract},
			Topics:    [][]common.Hash{{event.ID}, nil, nil, {tokenTopic}},
		}
		logs, err := client.FilterLogs(ctx, q)
		chunks++
		if err != nil {
			return nil, fmt.Errorf("FilterLogs [%d..%d] (chunk %d): %w", from, to, chunks, err)
		}
		if len(logs) > 0 {
			lg := logs[len(logs)-1]
			var ev struct {
				Entries []sealedKeyEntry
			}
			if err := parsedABI.UnpackIntoInterface(&ev, "ITransferred", lg.Data); err != nil {
				return nil, fmt.Errorf("decode ITransferred log: %w", err)
			}
			result := map[[32]byte][]byte{}
			for _, e := range ev.Entries {
				result[e.DataHash] = e.SealedKey
			}
			logf("ITransferred found at block %d (chunk %d)", lg.BlockNumber, chunks)
			return result, nil
		}
		if chunks%10 == 0 {
			logf("ITransferred scan: %d chunks searched, currently at [%d..%d]", chunks, from, to)
		}
		if from == 0 {
			return nil, fmt.Errorf("no ITransferred for tokenId %s in chain history (%d chunks scanned)", tokenID, chunks)
		}
		to = from - 1
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
}

func intelligentDatasOf(ctx context.Context, client *ethclient.Client, parsedABI abi.ABI, contract common.Address, agentID *big.Int) ([]intelligentData, error) {
	data, err := parsedABI.Pack("intelligentDatasOf", agentID)
	if err != nil {
		return nil, err
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &contract, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	var arr []intelligentData
	if err := parsedABI.UnpackIntoInterface(&arr, "intelligentDatasOf", out); err != nil {
		return nil, err
	}
	return arr, nil
}

func processIntelligentData(ctx context.Context, idx int, d intelligentData, sealedKey, agentSealPriv []byte, fallbackIndexer string) (decryptedEntry, bool) {
	tag := fmt.Sprintf("[%d]", idx)
	dataHashHex := "0x" + hex.EncodeToString(d.DataHash[:])

	var desc storageDescription
	if err := json.Unmarshal([]byte(d.DataDescription), &desc); err != nil {
		logf("FAIL bootstrap%s parse dataDescription: %v", tag, err)
		return decryptedEntry{}, false
	}
	indexer := desc.StoragePtr.Indexer
	if indexer == "" {
		indexer = fallbackIndexer
	}
	if indexer == "" {
		logf("FAIL bootstrap%s no indexer (description.storage_ptr.indexer empty and no fallback)", tag)
		return decryptedEntry{}, false
	}
	logf("bootstrap%s data=%s role=%q indexer=%s", tag, dataHashHex, desc.Role, indexer)

	outPath := fmt.Sprintf("/tmp/idata-%s.bin", hex.EncodeToString(d.DataHash[:]))
	if err := downloadWithRetry(ctx, dataHashHex, indexer, outPath); err != nil {
		logf("FAIL bootstrap%s download: %v", tag, err)
		return decryptedEntry{}, false
	}
	blob, err := os.ReadFile(outPath)
	if err != nil {
		logf("FAIL bootstrap%s read downloaded file: %v", tag, err)
		return decryptedEntry{}, false
	}

	// ECIES sealedKey -> data_key (32 bytes)
	dataKey, err := eciesgo.Decrypt(eciesgo.NewPrivateKeyFromBytes(agentSealPriv), sealedKey)
	if err != nil {
		logf("FAIL bootstrap%s ECIES decrypt sealedKey: %v", tag, err)
		return decryptedEntry{}, false
	}

	// AES-GCM-256: blob = nonce(12) || ciphertext+tag(16 at end)
	if len(blob) < 12+16 {
		logf("FAIL bootstrap%s blob too short (%d bytes)", tag, len(blob))
		return decryptedEntry{}, false
	}
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		logf("FAIL bootstrap%s AES new cipher: %v", tag, err)
		return decryptedEntry{}, false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		logf("FAIL bootstrap%s GCM init: %v", tag, err)
		return decryptedEntry{}, false
	}
	plaintext, err := gcm.Open(nil, blob[:12], blob[12:], nil)
	if err != nil {
		logf("FAIL bootstrap%s AES-GCM open: %v", tag, err)
		return decryptedEntry{}, false
	}

	// Don't dump plaintext — could contain API keys, prompts, or other secrets.
	// The dispatcher (tryStartAgent) decides what's safe to surface from each role.
	logf("OK   bootstrap%s decrypted (%d bytes, role=%q)", tag, len(plaintext), desc.Role)
	return decryptedEntry{Role: desc.Role, Plaintext: plaintext}, true
}

// ── Agent launcher (config-driven adapters) ─────────────────────────────────

// tryStartAgent finds the entry whose dataDescription.role == "config", parses
// it as the framework-agnostic agentConfig schema, and dispatches to the
// matching adapter (openclaw, etc.). On success the catch-all reverse proxy is
// armed by populating agentState.
func tryStartAgent(entries []decryptedEntry, agentSealPriv []byte, apiKey, sealID string) {
	var cfgRaw []byte
	for _, e := range entries {
		if e.Role == "config" {
			cfgRaw = e.Plaintext
			break
		}
	}
	if cfgRaw == nil {
		logf("no entry with role=\"config\"; agent will not start")
		return
	}
	var cfg agentConfig
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		logf("FAIL agent: parse config: %v", err)
		return
	}
	logf("agent config: framework=%s/%s inference=%s/%s",
		cfg.Framework.Name, cfg.Framework.Version, cfg.Inference.Provider, cfg.Inference.Model)

	switch cfg.Framework.Name {
	case "openclaw":
		if err := startOpenclaw(cfg, apiKey); err != nil {
			logf("FAIL agent (openclaw): %v", err)
			return
		}
		agent.set(agentSealPriv, "http://127.0.0.1:3284", sealID, &cfg)
		logf("OK   agent proxy armed -> openclaw on :3284")
	default:
		logf("FAIL agent: unsupported framework %q", cfg.Framework.Name)
	}
}

// startOpenclaw translates agentConfig into ~/.openclaw/openclaw.json, sets the
// inference provider's API key from the API_KEY env var, and spawns
// `openclaw gateway run --bind lan --port 3284` in the background.
func startOpenclaw(cfg agentConfig, apiKey string) error {
	provider := cfg.Inference.Provider
	if provider == "" {
		return fmt.Errorf("inference.provider missing")
	}
	if cfg.Inference.Model == "" {
		return fmt.Errorf("inference.model missing")
	}

	// openclaw's schema (as of v2026.3.8) only recognises `agents.defaults.model`
	// at the top of the agents tree — `systemPrompt` lives on *channel* configs
	// (whatsapp/telegram/discord), there is no agent-wide system prompt slot.
	// For gateway-only deployments we have nowhere clean to put it, so we
	// surface it in the log for visibility and skip writing it to the file.
	if cfg.Persona.SystemPrompt != "" {
		logf("note: persona.system_prompt provided (%d chars) but openclaw has "+
			"no gateway-level slot for it; ignoring in openclaw.json",
			len(cfg.Persona.SystemPrompt))
	}
	// gateway.auth.mode = "none": disable openclaw's own gateway auth. The
	// outer reverse proxy on :8080 is the trust boundary — every response is
	// signed with agent_seal_priv so callers verify provenance via signature,
	// not openclaw's per-request token.
	//
	// gateway.controlUi.dangerouslyAllowHostHeaderOriginFallback = true: the
	// upstream openclaw image sets this at build time, but we overwrite the
	// whole openclaw.json so it has to be re-emitted here. Sandbox containers
	// have no loopback UI access from the host, so the control UI needs the
	// host-header origin fallback to render at all.
	modelObj := map[string]any{
		"primary": provider + "/" + cfg.Inference.Model,
	}
	if len(cfg.Inference.Fallbacks) > 0 {
		fbs := make([]string, len(cfg.Inference.Fallbacks))
		for i, f := range cfg.Inference.Fallbacks {
			fbs[i] = provider + "/" + f
		}
		modelObj["fallbacks"] = fbs
	}
	// gateway.controlUi flags worth understanding:
	//   dangerouslyAllowHostHeaderOriginFallback: skip exact origin
	//     allowlisting; Origin must equal Host. Our reverse proxy rewrites
	//     both to "http://127.0.0.1:3284" so this match always holds.
	//   dangerouslyDisableDeviceAuth: skip the per-device identity check
	//     openclaw normally requires for the control UI. The browser is
	//     reaching us over plain HTTP via a public proxy URL (not a secure
	//     context), which would otherwise be refused.
	//   allowInsecureAuth: allow auth flows over non-HTTPS connections.
	// External traffic is gated by the bootstrap signing layer on :8080,
	// so loosening openclaw's own controls is acceptable here.
	// gateway.auth.mode = "trusted-proxy": openclaw treats requests as already
	// authenticated when they carry the configured userHeader, and the
	// connection comes from a trusted proxy IP. The bootstrap reverse proxy
	// on :8080 IS that trusted proxy — it sits inside the same container
	// (loopback, always trusted) and injects the user header on every
	// forwarded request. Combined with dangerouslyDisableDeviceAuth, this is
	// what unblocks the control UI WebSocket handshake without browser-side
	// device pairing (which would require HTTPS or localhost).
	//
	// auth.mode=none was tried first but openclaw's control UI hard-requires
	// either a real device identity, trusted-proxy auth, or sharedAuthOk
	// (token/password). trusted-proxy is the cleanest fit for our topology.
	occ := map[string]any{
		"gateway": map[string]any{
			"auth": map[string]any{
				"mode": "trusted-proxy",
				"trustedProxy": map[string]any{
					"userHeader": "X-Sealed-Agent-User",
				},
			},
			"trustedProxies": []string{"127.0.0.1", "::1"},
			"controlUi": map[string]any{
				"dangerouslyAllowHostHeaderOriginFallback": true,
				"dangerouslyDisableDeviceAuth":             true,
				"allowInsecureAuth":                        true,
			},
		},
		"agents": map[string]any{
			"defaults": map[string]any{
				"model": modelObj,
			},
		},
		"auth": map[string]any{
			"profiles": map[string]any{
				provider + ":api": map[string]any{
					"provider": provider,
					"mode":     "api_key",
				},
			},
			"order": map[string]any{
				provider: []string{provider + ":api"},
			},
		},
	}
	occJSON, err := json.MarshalIndent(occ, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal openclaw.json: %w", err)
	}
	if err := os.MkdirAll("/root/.openclaw", 0o755); err != nil {
		return fmt.Errorf("mkdir /root/.openclaw: %w", err)
	}
	if err := os.WriteFile("/root/.openclaw/openclaw.json", occJSON, 0o600); err != nil {
		return fmt.Errorf("write openclaw.json: %w", err)
	}
	logf("OK   wrote /root/.openclaw/openclaw.json (%d bytes)", len(occJSON))

	// Map API_KEY env -> provider-specific env var openclaw expects.
	if apiKey != "" {
		envName := ""
		switch provider {
		case "anthropic":
			envName = "ANTHROPIC_API_KEY"
		case "openai":
			envName = "OPENAI_API_KEY"
		}
		if envName != "" {
			if err := os.Setenv(envName, apiKey); err != nil {
				return fmt.Errorf("set %s: %w", envName, err)
			}
			logf("OK   exported %s from API_KEY", envName)
		}
	}

	// `openclaw config set gateway.mode local` (idempotent).
	if out, err := exec.Command("openclaw", "config", "set", "gateway.mode", "local").CombinedOutput(); err != nil {
		return fmt.Errorf("openclaw config set: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// Spawn gateway in background, capturing stdout/stderr to /tmp/openclaw.log.
	logFile, err := os.OpenFile("/tmp/openclaw.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open openclaw.log: %w", err)
	}
	// --bind loopback: openclaw refuses to start on a non-loopback bind when
	// gateway.auth.mode=none ("Refusing to bind gateway to lan without auth").
	// That's exactly the topology we want anyway — openclaw only listens on
	// 127.0.0.1, and the outer reverse proxy on :8080 (which signs every
	// response) is the real trust boundary for external traffic.
	//
	// --allow-unconfigured: required when agents.list is empty (we configure
	// only agents.defaults.model, no explicit agent entries).
	cmd := exec.Command("openclaw", "gateway", "run", "--allow-unconfigured", "--bind", "loopback", "--port", "3284")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start openclaw gateway: %w", err)
	}
	logf("OK   openclaw gateway spawned, pid=%d (log: /tmp/openclaw.log)", cmd.Process.Pid)

	go func() {
		err := cmd.Wait()
		logFile.Close()
		// Reset agent state so the proxy stops accepting requests.
		agent.set(nil, "", "", nil)
		if err != nil {
			logf("openclaw gateway exited: %v", err)
		} else {
			logf("openclaw gateway exited cleanly")
		}
	}()
	return nil
}

func downloadWithRetry(ctx context.Context, root, indexer, outPath string) error {
	var lastErr error
	for i := 0; i < downloadAttempts; i++ {
		if i > 0 {
			delay := 1 << uint(i)
			if delay > 60 {
				delay = 60
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(delay) * time.Second):
			}
		}
		// 0g-storage-client refuses to overwrite; remove leftover from a
		// previous partial / failed download (or container restart) first.
		_ = os.Remove(outPath)
		cmd := exec.CommandContext(ctx, "0g-storage-client", "download",
			"--root", root, "--file", outPath, "--indexer", indexer)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("attempt %d: %v: %s", i+1, err, strings.TrimSpace(string(out)))
	}
	return lastErr
}

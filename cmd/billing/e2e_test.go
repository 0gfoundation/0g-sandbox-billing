package main

// TestE2E_HTTPToChainSettlement is a full end-to-end test that exercises the
// complete request pipeline:
//
//  1. Deploys the SandboxServing beacon-proxy stack on an in-process simulated EVM.
//  2. Starts the full HTTP server (EIP-191 auth middleware + proxy handler) backed
//     by a mock Daytona server.
//  3. Sends an authenticated POST /api/sandbox; billing.OnCreate enqueues a
//     create-fee voucher into Redis.
//  4. Runs the settler loop; it settles the voucher on-chain.
//  5. Asserts that the on-chain lastNonce advanced to 1.
//
// The test skips gracefully when the compiled contract artifacts are absent
// (run `make build-contracts` to produce them).

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/0gfoundation/0g-sandbox-billing/internal/auth"
	"github.com/0gfoundation/0g-sandbox-billing/internal/billing"
	"github.com/0gfoundation/0g-sandbox-billing/internal/chain"
	"github.com/0gfoundation/0g-sandbox-billing/internal/config"
	"github.com/0gfoundation/0g-sandbox-billing/internal/daytona"
	"github.com/0gfoundation/0g-sandbox-billing/internal/proxy"
	"github.com/0gfoundation/0g-sandbox-billing/internal/settler"
	"github.com/0gfoundation/0g-sandbox-billing/internal/voucher"
)

// ── keys (Anvil default accounts, same as chain_integration_test) ─────────────

var (
	e2eProviderKeyHex = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	e2eUserKeyHex     = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	e2eChainID        = big.NewInt(1337)
)

// ── simChainClient ────────────────────────────────────────────────────────────

// simChainClient implements settler.ChainClient using a go-ethereum simulated
// backend. It submits the tx, commits a block, then returns statuses.
type simChainClient struct {
	contract     *chain.SandboxServing
	providerAuth *bind.TransactOpts
	simClient    simulated.Client
	backend      *simulated.Backend
}

func (c *simChainClient) SettleFeesWithTEE(ctx context.Context, vs []voucher.SandboxVoucher) ([]chain.SettlementStatus, error) {
	cvs := make([]chain.SandboxServingSandboxVoucher, len(vs))
	for i, v := range vs {
		cvs[i] = chain.SandboxServingSandboxVoucher{
			User: v.User, Provider: v.Provider,
			TotalFee: v.TotalFee, UsageHash: v.UsageHash,
			Nonce: v.Nonce, Signature: v.Signature,
		}
	}

	opts := *c.providerAuth
	opts.Context = ctx
	tx, err := c.contract.SettleFeesWithTEE(&opts, cvs)
	if err != nil {
		return nil, fmt.Errorf("SettleFeesWithTEE tx: %w", err)
	}
	c.backend.Commit()

	receipt, err := c.simClient.TransactionReceipt(ctx, tx.Hash())
	if err != nil {
		return nil, fmt.Errorf("get receipt: %w", err)
	}
	if receipt.Status == 0 {
		return nil, fmt.Errorf("settlement tx reverted")
	}

	// Tx succeeded: report StatusSuccess for each voucher in the batch.
	statuses := make([]chain.SettlementStatus, len(vs))
	// all zeros = StatusSuccess
	return statuses, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// e2eLoadArtifact reads a Foundry JSON artifact and returns (bytecode, parsedABI).
// Calls t.Skip if the artifact is not present (contracts not yet compiled).
func e2eLoadArtifact(t *testing.T, relPath, abiStr string) ([]byte, abi.ABI) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	fullPath := filepath.Join(filepath.Dir(thisFile), "..", "..", relPath)
	raw, err := os.ReadFile(fullPath)
	if err != nil {
		t.Skipf("artifact not found (run `make build-contracts`): %v", err)
	}
	var artifact struct {
		Bytecode struct{ Object string `json:"object"` } `json:"bytecode"`
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("parse artifact %s: %v", relPath, err)
	}
	hexStr := strings.TrimPrefix(artifact.Bytecode.Object, "0x")
	bytecode, err := hex.DecodeString(hexStr)
	if err != nil {
		t.Fatalf("decode bytecode %s: %v", relPath, err)
	}
	parsedABI, err := abi.JSON(strings.NewReader(abiStr))
	if err != nil {
		t.Fatalf("parse ABI %s: %v", relPath, err)
	}
	return bytecode, parsedABI
}

// e2eSignedHeaders builds EIP-191 auth headers for the given private key.
// Returns (X-Wallet-Address, X-Signed-Message, X-Wallet-Signature) values.
func e2eSignedHeaders(t *testing.T, privKeyHex string) (walletAddr, msgB64, sigHex string) {
	t.Helper()
	privKey, err := crypto.HexToECDSA(privKeyHex)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	walletAddr = crypto.PubkeyToAddress(privKey.PublicKey).Hex()

	req := auth.SignedRequest{
		Action:    "create",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
		Nonce:     fmt.Sprintf("e2e-%d", time.Now().UnixNano()),
	}
	msgBytes, _ := json.Marshal(req)
	hash := auth.HashMessage(msgBytes)
	sig, err := crypto.Sign(hash, privKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig[64] += 27 // normalize V to 27/28 (Ethereum convention)

	return walletAddr,
		base64.StdEncoding.EncodeToString(msgBytes),
		"0x" + hex.EncodeToString(sig)
}

// waitQueueLen polls rdb until the list at key has ≥ n items, or the timeout elapses.
func waitQueueLen(t *testing.T, rdb *redis.Client, key string, n int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		l, _ := rdb.LLen(context.Background(), key).Result()
		if l >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("queue %q did not reach length %d within %v", key, n, timeout)
}

// waitNonce polls the contract until GetLastNonce(user,provider) reaches want,
// or the deadline elapses.
func waitNonce(t *testing.T, contract *chain.SandboxServing, user, provider common.Address, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, err := contract.GetLastNonce(&bind.CallOpts{}, user, provider)
		if err != nil {
			t.Fatalf("GetLastNonce: %v", err)
		}
		if n.Int64() == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("lastNonce did not reach %d within %v", want, timeout)
}

// ── E2E test ──────────────────────────────────────────────────────────────────

func TestE2E_HTTPToChainSettlement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ── 1. Keys ──────────────────────────────────────────────────────────────
	providerKey, _ := crypto.HexToECDSA(e2eProviderKeyHex)
	userKey, _ := crypto.HexToECDSA(e2eUserKeyHex)
	providerAddr := crypto.PubkeyToAddress(providerKey.PublicKey)
	userAddr := crypto.PubkeyToAddress(userKey.PublicKey)

	// ── 2. Simulated chain ────────────────────────────────────────────────────
	balance, _ := new(big.Int).SetString("1000000000000000000000", 10) // 1000 ETH
	alloc := types.GenesisAlloc{
		providerAddr: {Balance: balance},
		userAddr:     {Balance: balance},
	}
	backend := simulated.NewBackend(alloc, simulated.WithBlockGasLimit(30_000_000))
	defer backend.Close()
	simClient := backend.Client()

	providerAuth, _ := bind.NewKeyedTransactorWithChainID(providerKey, e2eChainID)
	userAuth, _ := bind.NewKeyedTransactorWithChainID(userKey, e2eChainID)

	// Deploy SandboxServing implementation (no constructor args)
	implBytecode, implABI := e2eLoadArtifact(t,
		"contracts/out/SandboxServing.sol/SandboxServing.json",
		chain.SandboxServingMetaData.ABI)
	providerAuth.GasLimit = 5_000_000
	implAddr, _, _, err := bind.DeployContract(providerAuth, implABI, implBytecode, simClient)
	if err != nil {
		t.Fatalf("deploy impl: %v", err)
	}
	backend.Commit()

	// Deploy UpgradeableBeacon(impl, providerAddr)
	beaconBytecode, beaconABI := e2eLoadArtifact(t,
		"contracts/out/UpgradeableBeacon.sol/UpgradeableBeacon.json",
		chain.UpgradeableBeaconMetaData.ABI)
	providerAuth.GasLimit = 3_000_000
	beaconAddr, _, _, err := bind.DeployContract(providerAuth, beaconABI, beaconBytecode, simClient,
		implAddr, providerAddr)
	if err != nil {
		t.Fatalf("deploy beacon: %v", err)
	}
	backend.Commit()

	// Deploy BeaconProxy(beacon, initialize(0))
	proxyBytecode, proxyCtorABI := e2eLoadArtifact(t,
		"contracts/out/BeaconProxy.sol/BeaconProxy.json",
		`[{"type":"constructor","inputs":[{"name":"beacon","type":"address"},{"name":"data","type":"bytes"}],"stateMutability":"payable"}]`)
	initCalldata, err := implABI.Pack("initialize", big.NewInt(0))
	if err != nil {
		t.Fatalf("pack initialize: %v", err)
	}
	providerAuth.GasLimit = 5_000_000
	proxyAddr, _, _, err := bind.DeployContract(providerAuth, proxyCtorABI, proxyBytecode, simClient,
		beaconAddr, initCalldata)
	if err != nil {
		t.Fatalf("deploy proxy: %v", err)
	}
	backend.Commit()
	providerAuth.GasLimit = 0

	// Bind SandboxServing ABI to the proxy address
	contract, err := chain.NewSandboxServing(proxyAddr, simClient)
	if err != nil {
		t.Fatalf("bind contract: %v", err)
	}

	// Provider registers service (TEE signer == providerAddr)
	_, err = contract.AddOrUpdateService(providerAuth, "https://provider.test",
		providerAddr, big.NewInt(100), big.NewInt(0))
	if err != nil {
		t.Fatalf("addOrUpdateService: %v", err)
	}
	backend.Commit()

	// User deposits 10 ETH
	userAuth.Value, _ = new(big.Int).SetString("10000000000000000000", 10)
	_, err = contract.Deposit(userAuth, userAddr)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	backend.Commit()
	userAuth.Value = big.NewInt(0)

	// User acknowledges providerAddr as TEE signer
	_, err = contract.AcknowledgeTEESigner(userAuth, providerAddr, true)
	if err != nil {
		t.Fatalf("acknowledgeTEESigner: %v", err)
	}
	backend.Commit()

	// ── 3. miniredis ──────────────────────────────────────────────────────────
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	// ── 4. Mock Daytona: POST /api/sandbox → 201 {"id":"sb-e2e-1"} ───────────
	const sandboxID = "sb-e2e-1"
	mockDtona := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/sandbox" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%q}`, sandboxID)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockDtona.Close()

	// ── 5. Billing components ─────────────────────────────────────────────────
	// createFee=100 wei so OnCreate generates a non-trivial voucher.
	createFee := big.NewInt(100)
	computePrice := big.NewInt(0)
	log := zap.NewNop()

	// NonceReader backed by the contract on the simulated chain.
	// We reuse simChainClient (which satisfies billing.NonceReader via GetLastNonce).
	// Actually billing.NonceReader just needs GetLastNonce — satisfy it with a thin wrapper.
	nonceReader := &e2eNonceReader{contract: contract}

	signer := billing.NewSigner(providerKey, e2eChainID, proxyAddr, providerAddr, rdb, nonceReader, log)
	bh := billing.NewEventHandler(rdb, providerAddr.Hex(), computePrice, createFee, signer, log)

	// ── 6. HTTP server ────────────────────────────────────────────────────────
	gin.SetMode(gin.TestMode)
	r := gin.New()
	dtona := daytona.NewClient(mockDtona.URL, "test-admin-key")
	api := r.Group("/api", auth.Middleware(rdb))
	proxy.NewHandler(dtona, bh, log).Register(api)
	srv := httptest.NewServer(r)
	defer srv.Close()

	// ── 7. Authenticated POST /api/sandbox ────────────────────────────────────
	walletAddr, msgB64, sigHex := e2eSignedHeaders(t, e2eUserKeyHex)

	httpReq, err := http.NewRequestWithContext(ctx,
		http.MethodPost, srv.URL+"/api/sandbox", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Wallet-Address", walletAddr)
	httpReq.Header.Set("X-Signed-Message", msgB64)
	httpReq.Header.Set("X-Wallet-Signature", sigHex)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("POST /api/sandbox: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/sandbox: got HTTP %d, want 201", resp.StatusCode)
	}

	// ── 8. Wait for OnCreate to enqueue the create-fee voucher ───────────────
	// OnCreate is called in a goroutine inside handleCreate; poll Redis.
	queueKey := fmt.Sprintf(voucher.VoucherQueueKeyFmt, providerAddr.Hex())
	waitQueueLen(t, rdb, queueKey, 1, 3*time.Second)

	// ── 9. Settler: processes the voucher and settles on-chain ────────────────
	onchain := &simChainClient{
		contract:     contract,
		providerAuth: providerAuth,
		simClient:    simClient,
		backend:      backend,
	}

	settlerCtx, settlerCancel := context.WithCancel(ctx)
	defer settlerCancel()

	stopCh := make(chan settler.StopSignal, 4)
	cfg := &config.Config{
		Chain:   config.ChainConfig{ProviderAddress: providerAddr.Hex()},
		Billing: config.BillingConfig{VoucherIntervalSec: 1},
	}
	go settler.Run(settlerCtx, cfg, rdb, onchain, stopCh, log)

	// ── 10. Assert: on-chain lastNonce == 1 ───────────────────────────────────
	waitNonce(t, contract, userAddr, providerAddr, 1, 10*time.Second)
	t.Logf("E2E settlement confirmed: lastNonce(user=%s, provider=%s) = 1", userAddr.Hex(), providerAddr.Hex())
	settlerCancel()
}

// e2eNonceReader wraps the simulated chain's SandboxServing binding to satisfy
// billing.NonceReader (GetLastNonce) without pulling in chain.Client.
type e2eNonceReader struct {
	contract *chain.SandboxServing
}

func (r *e2eNonceReader) GetLastNonce(ctx context.Context, user, provider common.Address) (*big.Int, error) {
	return r.contract.GetLastNonce(&bind.CallOpts{Context: ctx}, user, provider)
}

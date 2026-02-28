// cmd/upgrade/main.go — upgrades SandboxServing by deploying a new implementation
// and pointing the UpgradeableBeacon at it.
//
// Because all state lives in the BeaconProxy, upgrading is:
//   1. Deploy a new SandboxServing implementation (no constructor args)
//   2. Call beacon.upgradeTo(newImpl)
//   3. Verify beacon.implementation() == newImpl
//
// The proxy address is UNCHANGED — no .env update needed.
// No user re-acknowledgement required. State is fully preserved.
//
// Usage:
//   go run ./cmd/upgrade/ \
//     --rpc      https://evmrpc-testnet.0g.ai \
//     --key      0x<deployer-private-key>      \
//     --chain-id 16602                         \
//     --beacon   0x<beacon-contract-address>
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/0gfoundation/0g-sandbox-billing/internal/chain"
)

func main() {
	rpcURL    := flag.String("rpc",      "https://evmrpc-testnet.0g.ai", "EVM RPC endpoint")
	keyHex   := flag.String("key",      "", "deployer/owner private key (hex)")
	chainID  := flag.Int64("chain-id",  16602, "chain ID")
	beaconHex := flag.String("beacon",  "", "UpgradeableBeacon contract address (required)")
	flag.Parse()

	if *keyHex == "" {
		fmt.Fprintln(os.Stderr, "error: --key is required")
		os.Exit(1)
	}
	if *beaconHex == "" {
		fmt.Fprintln(os.Stderr, "error: --beacon is required")
		os.Exit(1)
	}

	// ── private key ───────────────────────────────────────────────────────────
	privKey, err := crypto.HexToECDSA(strings.TrimPrefix(*keyHex, "0x"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse key: %v\n", err)
		os.Exit(1)
	}
	deployer := crypto.PubkeyToAddress(privKey.PublicKey)
	fmt.Printf("Deployer : %s\n", deployer.Hex())
	fmt.Printf("Beacon   : %s\n", *beaconHex)

	// ── chain client ──────────────────────────────────────────────────────────
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial rpc: %v\n", err)
		os.Exit(1)
	}

	auth, err := bind.NewKeyedTransactorWithChainID(privKey, big.NewInt(*chainID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "transactor: %v\n", err)
		os.Exit(1)
	}
	auth.Context = ctx

	// ── Step 1: Deploy new SandboxServing implementation ──────────────────────
	fmt.Printf("\n[1/2] Deploying new SandboxServing implementation...\n")

	raw, err := os.ReadFile("contracts/out/SandboxServing.sol/SandboxServing.json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "read artifact: %v\n", err)
		os.Exit(1)
	}
	var artifact struct {
		Bytecode struct{ Object string `json:"object"` } `json:"bytecode"`
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		fmt.Fprintf(os.Stderr, "parse artifact: %v\n", err)
		os.Exit(1)
	}
	bytecode, err := hex.DecodeString(strings.TrimPrefix(artifact.Bytecode.Object, "0x"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "decode bytecode: %v\n", err)
		os.Exit(1)
	}

	implABI, err := abi.JSON(strings.NewReader(chain.SandboxServingMetaData.ABI))
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse ABI: %v\n", err)
		os.Exit(1)
	}

	newImplAddr, implTx, _, err := bind.DeployContract(auth, implABI, bytecode, client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "deploy new impl: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Tx hash  : %s\n", implTx.Hash().Hex())

	implReceipt, err := bind.WaitMined(ctx, client, implTx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wait mined (impl): %v\n", err)
		os.Exit(1)
	}
	if implReceipt.Status == 0 {
		fmt.Fprintln(os.Stderr, "impl deploy tx reverted")
		os.Exit(1)
	}
	fmt.Printf("  New impl : %s\n", newImplAddr.Hex())

	// ── Step 2: beacon.upgradeTo(newImpl) ─────────────────────────────────────
	fmt.Printf("\n[2/2] Calling beacon.upgradeTo(%s)...\n", newImplAddr.Hex())

	beacon, err := chain.NewUpgradeableBeacon(common.HexToAddress(*beaconHex), client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bind beacon: %v\n", err)
		os.Exit(1)
	}

	upgradeTx, err := beacon.UpgradeTo(auth, newImplAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "upgradeTo: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Tx hash  : %s\n", upgradeTx.Hash().Hex())

	upgradeReceipt, err := bind.WaitMined(ctx, client, upgradeTx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wait mined (upgradeTo): %v\n", err)
		os.Exit(1)
	}
	if upgradeReceipt.Status == 0 {
		fmt.Fprintln(os.Stderr, "upgradeTo tx reverted")
		os.Exit(1)
	}

	// ── Verify ────────────────────────────────────────────────────────────────
	opts := &bind.CallOpts{Context: ctx}
	currentImpl, err := beacon.Implementation(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read beacon.implementation: %v\n", err)
		os.Exit(1)
	}
	if currentImpl != newImplAddr {
		fmt.Fprintf(os.Stderr, "verification failed: beacon.implementation=%s want %s\n",
			currentImpl.Hex(), newImplAddr.Hex())
		os.Exit(1)
	}

	fmt.Printf(`
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
UPGRADE COMPLETE
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
New implementation : %s
Upgrade tx         : %s
Beacon             : %s (unchanged)

The proxy address is UNCHANGED — no .env update required.
All user balances, nonces, and provider registrations are preserved.
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
`, newImplAddr.Hex(), upgradeTx.Hash().Hex(), *beaconHex)
}

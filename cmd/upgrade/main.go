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
// Usage (provide either --proxy or --beacon):
//
//	go run ./cmd/upgrade/ \
//	  --rpc      https://evmrpc-testnet.0g.ai \
//	  --key      0x<deployer-private-key>      \
//	  --chain-id 16602                         \
//	  --proxy    0x<proxy-address>
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

// beaconSlot is the ERC-1967 storage slot for the beacon address.
// = keccak256("eip1967.proxy.beacon") - 1
var beaconSlot = common.HexToHash("0xa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50")

func main() {
	rpcURL   := flag.String("rpc",      "https://evmrpc-testnet.0g.ai", "EVM RPC endpoint")
	keyHex   := flag.String("key",      "", "deployer/owner private key (hex)")
	chainID  := flag.Int64("chain-id",  16602, "chain ID")
	proxyHex := flag.String("proxy",    "", "BeaconProxy address (beacon derived automatically)")
	beaconHex := flag.String("beacon",  "", "UpgradeableBeacon address (alternative to --proxy)")
	flag.Parse()

	if *keyHex == "" {
		fmt.Fprintln(os.Stderr, "error: --key is required")
		os.Exit(1)
	}
	if *proxyHex == "" && *beaconHex == "" {
		fmt.Fprintln(os.Stderr, "error: --proxy or --beacon is required")
		os.Exit(1)
	}

	// ── private key ───────────────────────────────────────────────────────────
	privKey, err := crypto.HexToECDSA(strings.TrimPrefix(*keyHex, "0x"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse key: %v\n", err)
		os.Exit(1)
	}
	deployer := crypto.PubkeyToAddress(privKey.PublicKey)

	// ── chain client ──────────────────────────────────────────────────────────
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial rpc: %v\n", err)
		os.Exit(1)
	}

	// ── resolve beacon address ────────────────────────────────────────────────
	var beaconAddr common.Address
	if *proxyHex != "" {
		// Read the beacon address from the proxy's ERC-1967 storage slot.
		proxyAddr := common.HexToAddress(*proxyHex)
		raw, err := client.StorageAt(ctx, proxyAddr, beaconSlot, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read beacon slot: %v\n", err)
			os.Exit(1)
		}
		beaconAddr = common.BytesToAddress(raw)
		fmt.Printf("Proxy    : %s\n", proxyAddr.Hex())
		fmt.Printf("Beacon   : %s  (resolved from proxy)\n", beaconAddr.Hex())
	} else {
		beaconAddr = common.HexToAddress(*beaconHex)
		fmt.Printf("Beacon   : %s\n", beaconAddr.Hex())
	}
	fmt.Printf("Deployer : %s\n", deployer.Hex())

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

	beacon, err := chain.NewUpgradeableBeacon(beaconAddr, client)
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
`, newImplAddr.Hex(), upgradeTx.Hash().Hex(), beaconAddr.Hex())
}

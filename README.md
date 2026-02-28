# 0G Sandbox Billing

On-chain billing settlement for 0G Sandbox (TEE-based voucher model).

---

## Contract Architecture

```
User/Billing ──► BeaconProxy  (stable address, all ETH/state lives here)
                     │ reads implementation from beacon
                     ▼
               UpgradeableBeacon  (stores current impl, owned by deployer)
                     │ delegatecall
                     ▼
               SandboxServing impl  (pure logic, no state, replaceable)
```

The **proxy address never changes**. Upgrading only replaces the implementation.
Given the proxy address, beacon and impl can always be derived on-chain:

```bash
# Beacon address — ERC-1967 slot
cast storage <proxy> 0xa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50

# Current implementation
cast call <beacon> "implementation()(address)"

# Beacon owner
cast call <beacon> "owner()(address)"
```

---

## Scripts

### Deploy (first time)

Deploys the full beacon-proxy stack in 3 steps:
1. SandboxServing implementation (no constructor args)
2. UpgradeableBeacon (impl, deployer)
3. BeaconProxy (beacon, initialize(providerStake))

```bash
go run ./cmd/deploy/ \
  --rpc      https://evmrpc-testnet.0g.ai \
  --key      0x<deployer-private-key> \
  --chain-id 16602 \
  --stake    0
```

Output:
```
Implementation : 0x...
Beacon         : 0x...
Proxy (stable) : 0x...   ← set this as SETTLEMENT_CONTRACT
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--rpc` | `https://evmrpc-testnet.0g.ai` | EVM RPC endpoint |
| `--key` | (required) | Deployer private key (hex, with or without 0x) |
| `--chain-id` | `16602` | Chain ID |
| `--stake` | `0` | `providerStake` passed to `initialize()` (wei) |

---

### Upgrade

Deploys a new implementation and points the beacon at it.
**Proxy address is unchanged** — no `.env` update needed, no user re-acknowledgement required.

```bash
go run ./cmd/upgrade/ \
  --rpc      https://evmrpc-testnet.0g.ai \
  --key      0x<deployer-private-key> \
  --chain-id 16602 \
  --proxy    0x<proxy-address>
```

Output:
```
New implementation : 0x...
Upgrade tx         : 0x...
Beacon             : 0x... (unchanged)
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--rpc` | `https://evmrpc-testnet.0g.ai` | EVM RPC endpoint |
| `--key` | (required) | Deployer/owner private key |
| `--chain-id` | `16602` | Chain ID |
| `--proxy` | (required*) | BeaconProxy address — beacon resolved automatically |
| `--beacon` | (required*) | UpgradeableBeacon address (alternative to `--proxy`) |

\* Provide either `--proxy` or `--beacon`.

---

### Verify

Verifies all three contracts on the block explorer.
**Only the proxy address is needed** — beacon and impl are resolved automatically from chain.

```bash
./scripts/verify-contracts.sh --proxy 0x<proxy-address>
```

The script reads the beacon address from the proxy's ERC-1967 slot, calls
`beacon.implementation()` and `beacon.owner()`, submits verification for all
three contracts, then polls until each is confirmed.

After an upgrade, run the same command — the new impl is verified fresh,
beacon and proxy show `already_verified` and are skipped automatically.

Optional flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--proxy` | (required) | BeaconProxy address |
| `--rpc` | `https://evmrpc-testnet.0g.ai` | EVM RPC endpoint |
| `--api` | `https://chainscan-galileo.0g.ai/open/api` | Etherscan-compatible API |

---

## Development

```bash
# Build contracts (requires Docker)
make build-contracts

# Regenerate Go bindings
make abigen

# Run all tests
go test ./...

# Run Solidity tests (requires Docker)
make test-contracts
```

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

The proxy address never changes. Upgrading only replaces the implementation.

---

## Deployed Contracts (0G Galileo Testnet, chainID 16602)

| Contract | Address |
|----------|---------|
| BeaconProxy (stable) | `0x24cD979DBd0Ae924a3f0c832a724CF4C58E5C210` |
| UpgradeableBeacon    | `0xFDde4299dbD96e2Cd285495B9840995b5018D09B` |
| SandboxServing impl  | `0x458b6B42338618D9Eda09f320f0D1800BD2e1A04` |

Set in `.env`:
```
SETTLEMENT_CONTRACT=0x24cD979DBd0Ae924a3f0c832a724CF4C58E5C210
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
| `--stake` | `0` | `providerStake` value passed to `initialize()` (wei) |

---

### Upgrade

Deploys a new implementation and points the beacon at it.
**Proxy address is unchanged** — no `.env` update needed, no user re-acknowledgement required.

```bash
go run ./cmd/upgrade/ \
  --rpc      https://evmrpc-testnet.0g.ai \
  --key      0x<deployer-private-key> \
  --chain-id 16602 \
  --beacon   0x<beacon-address>
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
| `--beacon` | (required) | UpgradeableBeacon contract address |

---

### Verify

Verifies all three contracts on the block explorer.
**Only the proxy address is needed** — beacon and impl are resolved automatically from chain.

```bash
./scripts/verify-contracts.sh --proxy 0x<proxy-address>
```

The script:
1. Reads the beacon address from the proxy's ERC-1967 slot
2. Calls `beacon.implementation()` for the impl address
3. Calls `beacon.owner()` for the owner address (used in constructor args)
4. Submits verification for all three contracts
5. Polls until each is confirmed

Optional flags:
| Flag | Default | Description |
|------|---------|-------------|
| `--proxy` | (required) | BeaconProxy address |
| `--rpc` | `https://evmrpc-testnet.0g.ai` | EVM RPC endpoint |
| `--api` | `https://chainscan-galileo.0g.ai/open/api` | Etherscan-compatible API |

After an upgrade, run the same command again — the new impl will be verified fresh, beacon and proxy show `already_verified` and are skipped automatically.

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

#!/usr/bin/env bash
# scripts/verify-contracts.sh — verify all three beacon-proxy contracts on the explorer.
#
# Only the proxy address is needed. Beacon and impl addresses are derived on-chain:
#   proxy → eth_getStorageAt(ERC-1967 beacon slot) → beacon
#   beacon.implementation()                         → impl
#   beacon.owner()                                  → owner (needed for constructor args)
#
# Usage:
#   ./scripts/verify-contracts.sh --proxy 0x<proxy-address>
#
# Optional overrides:
#   --rpc     <url>   (default: https://evmrpc-testnet.0g.ai)
#   --api     <url>   (default: https://chainscan-galileo.0g.ai/open/api)
#
# Requires: go, python3, curl

set -uo pipefail

RPC_URL="https://evmrpc-testnet.0g.ai"
API_URL="https://chainscan-galileo.0g.ai/open/api"
COMPILER="v0.8.24+commit.e11b9ed9"
CHAIN_ID="16602"
APIKEY="00"
PROXY_ADDR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --proxy)  PROXY_ADDR="$2"; shift 2 ;;
    --rpc)    RPC_URL="$2";    shift 2 ;;
    --api)    API_URL="$2";    shift 2 ;;
    *) echo "Unknown flag: $1" >&2; exit 1 ;;
  esac
done

if [[ -z "$PROXY_ADDR" ]]; then
  echo "Usage: $0 --proxy 0x<proxy-address> [--rpc <url>] [--api <url>]" >&2
  exit 1
fi

strip0x() { echo "${1#0x}" | tr '[:upper:]' '[:lower:]'; }

# ── helpers: read beacon/impl/owner from chain ────────────────────────────────

# eth_getStorageAt(addr, slot) → 32-byte hex value
eth_storage() {
  local addr="$1" slot="$2"
  curl -s -X POST "$RPC_URL" \
    -H "Content-Type: application/json" \
    -d "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getStorageAt\",\"params\":[\"$addr\",\"$slot\",\"latest\"],\"id\":1}" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['result'])"
}

# eth_call(to, data) → return data hex
eth_call() {
  local to="$1" data="$2"
  curl -s -X POST "$RPC_URL" \
    -H "Content-Type: application/json" \
    -d "{\"jsonrpc\":\"2.0\",\"method\":\"eth_call\",\"params\":[{\"to\":\"$to\",\"data\":\"$data\"},\"latest\"],\"id\":1}" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['result'])"
}

# address from last 20 bytes of a 32-byte hex value
last20() { echo "0x${1: -40}"; }

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Resolving addresses from proxy $PROXY_ADDR ..."

# ERC-1967 beacon slot: keccak256("eip1967.proxy.beacon") - 1
BEACON_SLOT="0xa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50"

BEACON_RAW=$(eth_storage "$PROXY_ADDR" "$BEACON_SLOT")
BEACON_ADDR=$(last20 "$BEACON_RAW")

# implementation() selector = 0x5c60da1b
IMPL_RAW=$(eth_call "$BEACON_ADDR" "0x5c60da1b")
IMPL_ADDR=$(last20 "$IMPL_RAW")

# owner() selector = 0x8da5cb5b
OWNER_RAW=$(eth_call "$BEACON_ADDR" "0x8da5cb5b")
OWNER_ADDR=$(last20 "$OWNER_RAW")

echo "  Proxy  : $PROXY_ADDR"
echo "  Beacon : $BEACON_ADDR"
echo "  Impl   : $IMPL_ADDR"
echo "  Owner  : $OWNER_ADDR"

IMPL_HEX=$(strip0x   "$IMPL_ADDR")
BEACON_HEX=$(strip0x "$BEACON_ADDR")
OWNER_HEX=$(strip0x  "$OWNER_ADDR")

# ── ABI-encoded constructor args ──────────────────────────────────────────────
# UpgradeableBeacon(address impl_, address owner_)
BEACON_CTOR=$(python3 - <<EOF
print('${IMPL_HEX}'.zfill(64) + '${OWNER_HEX}'.zfill(64))
EOF
)

# BeaconProxy(address beacon, bytes data)
# data = initialize(uint256 0) = selector(fe4b84df) + 32-byte zero = 36 bytes
PROXY_CTOR=$(python3 - <<EOF
beacon  = '${BEACON_HEX}'.zfill(64)
offset  = hex(64)[2:].zfill(64)           # 0x40: bytes arg at word 2
datalen = hex(36)[2:].zfill(64)           # 0x24: 36 bytes
raw     = 'fe4b84df' + '0' * 64           # selector + uint256(0)
padded  = raw + '0' * (128 - len(raw))    # pad to 64-byte boundary
print(beacon + offset + datalen + padded)
EOF
)

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# ── submit_verify ─────────────────────────────────────────────────────────────
# Progress → stderr (always visible).  Only GUID → stdout (captured by caller).
submit_verify() {
  local label="$1" addr="$2" source_path="$3" source_key="$4"
  local contract_name="$5" ctor_args="$6"

  echo "" >&2
  echo "▶ Verifying $label ($addr)..." >&2

  local out
  out=$(go run ./cmd/verify/ \
    --contract      "$addr" \
    --source        "$source_path" \
    --source-key    "$source_key" \
    --contract-name "$contract_name" \
    --constructor-args "$ctor_args" \
    --api           "$API_URL" \
    --compiler      "$COMPILER" \
    --chain-id      "$CHAIN_ID" \
    --apikey        "$APIKEY" 2>&1) || true

  echo "$out" >&2

  # Extract and return GUID via stdout (empty if already verified)
  echo "$out" | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -1
}

GUID_IMPL=$(submit_verify \
  "SandboxServing impl" "$IMPL_ADDR" \
  "contracts/src/SandboxServing.sol" \
  "src/SandboxServing.sol" \
  "src/SandboxServing.sol:SandboxServing" \
  "")

GUID_BEACON=$(submit_verify \
  "UpgradeableBeacon" "$BEACON_ADDR" \
  "contracts/src/proxy/UpgradeableBeacon.sol" \
  "src/proxy/UpgradeableBeacon.sol" \
  "src/proxy/UpgradeableBeacon.sol:UpgradeableBeacon" \
  "$BEACON_CTOR")

GUID_PROXY=$(submit_verify \
  "BeaconProxy" "$PROXY_ADDR" \
  "contracts/src/proxy/BeaconProxy.sol" \
  "src/proxy/BeaconProxy.sol" \
  "src/proxy/BeaconProxy.sol:BeaconProxy" \
  "$PROXY_CTOR")

# ── poll until confirmed ──────────────────────────────────────────────────────
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Polling verification status..."

poll_one() {
  local guid="$1" label="$2"
  if [[ -z "$guid" ]]; then
    echo "  ✓ $label: already verified"
    return 0
  fi
  local i result
  for i in $(seq 1 12); do
    sleep 5
    result=$(curl -s \
      "${API_URL}?module=contract&action=checkverifystatus&guid=${guid}&apikey=${APIKEY}" \
      | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('result',''))" 2>/dev/null \
      || echo "")
    if echo "$result" | grep -qi "pass\|verified"; then
      echo "  ✓ $label: $result"
      return 0
    elif echo "$result" | grep -qi "fail\|error\|invalid"; then
      echo "  ✗ $label: $result" >&2
      return 1
    fi
    echo "  … $label: ${result:-pending} (${i}/12)"
  done
  echo "  ? $label: timed out polling" >&2
  return 1
}

poll_one "$GUID_IMPL"   "SandboxServing impl"
poll_one "$GUID_BEACON" "UpgradeableBeacon"
poll_one "$GUID_PROXY"  "BeaconProxy"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Explorer links:"
echo "  impl   : https://chainscan-galileo.0g.ai/address/${IMPL_ADDR}#code"
echo "  beacon : https://chainscan-galileo.0g.ai/address/${BEACON_ADDR}#code"
echo "  proxy  : https://chainscan-galileo.0g.ai/address/${PROXY_ADDR}#code"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

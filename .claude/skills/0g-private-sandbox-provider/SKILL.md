---
name: 0g-private-sandbox-provider
description: Use this skill when a provider wants to deploy, register, or operate their 0G Private Sandbox service — covering tapp-cli deployment, on-chain registration, snapshot management, earnings withdrawal, sandbox monitoring, and settlement history.
version: 2.0.0
author: 0G Labs
tags: [0g, sandbox, provider, registration, snapshot, tapp]
repository: https://github.com/0gfoundation/0g-sandbox
---

# 0G Private Sandbox — Provider Skill

---

## MANDATORY: Session setup

Detect the language of the user's message and respond in that language throughout the entire session.

First, set up the `0g-provider` CLI (run silently before asking anything):

```bash
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m); [ "$ARCH" = "x86_64" ] && ARCH=amd64; [ "$ARCH" = "aarch64" ] && ARCH=arm64
if command -v 0g-provider &>/dev/null; then
  export PROVIDER_CLI=0g-provider
elif [ -x "$HOME/.local/bin/0g-provider" ]; then
  export PROVIDER_CLI="$HOME/.local/bin/0g-provider"
else
  mkdir -p ~/.local/bin
  curl -fsSL "https://github.com/0gfoundation/0g-sandbox/releases/latest/download/0g-provider_${OS}_${ARCH}" \
    -o ~/.local/bin/0g-provider && chmod +x ~/.local/bin/0g-provider
  export PROVIDER_CLI="$HOME/.local/bin/0g-provider"
fi
echo "CLI ready: $PROVIDER_CLI"
```

Then output the following verbatim and wait for the answer:

---

What would you like to do?

**A — First-time setup**
Deploy the billing service and register it on-chain for the first time.

**B — Update service registration**
Update URL, TEE signer, or pricing on an already-deployed service.

**C — Day-to-day operations**
- `snapshot` — Register, import, list, or delete sandbox images
- `withdraw` — Withdraw accumulated earnings
- `monitor` — View active billing sessions, force-delete sandboxes, archive all
- `events` — View voucher settlement history
- `status` — Check current registration and earnings

Type A, B, or the operation name →

---

After receiving the answer, proceed to the relevant section below.

---

## A — First-time Setup

Full sequence: deploy the billing service → get TEE signing address → register on-chain → verify.

### Step 1 — Deploy the billing service

Use the **0g-tapp-cli skill** to deploy. Invoke it now if the user hasn't deployed yet:

> Key points for 0G Sandbox deployment:
> - App ID: `0g-sandbox` (must match `BACKEND_APP_NAME` in `.env`)
> - Docker Compose file: `docker-compose.yml` in the repo root
> - Server: provider's Tapp TEE server (e.g. `47.236.111.154:50051`)
> - Fill `.env` from `.env.testnet` or `.env.mainnet` before deploying
> - After deploy: note the task ID, wait for it to complete

```bash
tapp-cli -s http://<tapp-server>:50051 start-app -f docker-compose.yml --app-id 0g-sandbox
tapp-cli -s http://<tapp-server>:50051 get-task-status --task-id <task-id>
```

### Step 2 — Get the TEE signing address

The TEE signing key is only known after the app starts — this is why registration must happen after deploy.

```bash
tapp-cli -s http://<tapp-server>:50051 get-app-key --app-id 0g-sandbox
# → e.g. 0xe29b6f4e65a796d77196faf511e0e0b859503656
```

Save this address — it is the `--tee-signer` value for registration.

### Step 3 — Register service on-chain

Two options — **dashboard is recommended**:

#### Option A: Dashboard (recommended)

Open `http://<billing-proxy-host>:8080/dashboard` in a browser.
Connect your provider wallet → Service Registration card → click "Register / Update".

Fill in:
- **Service URL** — public URL of the billing proxy (e.g. `http://47.236.111.154:8080`)
- **TEE Signer Address** — from Step 2
- **CPU Price / min** — default `1000000000000000` neuron (= 0.001 0G/CPU/min)
- **Mem Price / GB / min** — default `500000000000000` neuron (= 0.0005 0G/GB/min)
- **Create Fee** — fee per sandbox creation; **enter carefully** (dashboard default `5000000` is very small — CLI default `60000000000000000` = 0.06 0G is more typical)
- **Stake** — required only on first registration (typically 100 0G on testnet); check the "Required Stake" value shown in the Contract card

#### Option B: CLI

Ask the user for all values, then run:

```bash
PROVIDER_KEY=0x<provider-wallet-key> $PROVIDER_CLIregister \
  --rpc <rpc-url> \
  --chain-id <chain-id> \
  --contract <settlement-contract> \
  --url <billing-proxy-url> \
  --tee-signer <tee-address-from-step2> \
  --price-per-cpu <neuron> \
  --price-per-mem <neuron> \
  --fee <neuron>
```

Network presets:
- Testnet: `--rpc https://evmrpc-testnet.0g.ai --chain-id 16602`
- Mainnet:  `--rpc https://evmrpc.0g.ai --chain-id 16661`

**First registration:** the required stake is read from the contract and attached automatically as `msg.value`. Provider wallet must have enough 0G.

### Step 4 — Verify registration

```bash
$PROVIDER_CLIstatus \
  --rpc <rpc-url> \
  --contract <settlement-contract> \
  --address <provider-address>
```

Or check the Service Registration card on the dashboard — it should show **✓ Registered**.

### Step 5 — Restart billing service to pick up registration

```bash
tapp-cli -s http://<tapp-server>:50051 stop-service --app-id 0g-sandbox --service-name billing
tapp-cli -s http://<tapp-server>:50051 start-service --app-id 0g-sandbox --service-name billing
```

---

## B — Update Service Registration

Re-run registration with new values. No stake required for updates.

**Via dashboard:** open `/dashboard` → Service Registration → "Register / Update" → submit new values.

**Via CLI:**

```bash
PROVIDER_KEY=0x<key> $PROVIDER_CLIregister \
  --rpc <rpc-url> \
  --chain-id <chain-id> \
  --contract <settlement-contract> \
  --url <new-url> \
  --tee-signer <tee-address> \
  --price-per-cpu <neuron> \
  --price-per-mem <neuron> \
  --fee <neuron>
```

> **Warning:** changing the TEE signer increments `signerVersion`. All existing users will get `NOT_ACKNOWLEDGED` errors until they re-run acknowledge.

### Unit conversion reference

| Human-readable | Neuron value |
|----------------|-------------|
| 0.001 0G/CPU/min | `1000000000000000` |
| 0.0005 0G/GB/min | `500000000000000` |
| 0.06 0G create fee | `60000000000000000` |

---

## Snapshot Management

Snapshots are provider-managed shared base images that users pick when creating sandboxes.

**Easiest via dashboard:** `/dashboard` → Snapshots section.

### Option A: Build locally and push

**1. Build image**

```bash
docker build -t <image-name>:<version> -f <Dockerfile> .
```

Base image must be `daytonaio/sandbox:0.5.0-slim` (runs as `USER daytona`):

```dockerfile
FROM daytonaio/sandbox:0.5.0-slim
RUN curl https://sh.rustup.rs -sSf | sh -s -- -y --default-toolchain stable --profile minimal
ENV PATH="/home/daytona/.cargo/bin:${PATH}"
# NOT /root/.cargo/bin
```

Tag must NOT be `:latest`.

**2. Push to internal registry via runner container**

```bash
docker save <image-name>:<version> | docker exec -i <runner-container> docker load
docker exec <runner-container> docker tag \
  <image-name>:<version> \
  registry:6000/daytona/<image-name>:<version>
docker exec <runner-container> docker push \
  registry:6000/daytona/<image-name>:<version>
```

Or with the built-in helper (default runner: `0g-sandbox-billing-runner-1`):

```bash
$PROVIDER_CLIpush-image \
  --image <image-name>:<version> \
  --runner <runner-container-name>
```

### Option B: Import from external registry

Pull any public or private image directly into the internal registry — no local build needed.

**Via dashboard:** Snapshots → "↓ Import Image" — fill in source image, optional credentials, target name and tag.

**Via API:**
```bash
# POST /api/registry/pull  (provider auth required)
# src: source image (e.g. docker.io/library/ubuntu:22.04)
# name: target repo under registry:6000/daytona/
# tag: must NOT be "latest"
# username/password: optional, for private registries
```

This is **synchronous** — may take several minutes for large images.

### Register snapshot

After the image is in the registry, register it as a snapshot:

```bash
# Single snapshot (custom or default spec)
PROVIDER_KEY=0x<key> $PROVIDER_CLIsnapshot \
  --api http://<billing-proxy>:8080 \
  --image registry:6000/daytona/<image-name>:<version> \
  --name <snapshot-name>

# With size tiers (creates <name>-small, <name>-medium, <name>-large)
PROVIDER_KEY=0x<key> $PROVIDER_CLIsnapshot \
  --api http://<billing-proxy>:8080 \
  --image registry:6000/daytona/<image-name>:<version> \
  --name <snapshot-name> \
  --tiers
```

CLI `--tiers` sizes: small (1C/1GB/10GB disk), medium (2C/4GB/30GB), large (4C/8GB/60GB).
Dashboard presets: small (1C/1GB/3GB), medium (2C/4GB/10GB), large (4C/8GB/20GB).

State: `pending` → `active` in ~30s (Daytona pulls the image).

### List / delete snapshots

```bash
PROVIDER_KEY=0x<key> $PROVIDER_CLIsnapshots --api http://<billing-proxy>:8080
PROVIDER_KEY=0x<key> $PROVIDER_CLIdelete-snapshot \
  --api http://<billing-proxy>:8080 --id <snapshot-name>
```

---

## Withdraw Earnings

```bash
PROVIDER_KEY=0x<key> $PROVIDER_CLIwithdraw \
  --rpc <rpc-url> \
  --chain-id <chain-id> \
  --contract <settlement-contract>
```

Or via dashboard: Earnings card → "Withdraw Earnings" button.

If earnings are 0, the command exits early with a message.

---

## Monitor — Billing Sessions & Sandboxes

**Via dashboard:** `/dashboard` → All Sandboxes.

Shows all active billing sessions: sandbox ID, owner wallet, state, accrued fee.

### Force delete a sandbox

Permanently deletes a sandbox regardless of state:

- Dashboard: Delete button next to each sandbox row
- API: `DELETE /api/sandbox/<id>/force` (signed request)

### Archive all sandboxes

Stops all running sandboxes and backs up state to object storage. Use before a server redeploy.

- Dashboard: "Archive All" button
- API: `POST /api/archive-all` (signed request)

User sandboxes are not lost — they can be restored after redeploy.

---

## Voucher Settlement History

Browse on-chain `SettleFeesWithTEE` events.

**Via dashboard:** `/dashboard` → Voucher Settlement History → select time range → Load.

Time range options: last 1h / 24h / 7d / all history. Results are paginated (50 per page).

**Via API:**
```
GET /api/events?since=<unix-timestamp>&page=<n>&page_size=50
```

Each event: `timestamp`, `user`, `total_fee`, `nonce`, `status` (SUCCESS / INSUFFICIENT_BALANCE / …), `tx_hash`.

---

## Status Check

```bash
# With address (no key needed)
$PROVIDER_CLIstatus \
  --rpc <rpc-url> \
  --contract <settlement-contract> \
  --address <provider-address>
```

Shows: URL, TEE signer, CPU/mem pricing, create fee, signer version, stake, earnings.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `AddOrUpdateService` fails: insufficient funds | Wallet < required stake | Fund provider wallet (100 0G on testnet) |
| `PROVIDER_KEY` not found | Env var not set | `export PROVIDER_KEY=0x<key>` before running |
| Snapshot state stays `pending` | Daytona can't pull image | Check registry:6000 is reachable from Daytona; tag must not be `:latest` |
| `push-image` fails: runner not found | Wrong runner container name | `docker ps` to find name, pass `--runner <name>` |
| `delete-snapshot` 404 | Wrong snapshot name | Run `snapshots` command to list exact names |
| Import image very slow | Large image, synchronous pull | Normal — wait or check server logs |
| Users get `NOT_ACKNOWLEDGED` after re-registration | TEE signer changed, signerVersion incremented | Users must re-run `acknowledge` |
| Dashboard create fee default looks too small | Dashboard default = `5000000` neuron (≈ 0 0G) | Always set fee explicitly; CLI default = `60000000000000000` (0.06 0G) |

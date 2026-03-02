# 0G Sandbox Billing

一个 Go 计费代理服务器，部署在 Daytona（沙盒运行时）前，通过 TEE 签名的链上 voucher 以 0G 代币向用户收费。
用户向 Solidity 结算合约充值；代理生成 EIP-712 签名的 voucher，定期在链上结算。

> **初次了解？** 请先阅读 [`CLAUDE.md`](CLAUDE.md)，了解架构、核心概念、计费流程及服务启动方法。

> English version: [README.md](README.md)

---

## 合约架构

```
User/Billing ──► BeaconProxy  (稳定地址，所有 ETH/状态存于此)
                     │ 从 beacon 读取实现地址
                     ▼
               UpgradeableBeacon  (存储当前 impl，由部署者拥有)
                     │ delegatecall
                     ▼
               SandboxServing impl  (纯逻辑，无状态，可替换)
```

**代理地址永不改变**。升级只需替换实现合约。
给定代理地址，可在链上推导出 beacon 和 impl 地址：

```bash
# Beacon 地址 — ERC-1967 slot
cast storage <proxy> 0xa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50

# 当前实现地址
cast call <beacon> "implementation()(address)"

# Beacon 所有者
cast call <beacon> "owner()(address)"
```

---

## 脚本工具

### 部署（首次）

分三步部署完整的 beacon-proxy 合约栈：
1. SandboxServing 实现合约（无构造参数）
2. UpgradeableBeacon（impl, deployer）
3. BeaconProxy（beacon, initialize(providerStake)）

```bash
go run ./cmd/deploy/ \
  --rpc      https://evmrpc-testnet.0g.ai \
  --key      0x<deployer-private-key> \
  --chain-id 16602 \
  --stake    0
```

输出：
```
Implementation : 0x...
Beacon         : 0x...
Proxy (stable) : 0x...   ← 将此地址设为 SETTLEMENT_CONTRACT
```

参数说明：

| 参数 | 默认值 | 说明 |
|------|---------|-------------|
| `--rpc` | `https://evmrpc-testnet.0g.ai` | EVM RPC 地址 |
| `--key` | （必填）| 部署者私钥（十六进制，0x 可选）|
| `--chain-id` | `16602` | 链 ID |
| `--stake` | `0` | 传入 `initialize()` 的 `providerStake`（neuron）|

---

### 升级

部署新实现合约并将 beacon 指向它。
**代理地址不变** — 无需更新 `.env`，无需用户重新确认。

```bash
go run ./cmd/upgrade/ \
  --rpc      https://evmrpc-testnet.0g.ai \
  --key      0x<deployer-private-key> \
  --chain-id 16602 \
  --proxy    0x<proxy-address>
```

输出：
```
New implementation : 0x...
Upgrade tx         : 0x...
Beacon             : 0x... (unchanged)
```

参数说明：

| 参数 | 默认值 | 说明 |
|------|---------|-------------|
| `--rpc` | `https://evmrpc-testnet.0g.ai` | EVM RPC 地址 |
| `--key` | （必填）| 部署者/所有者私钥 |
| `--chain-id` | `16602` | 链 ID |
| `--proxy` | （二选一*）| BeaconProxy 地址 — 自动解析 beacon |
| `--beacon` | （二选一*）| UpgradeableBeacon 地址（与 `--proxy` 二选一）|

\* 提供 `--proxy` 或 `--beacon` 其中之一。

---

### 验证

在区块浏览器上验证全部三个合约。
**只需提供代理地址** — beacon 和 impl 从链上自动解析。

```bash
./scripts/verify-contracts.sh --proxy 0x<proxy-address>
```

脚本从代理的 ERC-1967 slot 读取 beacon 地址，调用 `beacon.implementation()` 和 `beacon.owner()`，
提交三个合约的验证请求，并轮询直到全部确认。

升级后运行相同命令 — 新 impl 重新验证，beacon 和 proxy 显示 `already_verified` 并自动跳过。

可选参数：

| 参数 | 默认值 | 说明 |
|------|---------|-------------|
| `--proxy` | （必填）| BeaconProxy 地址 |
| `--rpc` | `https://evmrpc-testnet.0g.ai` | EVM RPC 地址 |
| `--api` | `https://chainscan-galileo.0g.ai/open/api` | Etherscan 兼容 API |

---

## 启动服务器

将 `.env.example` 复制为 `.env`，填入必要参数，然后：

```bash
go run ./cmd/billing/
```

关键环境变量：

| 变量 | 默认值 | 说明 |
|----------|---------|-------------|
| `DAYTONA_API_URL` | （必填）| Daytona API 地址 |
| `DAYTONA_ADMIN_KEY` | （必填）| Daytona 管理员密钥 |
| `SETTLEMENT_CONTRACT` | （必填）| BeaconProxy 合约地址 |
| `RPC_URL` | （必填）| EVM RPC 地址 |
| `CHAIN_ID` | （必填）| 链 ID（如 16602）|
| `REDIS_ADDR` | `redis:6379` | Redis 地址 |
| `COMPUTE_PRICE_PER_SEC` | `16667` | 每个沙盒的费用（neuron/秒，约 1M neuron/分钟）|
| `CREATE_FEE` | `5000000` | 每次沙盒创建的固定费用（neuron）|
| `VOUCHER_INTERVAL_SEC` | `3600` | voucher 刷新间隔（秒）|
| `PORT` | `8080` | HTTP 服务端口 |
| `MOCK_TEE` | — | 设为 `true` 用于本地开发（使用 `MOCK_APP_PRIVATE_KEY` 代替 TDX gRPC）|
| `MOCK_APP_PRIVATE_KEY` | — | `MOCK_TEE=true` 时使用的十六进制私钥 |

---

## 开发

```bash
# 编译合约（需要 Docker）
make build-contracts

# 重新生成 Go 绑定
make abigen

# 运行所有测试
go test ./...

# 运行 Solidity 测试（需要 Docker）
make test-contracts
```

# 0G Sandbox — 合约注册表

网络：**0G Galileo 测试网**（chain ID 16602）
浏览器：https://chainscan-galileo.0g.ai
部署者/所有者：`0xB831371eb2703305f1d9F8542163633D0675CEd7`

> English version: [CONTRACTS.md](CONTRACTS.md)

---

## 开发合约

> 用于本地开发和集成测试。数据可随时重置。

| 组件 | 地址 |
|------|------|
| **Proxy**（稳定地址）| `0x2024eB0Cc14316fF8Cc425bFB7CC37FD8713E9b3` |
| Beacon | `0xaa77C82Dc6b4243Ff272d88619BD4f23455CCB6E` |

**升级历史：**

| 日期 | Impl | 变更说明 |
|------|------|---------|
| 初始 | — | 首次部署：per-provider 余额隔离，owner 模型 |
| 2026-03-10 | `0x9a3D6C66e3e6E020D8D40d851Db76D76EBfa93f2` | 移除 `settleFeesWithTEE` 中 `msg.sender == provider` 限制，TEE key 直接签结算 tx，无需 `PROVIDER_PRIVATE_KEY` |

```env
SETTLEMENT_CONTRACT=0x2024eB0Cc14316fF8Cc425bFB7CC37FD8713E9b3
```

---

## 测试网合约

> 正式测试网部署，用于 provider 注册和真实计费测试。

| 组件 | 地址 |
|------|------|
| **Proxy**（稳定地址）| `0xd7e0CD227e602FedBb93c36B1F5bf415398508a4` |
| Beacon | `0xe75F37A353EbCbAA497Ea752a6c910c9d0462382` |
| Implementation | `0x6B789e297bcC3c2F375779f1224b534A4c576445` |

**部署时间：** 2026-03-10
**Provider 质押：** 100 0G（`100000000000000000000` neuron）

```env
SETTLEMENT_CONTRACT=0xd7e0CD227e602FedBb93c36B1F5bf415398508a4
```

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

## 部署（首次）

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

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--rpc` | `https://evmrpc-testnet.0g.ai` | EVM RPC 地址 |
| `--key` | （必填）| 部署者私钥（十六进制，0x 可选）|
| `--chain-id` | `16602` | 链 ID |
| `--stake` | `0` | 传入 `initialize()` 的 `providerStake`（neuron）|

---

## 升级

部署新实现合约并将 beacon 指向它。
**代理地址不变** — 无需更新 `.env`，无需用户重新确认。

```bash
go run ./cmd/upgrade/ \
  --rpc      https://evmrpc-testnet.0g.ai \
  --key      0x<deployer-private-key> \
  --chain-id 16602 \
  --proxy    0x<proxy-address>
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--rpc` | `https://evmrpc-testnet.0g.ai` | EVM RPC 地址 |
| `--key` | （必填）| 部署者/所有者私钥 |
| `--chain-id` | `16602` | 链 ID |
| `--proxy` | （二选一*）| BeaconProxy 地址 — 自动解析 beacon |
| `--beacon` | （二选一*）| UpgradeableBeacon 地址（与 `--proxy` 二选一）|

\* 提供 `--proxy` 或 `--beacon` 其中之一。

---

## 验证

在区块浏览器上验证全部三个合约。
**只需提供代理地址** — beacon 和 impl 从链上自动解析。

```bash
./scripts/verify-contracts.sh --proxy 0x<proxy-address>
```

---

## Provider 注册

合约部署后，使用 `cmd/provider` 在链上注册服务。
完整说明见 [`CLI.md`](CLI.md)。

```bash
# 从 tapp-daemon 获取 TEE 签名地址
tapp-cli -s http://<server>:50051 get-app-key --app-id 0g-sandbox
# → Ethereum Address: 0x61beb835...

PROVIDER_KEY=0x<provider-key> go run ./cmd/provider/ init-service \
  --tee-signer <TEE-signer-address> \
  --url        http://<billing-proxy>:8080
```

完成后在 `.env` 中设置 `PROVIDER_ADDRESS`，并向 TEE 地址充入少量 0G 用于 gas。

---

## 设计说明

- **Proxy 地址永不变** — 升级只替换 implementation，proxy 地址是对外稳定地址
- **结算开放** — `settleFeesWithTEE` 任何人可调用，provider 由 voucher 内的 `v.provider` 字段标识，与 `msg.sender` 无关
- **Provider stake 未实现退出机制** — 质押 ETH 目前无法取回，待后续实现 `requestExit` / `withdrawStake`

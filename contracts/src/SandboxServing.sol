// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title SandboxServing
/// @notice On-chain billing settlement for 0G Sandbox (TEE-based voucher model)
contract SandboxServing {

    // ─── Constants ────────────────────────────────────────────────────────────

    uint256 public constant LOCK_TIME = 2 hours;

    /// @dev EIP-712 type hash — field order must match the Go voucher.Sign() implementation
    bytes32 private constant VOUCHER_TYPEHASH = keccak256(
        "SandboxVoucher(address user,address provider,bytes32 usageHash,uint256 nonce,uint256 totalFee)"
    );

    // ─── Structs ──────────────────────────────────────────────────────────────

    struct Account {
        uint256 balance;
        uint256 pendingRefund;
        uint256 refundUnlockAt;
        mapping(address => uint256) lastNonce;       // provider → last settled nonce
        mapping(address => bool)    teeAcknowledged; // provider → user ack'd TEE signer
    }

    struct Service {
        string  url;
        address teeSignerAddress;
        uint256 computePricePerMin;
        uint256 createFee;
        uint256 signerVersion; // incremented on every teeSignerAddress change
    }

    struct SandboxVoucher {
        address user;
        address provider;
        uint256 totalFee;
        bytes32 usageHash;
        uint256 nonce;
        bytes   signature;
    }

    enum SettlementStatus {
        SUCCESS,               // 0
        INSUFFICIENT_BALANCE,  // 1
        PROVIDER_MISMATCH,     // 2
        NOT_ACKNOWLEDGED,      // 3
        INVALID_NONCE,         // 4
        INVALID_SIGNATURE      // 5
    }

    // ─── State ────────────────────────────────────────────────────────────────

    /// @dev Required stake to register as a provider (set at deploy time)
    uint256 public immutable providerStake;

    bytes32 private immutable _domainSeparator;

    mapping(address => Account) private _accounts;
    mapping(address => Service) public  services;
    mapping(address => bool)    public  serviceExists;
    mapping(address => uint256) public  providerEarnings;
    mapping(address => uint256) public  providerStakes;

    // ─── Events ───────────────────────────────────────────────────────────────

    event Deposited(address indexed recipient, address indexed sender, uint256 amount);
    event RefundRequested(address indexed user, uint256 amount, uint256 unlockAt);
    event RefundWithdrawn(address indexed user, uint256 amount);
    event VoucherSettled(
        address indexed user,
        address indexed provider,
        uint256         totalFee,
        bytes32         usageHash,
        uint256         nonce,
        SettlementStatus status
    );
    event EarningsWithdrawn(address indexed provider, uint256 amount);
    event ServiceUpdated(
        address indexed provider,
        string  url,
        address teeSignerAddress,
        uint256 signerVersion
    );
    event TEESignerAcknowledged(address indexed user, address indexed provider, bool acknowledged);

    // ─── Modifiers ────────────────────────────────────────────────────────────

    bool private _locked;
    modifier nonReentrant() {
        require(!_locked, "reentrant");
        _locked = true;
        _;
        _locked = false;
    }

    // ─── Constructor ──────────────────────────────────────────────────────────

    constructor(uint256 _providerStake) {
        providerStake = _providerStake;
        _domainSeparator = keccak256(abi.encode(
            keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"),
            keccak256(bytes("0G Sandbox Serving")),
            keccak256(bytes("1")),
            block.chainid,
            address(this)
        ));
    }

    // ─── Account: deposit / refund ─────────────────────────────────────────────

    /// @notice Deposit ETH into recipient's billing account. Supports third-party top-up.
    function deposit(address recipient) external payable {
        require(msg.value > 0, "zero deposit");
        _accounts[recipient].balance += msg.value;
        emit Deposited(recipient, msg.sender, msg.value);
    }

    /// @notice Request a refund. Enters a lockTime period before withdrawal is allowed.
    /// @dev Cancels any existing pending refund first (re-enters balance).
    function requestRefund(uint256 amount) external {
        Account storage acct = _accounts[msg.sender];
        require(amount > 0, "zero amount");
        // Re-absorb any previous pending refund
        acct.balance += acct.pendingRefund;
        require(acct.balance >= amount, "insufficient balance");
        acct.balance -= amount;
        acct.pendingRefund = amount;
        acct.refundUnlockAt = block.timestamp + LOCK_TIME;
        emit RefundRequested(msg.sender, amount, acct.refundUnlockAt);
    }

    /// @notice Withdraw a previously requested refund after the lock period.
    function withdrawRefund() external nonReentrant {
        Account storage acct = _accounts[msg.sender];
        require(acct.pendingRefund > 0, "no pending refund");
        require(block.timestamp >= acct.refundUnlockAt, "refund locked");
        uint256 amount = acct.pendingRefund;
        acct.pendingRefund = 0;
        (bool ok,) = msg.sender.call{value: amount}("");
        require(ok, "transfer failed");
        emit RefundWithdrawn(msg.sender, amount);
    }

    // ─── Settlement ───────────────────────────────────────────────────────────

    /// @notice Settle a batch of TEE-signed vouchers. Only the provider can submit their own vouchers.
    function settleFeesWithTEE(SandboxVoucher[] calldata vouchers)
        external
        nonReentrant
        returns (SettlementStatus[] memory statuses)
    {
        statuses = new SettlementStatus[](vouchers.length);
        for (uint256 i = 0; i < vouchers.length; i++) {
            statuses[i] = _settleOne(vouchers[i]);
        }
    }

    function _settleOne(SandboxVoucher calldata v) internal returns (SettlementStatus) {
        if (v.provider != msg.sender || !serviceExists[v.provider]) {
            return SettlementStatus.PROVIDER_MISMATCH;
        }

        Account storage acct = _accounts[v.user];

        if (!acct.teeAcknowledged[v.provider]) {
            return SettlementStatus.NOT_ACKNOWLEDGED;
        }

        if (v.nonce <= acct.lastNonce[v.provider]) {
            return SettlementStatus.INVALID_NONCE;
        }

        if (!_verifySignature(v)) {
            return SettlementStatus.INVALID_SIGNATURE;
        }

        // Commit nonce before any state changes (prevents replay even on partial failures)
        acct.lastNonce[v.provider] = v.nonce;

        if (acct.balance >= v.totalFee) {
            // Full payment
            acct.balance -= v.totalFee;
            providerEarnings[v.provider] += v.totalFee;
            // Restore LIFO invariant: pendingRefund ≤ balance.
            // The excess is simply cancelled — it is NOT transferred to the provider.
            if (acct.pendingRefund > acct.balance) {
                acct.pendingRefund = acct.balance;
            }
            emit VoucherSettled(v.user, v.provider, v.totalFee, v.usageHash, v.nonce, SettlementStatus.SUCCESS);
            return SettlementStatus.SUCCESS;
        } else {
            // Drain everything (balance + pendingRefund)
            uint256 paid = acct.balance + acct.pendingRefund;
            acct.balance = 0;
            acct.pendingRefund = 0;
            providerEarnings[v.provider] += paid;
            emit VoucherSettled(v.user, v.provider, v.totalFee, v.usageHash, v.nonce, SettlementStatus.INSUFFICIENT_BALANCE);
            return SettlementStatus.INSUFFICIENT_BALANCE;
        }
    }

    /// @notice View-only preview of settlement results (for Auto-Settler pre-check).
    function previewSettlementResults(SandboxVoucher[] calldata vouchers)
        external
        view
        returns (SettlementStatus[] memory statuses)
    {
        statuses = new SettlementStatus[](vouchers.length);
        for (uint256 i = 0; i < vouchers.length; i++) {
            statuses[i] = _previewOne(vouchers[i]);
        }
    }

    function _previewOne(SandboxVoucher calldata v) internal view returns (SettlementStatus) {
        if (v.provider != msg.sender || !serviceExists[v.provider]) {
            return SettlementStatus.PROVIDER_MISMATCH;
        }
        Account storage acct = _accounts[v.user];
        if (!acct.teeAcknowledged[v.provider]) {
            return SettlementStatus.NOT_ACKNOWLEDGED;
        }
        if (v.nonce <= acct.lastNonce[v.provider]) {
            return SettlementStatus.INVALID_NONCE;
        }
        if (!_verifySignature(v)) {
            return SettlementStatus.INVALID_SIGNATURE;
        }
        if (acct.balance >= v.totalFee) {
            return SettlementStatus.SUCCESS;
        }
        return SettlementStatus.INSUFFICIENT_BALANCE;
    }

    function _verifySignature(SandboxVoucher calldata v) internal view returns (bool) {
        bytes32 structHash = keccak256(abi.encode(
            VOUCHER_TYPEHASH,
            v.user,
            v.provider,
            v.usageHash,
            v.nonce,
            v.totalFee
        ));
        bytes32 digest = keccak256(abi.encodePacked("\x19\x01", _domainSeparator, structHash));
        address recovered = _ecrecover(digest, v.signature);
        return recovered != address(0) && recovered == services[v.provider].teeSignerAddress;
    }

    function _ecrecover(bytes32 digest, bytes memory sig) internal pure returns (address) {
        if (sig.length != 65) return address(0);
        bytes32 r;
        bytes32 s;
        uint8   v;
        assembly {
            r := mload(add(sig, 32))
            s := mload(add(sig, 64))
            v := byte(0, mload(add(sig, 96)))
        }
        if (v < 27) v += 27;
        if (v != 27 && v != 28) return address(0);
        return ecrecover(digest, v, r, s);
    }

    // ─── Provider earnings ────────────────────────────────────────────────────

    function withdrawEarnings() external nonReentrant {
        uint256 amount = providerEarnings[msg.sender];
        require(amount > 0, "no earnings");
        providerEarnings[msg.sender] = 0;
        (bool ok,) = msg.sender.call{value: amount}("");
        require(ok, "transfer failed");
        emit EarningsWithdrawn(msg.sender, amount);
    }

    // ─── Provider Management ──────────────────────────────────────────────────

    /// @notice Register or update provider service.
    /// @dev First registration requires staking providerStake ETH.
    ///      Changing teeSignerAddress increments signerVersion, signalling users to re-acknowledge.
    function addOrUpdateService(
        string  calldata url,
        address teeSignerAddress,
        uint256 computePricePerMin,
        uint256 createFee
    ) external payable {
        bool isNew = !serviceExists[msg.sender];
        if (isNew) {
            require(msg.value >= providerStake, "insufficient stake");
            providerStakes[msg.sender] = msg.value;
            serviceExists[msg.sender] = true;
        }

        Service storage svc = services[msg.sender];
        bool signerChanged = !isNew && svc.teeSignerAddress != teeSignerAddress;

        svc.url                = url;
        svc.teeSignerAddress   = teeSignerAddress;
        svc.computePricePerMin = computePricePerMin;
        svc.createFee          = createFee;
        if (signerChanged) {
            svc.signerVersion += 1;
        }

        emit ServiceUpdated(msg.sender, url, teeSignerAddress, svc.signerVersion);
    }

    /// @notice Acknowledge (or revoke) the current TEE signer for a provider.
    function acknowledgeTEESigner(address provider, bool acknowledged) external {
        require(serviceExists[provider], "provider not found");
        _accounts[msg.sender].teeAcknowledged[provider] = acknowledged;
        emit TEESignerAcknowledged(msg.sender, provider, acknowledged);
    }

    // ─── View Functions ───────────────────────────────────────────────────────

    function getAccount(address user)
        external
        view
        returns (uint256 balance, uint256 pendingRefund, uint256 refundUnlockAt)
    {
        Account storage a = _accounts[user];
        return (a.balance, a.pendingRefund, a.refundUnlockAt);
    }

    function getLastNonce(address user, address provider) external view returns (uint256) {
        return _accounts[user].lastNonce[provider];
    }

    function getProviderEarnings(address provider) external view returns (uint256) {
        return providerEarnings[provider];
    }

    function isTEEAcknowledged(address user, address provider) external view returns (bool) {
        return _accounts[user].teeAcknowledged[provider];
    }

    function domainSeparator() external view returns (bytes32) {
        return _domainSeparator;
    }
}

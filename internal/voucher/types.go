package voucher

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// SandboxVoucher is the signed billing proof submitted to the smart contract.
// SandboxID is metadata only (not part of the EIP-712 struct); it is carried
// in JSON so the settler knows which sandbox to stop on failure.
type SandboxVoucher struct {
	SandboxID string         `json:"sandbox_id"`
	User      common.Address `json:"user"`
	Provider  common.Address `json:"provider"`
	TotalFee  *big.Int       `json:"total_fee"`
	UsageHash [32]byte       `json:"usage_hash"`
	Nonce     *big.Int       `json:"nonce"`
	Signature []byte         `json:"signature"`
}

// Redis key templates
const (
	VoucherQueueKeyFmt = "voucher:queue:%s" // %s = provider address (checksummed)
	VoucherDLQKeyFmt   = "voucher:dlq:%s"
	NonceKeyFmt        = "billing:nonce:%s:%s" // %s = owner, provider
)

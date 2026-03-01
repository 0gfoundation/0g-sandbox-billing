package settler

import (
	"context"

	"github.com/0gfoundation/0g-sandbox-billing/internal/chain"
	"github.com/0gfoundation/0g-sandbox-billing/internal/voucher"
)

// StopSignal carries the reason a sandbox should be stopped.
type StopSignal struct {
	SandboxID string
	Reason    string // "insufficient_balance" | "not_acknowledged"
}

// ChainClient submits signed vouchers to the settlement contract.
// Satisfied by *chain.Client; decoupled here so the settler can be tested
// without a live RPC connection.
type ChainClient interface {
	SettleFeesWithTEE(ctx context.Context, vouchers []voucher.SandboxVoucher) ([]chain.SettlementStatus, error)
}

package billing

import (
	"context"
	"math/big"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RunGenerator periodically scans all billing sessions and pre-charges the next
// compute period for any session whose NextVoucherAt has elapsed.
func RunGenerator(ctx context.Context, rdb *redis.Client, h *EventHandler, log *zap.Logger) {
	interval := time.Duration(h.voucherIntervalSec) * time.Second

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Info("voucher generator started", zap.Duration("interval", interval))

	for {
		select {
		case <-ctx.Done():
			log.Info("voucher generator stopped")
			return
		case <-ticker.C:
			runGeneration(ctx, rdb, h, log)
		}
	}
}

func runGeneration(ctx context.Context, rdb *redis.Client, h *EventHandler, log *zap.Logger) {
	sessions, err := ScanAllSessions(ctx, rdb)
	if err != nil {
		log.Error("generator: scan sessions", zap.Error(err))
		return
	}

	now := time.Now().Unix()

	for _, sess := range sessions {
		s := sess
		if now < s.NextVoucherAt {
			continue
		}

		// Use per-sandbox rate stored in session; fall back to global flat rate.
		price := h.computePricePerSec
		if s.PricePerSec != "" {
			if p, ok := new(big.Int).SetString(s.PricePerSec, 10); ok && p.Sign() > 0 {
				price = p
			}
		}

		nextVoucherAt, err := h.emitPeriodVoucher(ctx, s.SandboxID, s.Owner, price, s.NextVoucherAt)
		if err != nil {
			log.Error("generator: emit period voucher", zap.String("sandbox", s.SandboxID), zap.Error(err))
			continue
		}

		if err := UpdateNextVoucherAt(ctx, rdb, s.SandboxID, nextVoucherAt); err != nil {
			log.Error("generator: update next_voucher_at", zap.String("sandbox", s.SandboxID), zap.Error(err))
		}
	}
}

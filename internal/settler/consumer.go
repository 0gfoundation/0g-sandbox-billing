package settler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/0gfoundation/0g-sandbox-billing/internal/chain"
	"github.com/0gfoundation/0g-sandbox-billing/internal/config"
	"github.com/0gfoundation/0g-sandbox-billing/internal/voucher"
)

const maxBatchSize = 50

// Run is the main settler loop: BLPOP → settle → handle statuses.
func Run(ctx context.Context, cfg *config.Config, rdb *redis.Client, onchain *chain.Client, stopCh chan<- StopSignal, log *zap.Logger) {
	queueKey := fmt.Sprintf(voucher.VoucherQueueKeyFmt, cfg.Chain.ProviderAddress)
	// lockTime/2 as BLPOP timeout (half the lock window for responsiveness)
	blpopTimeout := time.Duration(cfg.Billing.VoucherIntervalSec) * time.Second / 2

	log.Info("settler started", zap.String("queue", queueKey))

	for {
		if ctx.Err() != nil {
			log.Info("settler stopped")
			return
		}

		// BLPOP blocks until an item appears or timeout
		results, err := rdb.BLPop(ctx, blpopTimeout, queueKey).Result()
		if err != nil {
			if err == redis.Nil {
				// Timeout: no items, loop back
				continue
			}
			if ctx.Err() != nil {
				return
			}
			log.Error("settler: BLPOP error", zap.Error(err))
			time.Sleep(time.Second)
			continue
		}

		// results[0] = key, results[1] = value (already popped by BLPOP)
		firstItem := results[1]

		// Peek remaining items (don't pop yet; pop happens in handler after settlement)
		remaining, err := rdb.LRange(ctx, queueKey, 0, int64(maxBatchSize-2)).Result()
		if err != nil {
			log.Error("settler: LRANGE", zap.Error(err))
			remaining = nil
		}

		// Deserialize batch
		rawItems := append([]string{firstItem}, remaining...)
		vouchers := make([]voucher.SandboxVoucher, 0, len(rawItems))
		for _, raw := range rawItems {
			var v voucher.SandboxVoucher
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				log.Error("settler: unmarshal voucher", zap.String("raw", raw), zap.Error(err))
				continue
			}
			vouchers = append(vouchers, v)
		}

		if len(vouchers) == 0 {
			continue
		}

		// Submit to chain
		statuses, err := onchain.SettleFeesWithTEE(ctx, vouchers)
		if err != nil {
			log.Error("settler: SettleFeesWithTEE", zap.Error(err))
			// Re-push first item back (it was already BLPOP'd)
			_ = rdb.LPush(ctx, queueKey, firstItem)
			time.Sleep(5 * time.Second)
			continue
		}

		// Handle results (first item already popped; handler pops the rest)
		HandleStatuses(ctx, rdb, stopCh, queueKey, firstItem, vouchers, statuses, log)
	}
}

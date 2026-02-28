package billing

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/redis/go-redis/v9"

	"github.com/0gfoundation/0g-sandbox-billing/internal/voucher"
)

// Signer is the concrete VoucherSigner: signs with the TEE key and pushes to Redis.
type Signer struct {
	privKey      *ecdsa.PrivateKey
	chainID      *big.Int
	contractAddr common.Address
	providerAddr common.Address
	rdb          *redis.Client
}

func NewSigner(
	privKey *ecdsa.PrivateKey,
	chainID *big.Int,
	contractAddr common.Address,
	providerAddr common.Address,
	rdb *redis.Client,
) *Signer {
	return &Signer{
		privKey:      privKey,
		chainID:      chainID,
		contractAddr: contractAddr,
		providerAddr: providerAddr,
		rdb:          rdb,
	}
}

// SignAndEnqueue signs the voucher with the TEE private key and pushes it onto
// the provider's voucher queue in Redis.
func (s *Signer) SignAndEnqueue(ctx context.Context, v *voucher.SandboxVoucher) error {
	if err := voucher.Sign(v, s.privKey, s.chainID, s.contractAddr); err != nil {
		return fmt.Errorf("sign voucher: %w", err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal voucher: %w", err)
	}
	queueKey := fmt.Sprintf(voucher.VoucherQueueKeyFmt, s.providerAddr.Hex())
	return s.rdb.RPush(ctx, queueKey, string(raw)).Err()
}

// IncrNonce atomically increments and returns the nonce for a (owner, provider) pair.
func (s *Signer) IncrNonce(ctx context.Context, owner, provider string) (*big.Int, error) {
	key := fmt.Sprintf(voucher.NonceKeyFmt,
		strings.ToLower(owner),
		strings.ToLower(provider),
	)
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("incr nonce: %w", err)
	}
	return big.NewInt(n), nil
}

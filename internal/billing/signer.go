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
	"go.uber.org/zap"

	"github.com/0gfoundation/0g-sandbox-billing/internal/voucher"
)

// NonceReader reads the last settled nonce for a (user, provider) pair from
// the smart contract. Satisfied by *chain.Client; decoupled here so the
// billing package does not import chain.
type NonceReader interface {
	GetLastNonce(ctx context.Context, user, provider common.Address) (*big.Int, error)
}

// seedAndIncrScript atomically seeds a Redis nonce key from the chain value
// (if the key is absent) and then increments it.
//
// KEYS[1] = nonce key
// ARGV[1] = chain's lastNonce (seed value, string)
//
// SET NX ensures only the first concurrent caller seeds the key; all callers
// then get a unique monotone value from INCR.
var seedAndIncrScript = redis.NewScript(`
redis.call('SET', KEYS[1], ARGV[1], 'NX')
return redis.call('INCR', KEYS[1])
`)

// Signer is the concrete VoucherSigner: signs with the TEE key and pushes to Redis.
type Signer struct {
	privKey      *ecdsa.PrivateKey
	chainID      *big.Int
	contractAddr common.Address
	providerAddr common.Address
	rdb          *redis.Client
	nonceReader  NonceReader
	log          *zap.Logger
}

func NewSigner(
	privKey *ecdsa.PrivateKey,
	chainID *big.Int,
	contractAddr common.Address,
	providerAddr common.Address,
	rdb *redis.Client,
	nonceReader NonceReader,
	log *zap.Logger,
) *Signer {
	return &Signer{
		privKey:      privKey,
		chainID:      chainID,
		contractAddr: contractAddr,
		providerAddr: providerAddr,
		rdb:          rdb,
		nonceReader:  nonceReader,
		log:          log,
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
//
// On the first call after a Redis restart the key will be absent. In that case
// the current on-chain lastNonce is fetched and used to seed the Redis counter
// before incrementing, so the first emitted voucher always has a nonce that is
// strictly greater than the last one the contract accepted.
func (s *Signer) IncrNonce(ctx context.Context, owner, provider string) (*big.Int, error) {
	key := fmt.Sprintf(voucher.NonceKeyFmt,
		strings.ToLower(owner),
		strings.ToLower(provider),
	)

	// Fast path: key exists → plain INCR (single round-trip, no chain call).
	exists, err := s.rdb.Exists(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("check nonce key: %w", err)
	}
	if exists > 0 {
		n, err := s.rdb.Incr(ctx, key).Result()
		if err != nil {
			return nil, fmt.Errorf("incr nonce: %w", err)
		}
		return big.NewInt(n), nil
	}

	// Slow path: key absent (Redis restart or first-ever use).
	// Fetch the last settled nonce from the contract so we never reuse a nonce.
	chainNonce, err := s.nonceReader.GetLastNonce(
		ctx,
		common.HexToAddress(owner),
		common.HexToAddress(provider),
	)
	if err != nil {
		// Chain unreachable: fall back to seeding from 0.
		// The emitted nonce will be 1, which is only correct if no voucher has
		// ever been settled for this pair. Log a warning so operators notice.
		s.log.Warn("IncrNonce: cannot read chain nonce, seeding from 0 — voucher may be rejected if on-chain nonce > 0",
			zap.String("owner", owner),
			zap.String("provider", provider),
			zap.Error(err),
		)
		chainNonce = big.NewInt(0)
	}

	// Atomically: SET key chainNonce NX; INCR key.
	// SET NX is a no-op if another goroutine already seeded the key between the
	// EXISTS check above and this point; INCR always returns a unique value.
	n, err := seedAndIncrScript.Run(ctx, s.rdb, []string{key}, chainNonce.String()).Int64()
	if err != nil {
		return nil, fmt.Errorf("seed and incr nonce: %w", err)
	}
	return big.NewInt(n), nil
}

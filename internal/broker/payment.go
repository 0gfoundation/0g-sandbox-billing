package broker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func inflightKey(user, provider common.Address) string {
	return fmt.Sprintf("broker:topup:inflight:%s:%s", strings.ToLower(user.Hex()), strings.ToLower(provider.Hex()))
}

func backoffKey(user, provider common.Address) string {
	return fmt.Sprintf("broker:topup:backoff:%s:%s", strings.ToLower(user.Hex()), strings.ToLower(provider.Hex()))
}

// IsInflightOrBackoff returns true if a deposit is already in-flight or in backoff for (user, provider).
func IsInflightOrBackoff(ctx context.Context, rdb *redis.Client, user, provider common.Address) bool {
	keys := []string{inflightKey(user, provider), backoffKey(user, provider)}
	for _, k := range keys {
		n, err := rdb.Exists(ctx, k).Result()
		if err == nil && n > 0 {
			return true
		}
	}
	return false
}


// PaymentLayer abstracts the external funding service that calls
// contract.deposit(user, provider, amount) on behalf of the Broker.
type PaymentLayer interface {
	// RequestDeposit asks the Payment Layer to deposit amount neuron into the
	// (user, provider) bucket in the SandboxServing contract.
	RequestDeposit(ctx context.Context, user, provider common.Address, amount *big.Int) error
}

// ── NoopPaymentLayer ──────────────────────────────────────────────────────────

// NoopPaymentLayer logs the request and always succeeds.
// Used before the Payment Layer service is ready.
type NoopPaymentLayer struct{ log *zap.Logger }

func NewNoopPaymentLayer(log *zap.Logger) *NoopPaymentLayer {
	return &NoopPaymentLayer{log: log}
}

func (n *NoopPaymentLayer) RequestDeposit(_ context.Context, user, provider common.Address, amount *big.Int) error {
	n.log.Info("payment layer noop: deposit skipped",
		zap.String("user", user.Hex()),
		zap.String("provider", provider.Hex()),
		zap.String("amount", amount.String()))
	return nil
}

// ── HTTPPaymentLayer ──────────────────────────────────────────────────────────

// HTTPPaymentLayer calls the real Payment Layer HTTP endpoint, signing each
// request with the Broker's TEE key so the Payment Layer can verify origin.
// The signature is sent as an Authorization: Bearer header.
// Timestamp (milliseconds) serves as replay protection.
type HTTPPaymentLayer struct {
	url             string
	signer          *ecdsa.PrivateKey
	client          *http.Client
	log             *zap.Logger
	rdb             *redis.Client
	pollInterval    time.Duration
	pollTimeout     time.Duration
	monitorInterval time.Duration
}

func NewHTTPPaymentLayer(url string, signer *ecdsa.PrivateKey, log *zap.Logger, rdb *redis.Client, pollIntervalSec, pollTimeoutSec, monitorIntervalSec int64) *HTTPPaymentLayer {
	return &HTTPPaymentLayer{
		url:             url,
		signer:          signer,
		client:          &http.Client{Timeout: 10 * time.Second},
		log:             log,
		rdb:             rdb,
		pollInterval:    time.Duration(pollIntervalSec) * time.Second,
		pollTimeout:     time.Duration(pollTimeoutSec) * time.Second,
		monitorInterval: time.Duration(monitorIntervalSec) * time.Second,
	}
}

type depositRequest struct {
	User      string `json:"user"`
	Provider  string `json:"provider"`
	Amount    string `json:"amount"`
	Timestamp int64  `json:"timestamp"` // milliseconds
}

type depositResponse struct {
	RequestID string `json:"request_id"`
}

type depositStatusResponse struct {
	Status string `json:"status"` // "pending", "success", "failed"
	Error  string `json:"error,omitempty"`
}

func (h *HTTPPaymentLayer) RequestDeposit(ctx context.Context, user, provider common.Address, amount *big.Int) error {
	ts := time.Now().UnixMilli()

	// Sign: keccak256(user | provider | amount | timestamp_ms)
	msg := crypto.Keccak256(
		user.Bytes(),
		provider.Bytes(),
		common.LeftPadBytes(amount.Bytes(), 32),
		common.LeftPadBytes(new(big.Int).SetInt64(ts).Bytes(), 8),
	)
	prefixed := crypto.Keccak256([]byte("\x19Ethereum Signed Message:\n32"), msg)
	sig, err := crypto.Sign(prefixed, h.signer)
	if err != nil {
		return fmt.Errorf("sign deposit request: %w", err)
	}
	sig[64] += 27 // normalize V to Ethereum convention (27/28)

	body, _ := json.Marshal(depositRequest{
		User:      user.Hex(),
		Provider:  provider.Hex(),
		Amount:    amount.String(),
		Timestamp: ts,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url+"/deposit", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer 0x%x", sig))

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("payment layer request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		return fmt.Errorf("payment layer returned HTTP %d", resp.StatusCode)
	}

	var dr depositResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return fmt.Errorf("decode deposit response: %w", err)
	}
	if dr.RequestID == "" {
		return fmt.Errorf("payment layer returned empty request_id")
	}

	// Mark in-flight so the monitor won't re-trigger until this request resolves.
	h.rdb.Set(ctx, inflightKey(user, provider), dr.RequestID, h.pollTimeout+30*time.Second) //nolint:errcheck

	h.log.Info("payment layer: deposit requested, polling status",
		zap.String("user", user.Hex()),
		zap.String("provider", provider.Hex()),
		zap.String("amount", amount.String()),
		zap.String("request_id", dr.RequestID))

	go h.pollDepositStatus(dr.RequestID, user, provider)
	return nil
}

func (h *HTTPPaymentLayer) pollDepositStatus(requestID string, user, provider common.Address) {
	ctx, cancel := context.WithTimeout(context.Background(), h.pollTimeout)
	defer cancel()

	statusURL := fmt.Sprintf("%s/deposit/status?id=%s", h.url, requestID)
	ticker := time.NewTicker(h.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			h.rdb.Del(context.Background(), inflightKey(user, provider))                                              //nolint:errcheck
			h.rdb.Set(context.Background(), backoffKey(user, provider), "1", 5*h.monitorInterval) //nolint:errcheck
			h.log.Warn("payment layer: deposit status poll timed out, backoff applied",
				zap.String("request_id", requestID),
				zap.String("user", user.Hex()),
				zap.String("provider", provider.Hex()))
			return
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
			if err != nil {
				h.log.Warn("payment layer: build status request failed", zap.Error(err))
				continue
			}
			resp, err := h.client.Do(req)
			if err != nil {
				h.log.Warn("payment layer: status poll failed", zap.String("request_id", requestID), zap.Error(err))
				continue
			}
			var sr depositStatusResponse
			json.NewDecoder(resp.Body).Decode(&sr) //nolint:errcheck
			resp.Body.Close()                      //nolint:errcheck

			switch sr.Status {
			case "success":
				h.rdb.Del(ctx, inflightKey(user, provider)) //nolint:errcheck
				h.log.Info("payment layer: deposit confirmed",
					zap.String("request_id", requestID),
					zap.String("user", user.Hex()),
					zap.String("provider", provider.Hex()))
				return
			case "failed":
				h.rdb.Del(ctx, inflightKey(user, provider))                                      //nolint:errcheck
				h.rdb.Set(ctx, backoffKey(user, provider), "1", 2*h.monitorInterval) //nolint:errcheck
				h.log.Warn("payment layer: deposit failed, backoff applied",
					zap.String("request_id", requestID),
					zap.String("user", user.Hex()),
					zap.String("provider", provider.Hex()),
					zap.String("error", sr.Error))
				return
			default:
				// pending — keep polling
			}
		}
	}
}

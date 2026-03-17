package broker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"go.uber.org/zap"
)

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
	url    string
	signer *ecdsa.PrivateKey
	client *http.Client
	log    *zap.Logger
}

func NewHTTPPaymentLayer(url string, signer *ecdsa.PrivateKey, log *zap.Logger) *HTTPPaymentLayer {
	return &HTTPPaymentLayer{
		url:    url,
		signer: signer,
		client: &http.Client{Timeout: 10 * time.Second},
		log:    log,
	}
}

type depositRequest struct {
	User      string `json:"user"`
	Provider  string `json:"provider"`
	Amount    string `json:"amount"`
	Timestamp int64  `json:"timestamp"` // milliseconds
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
	h.log.Info("payment layer: deposit requested",
		zap.String("user", user.Hex()),
		zap.String("provider", provider.Hex()),
		zap.String("amount", amount.String()))
	return nil
}

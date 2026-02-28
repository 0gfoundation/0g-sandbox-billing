package auth

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// SignedRequest is the JSON payload inside X-Signed-Message (fields sorted).
type SignedRequest struct {
	Action     string          `json:"action"`
	ExpiresAt  int64           `json:"expires_at"`
	Nonce      string          `json:"nonce"`
	Payload    json.RawMessage `json:"payload"`
	ResourceID string          `json:"resource_id"`
}

const maxFutureWindow = 5 * time.Minute

// Middleware returns a Gin handler that validates EIP-191 wallet signatures.
func Middleware(rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		walletAddr := c.GetHeader("X-Wallet-Address")
		signedMsgB64 := c.GetHeader("X-Signed-Message")
		sigHex := c.GetHeader("X-Wallet-Signature")

		if walletAddr == "" || signedMsgB64 == "" || sigHex == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing auth headers"})
			return
		}

		// Decode signed message
		msgBytes, err := base64.StdEncoding.DecodeString(signedMsgB64)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid X-Signed-Message encoding"})
			return
		}

		var req SignedRequest
		if err := json.Unmarshal(msgBytes, &req); err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid signed message JSON"})
			return
		}

		now := time.Now().Unix()

		// Check expiry
		if req.ExpiresAt <= now {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "request expired"})
			return
		}
		if req.ExpiresAt > now+int64(maxFutureWindow.Seconds()) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "expires_at too far in future"})
			return
		}

		// Decode signature
		sigHex = strings.TrimPrefix(sigHex, "0x")
		sig, err := hex.DecodeString(sigHex)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid signature hex"})
			return
		}

		// Recover signer
		recovered, err := Recover(msgBytes, sig)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
			return
		}
		if !strings.EqualFold(recovered.Hex(), walletAddr) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
			return
		}

		// Nonce dedup via Redis SET NX
		nonceKey := "nonce:" + req.Nonce
		ttl := time.Duration(req.ExpiresAt-now) * time.Second
		set, err := rdb.SetNX(context.Background(), nonceKey, 1, ttl).Result()
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		if !set {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "nonce already used"})
			return
		}

		c.Set("wallet_address", walletAddr)
		c.Next()
	}
}

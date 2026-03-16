package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/0gfoundation/0g-sandbox/internal/broker"
	"github.com/0gfoundation/0g-sandbox/internal/chain"
	"github.com/0gfoundation/0g-sandbox/internal/config"
	"github.com/0gfoundation/0g-sandbox/internal/indexer"
	"github.com/0gfoundation/0g-sandbox/internal/tee"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync() //nolint:errcheck

	cfg, err := config.LoadBroker()
	if err != nil {
		log.Fatal("config load failed", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── Redis ─────────────────────────────────────────────────────────────────
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatal("redis ping failed", zap.Error(err))
	}

	// ── TEE signing key ───────────────────────────────────────────────────────
	appKey, err := tee.Get(ctx)
	if err != nil {
		log.Fatal("failed to retrieve TEE signing key", zap.Error(err))
	}
	cfg.Chain.TEEPrivateKey = appKey.PrivateKeyHex

	if cfg.Chain.ProviderAddress == "" {
		privKey, err := crypto.HexToECDSA(appKey.PrivateKeyHex)
		if err != nil {
			log.Fatal("invalid TEE private key", zap.Error(err))
		}
		cfg.Chain.ProviderAddress = crypto.PubkeyToAddress(privKey.PublicKey).Hex()
	}

	// ── Chain client ──────────────────────────────────────────────────────────
	onchain, err := chain.NewClient(cfg)
	if err != nil {
		log.Fatal("chain client init failed", zap.Error(err))
	}

	// ── Payment layer ─────────────────────────────────────────────────────────
	var payment broker.PaymentLayer
	if cfg.Broker.PaymentLayerURL != "" {
		payment = broker.NewHTTPPaymentLayer(cfg.Broker.PaymentLayerURL, onchain.PrivateKey(), log)
		log.Info("payment layer configured", zap.String("url", cfg.Broker.PaymentLayerURL))
	} else {
		payment = broker.NewNoopPaymentLayer(log)
		log.Info("payment layer: noop (PAYMENT_LAYER_URL not set)")
	}

	// ── Provider indexer ──────────────────────────────────────────────────────
	idx := indexer.New(onchain, rdb, log)
	idx.LoadFromRedis(ctx)
	go idx.Run(ctx)

	// ── Balance monitor ───────────────────────────────────────────────────────
	mon := broker.NewMonitor(
		rdb, onchain, payment, log,
		cfg.Broker.MonitorIntervalSec,
		cfg.Broker.TopupIntervals,
		cfg.Broker.ThresholdIntervals,
	)
	go mon.Run(ctx)

	// ── HTTP server ───────────────────────────────────────────────────────────
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	r.GET("/api/providers", func(c *gin.Context) {
		providers := idx.GetAll()
		if providers == nil {
			providers = []indexer.ProviderRecord{}
		}
		c.JSON(http.StatusOK, providers)
	})

	if os.Getenv("BROKER_DEBUG") == "true" {
		r.GET("/api/monitor", func(c *gin.Context) {
			sessions := mon.GetSessions(c.Request.Context())
			if sessions == nil {
				sessions = []broker.SessionEntry{}
			}
			c.JSON(http.StatusOK, gin.H{
				"total_sessions": len(sessions),
				"sessions":       sessions,
			})
		})
		log.Info("debug route enabled: GET /api/monitor")
	}

	// Session endpoints — called by the billing proxy (TEE-signed requests).
	sessionHandler := broker.NewSessionHandler(idx, onchain, payment, rdb, log, cfg.Broker.TopupIntervals)
	r.POST("/api/session", sessionHandler.HandlePost)
	r.DELETE("/api/session/:id", sessionHandler.HandleDelete)

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	go func() {
		log.Info("broker started", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("broker listen failed", zap.Error(err))
		}
	}()

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("broker shutting down")
	cancel()
}

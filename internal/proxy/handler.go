package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/0gfoundation/0g-sandbox-billing/internal/daytona"
)

// BillingHooks is satisfied by billing.EventHandler.
// Decoupled here so proxy tests can use a mock.
type BillingHooks interface {
	OnCreate(ctx context.Context, sandboxID, ownerAddr string)
	OnStart(ctx context.Context, sandboxID, ownerAddr string)
	OnStop(ctx context.Context, sandboxID string)
	OnDelete(ctx context.Context, sandboxID string)
	OnArchive(ctx context.Context, sandboxID string)
}

// Handler wires up all proxy routes onto a Gin engine.
type Handler struct {
	dtona   *daytona.Client
	billing BillingHooks
	rp      *httputil.ReverseProxy
	log     *zap.Logger
}

func NewHandler(dtona *daytona.Client, bh BillingHooks, log *zap.Logger) *Handler {
	target, _ := url.Parse(dtona.BaseURL())
	rp := httputil.NewSingleHostReverseProxy(target)

	// Inject admin key on every forwarded request
	orig := rp.Director
	rp.Director = func(req *http.Request) {
		orig(req)
		req.Header.Set("Authorization", "Bearer "+dtona.AdminKey())
		req.Host = target.Host
	}

	return &Handler{dtona: dtona, billing: bh, rp: rp, log: log}
}

// Register mounts all routes. authMiddleware should already be applied to the group.
//
// Route structure:
//   - Static routes without sub-actions are registered normally.
//   - All /sandbox/:id/* routes go through a single catch-all handler to avoid
//     Gin's restriction on mixing static segments and wildcard catch-alls.
func (h *Handler) Register(rg *gin.RouterGroup) {
	// ── Create sandbox ─────────────────────────────────────────────────────
	rg.POST("/sandbox", h.handleCreate)

	// ── List / paginated (filter by owner) ────────────────────────────────
	rg.GET("/sandbox", h.handleList)
	rg.GET("/sandbox/paginated", h.handleList)
	rg.GET("/volumes", h.handleListGeneric("daytona-owner"))
	rg.GET("/snapshots", h.handleListGeneric("daytona-owner"))

	// ── DELETE /sandbox/:id (no action suffix, safe to register separately) ─
	rg.DELETE("/sandbox/:id", h.withOwner(h.handleDelete))

	// ── Catch-all for /sandbox/:id/<action> ────────────────────────────────
	// Blocked (autostop/autoarchive), lifecycle hooks, label protection, and
	// transparent forwarding are all dispatched here to keep Gin happy.
	rg.Any("/sandbox/:id/*action", h.handleCatchAll)

	// ── GET /sandbox/:id (no wildcard suffix) ─────────────────────────────
	rg.GET("/sandbox/:id", h.withOwner(h.forward))
}

// ── Create ─────────────────────────────────────────────────────────────────

func (h *Handler) handleCreate(c *gin.Context) {
	wallet := c.GetString("wallet_address")

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body"})
		return
	}

	modified, err := InjectOwner(body, wallet)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	c.Request.Body = io.NopCloser(bytes.NewReader(modified))
	c.Request.ContentLength = int64(len(modified))

	// Use a plain httptest.Recorder to buffer the upstream response so we
	// can extract the sandbox ID without wrapping gin.ResponseWriter
	// (which causes http.CloseNotifier interface issues in tests).
	upstream := httptest.NewRecorder()
	h.rp.ServeHTTP(upstream, c.Request)

	// Copy recorded response → real writer
	result := upstream.Result()
	for k, vs := range result.Header {
		for _, v := range vs {
			c.Writer.Header().Add(k, v)
		}
	}
	c.Writer.WriteHeader(result.StatusCode)
	io.Copy(c.Writer, result.Body) //nolint:errcheck

	if result.StatusCode >= 200 && result.StatusCode < 300 {
		if id := extractID(upstream.Body.Bytes()); id != "" {
			go h.billing.OnCreate(c.Request.Context(), id, wallet)
		}
	}
}

// ── Lifecycle ───────────────────────────────────────────────────────────────
// For these endpoints we only need the status code; write directly to c.Writer
// and read c.Writer.Status() afterwards.

func (h *Handler) handleStart(c *gin.Context) {
	id := c.Param("id")
	wallet := c.GetString("wallet_address")
	h.rp.ServeHTTP(safeWriter{c.Writer}, c.Request)
	if c.Writer.Status() >= 200 && c.Writer.Status() < 300 {
		go h.billing.OnStart(c.Request.Context(), id, wallet)
	}
}

func (h *Handler) handleStop(c *gin.Context) {
	id := c.Param("id")
	h.rp.ServeHTTP(safeWriter{c.Writer}, c.Request)
	if c.Writer.Status() >= 200 && c.Writer.Status() < 300 {
		go h.billing.OnStop(c.Request.Context(), id)
	}
}

func (h *Handler) handleDelete(c *gin.Context) {
	id := c.Param("id")
	h.rp.ServeHTTP(safeWriter{c.Writer}, c.Request)
	if c.Writer.Status() >= 200 && c.Writer.Status() < 300 {
		go h.billing.OnDelete(c.Request.Context(), id)
	}
}

func (h *Handler) handleArchive(c *gin.Context) {
	id := c.Param("id")
	h.rp.ServeHTTP(safeWriter{c.Writer}, c.Request)
	if c.Writer.Status() >= 200 && c.Writer.Status() < 300 {
		go h.billing.OnArchive(c.Request.Context(), id)
	}
}

// ── Labels ──────────────────────────────────────────────────────────────────

func (h *Handler) handleLabels(c *gin.Context) {
	body, _ := io.ReadAll(c.Request.Body)
	stripped, err := StripOwnerLabel(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid label payload"})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(stripped))
	c.Request.ContentLength = int64(len(stripped))
	h.forward(c)
}

// ── List ────────────────────────────────────────────────────────────────────

func (h *Handler) handleList(c *gin.Context) {
	wallet := c.GetString("wallet_address")
	sandboxes, err := h.dtona.ListSandboxes(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream error"})
		return
	}
	var filtered []daytona.Sandbox
	for _, s := range sandboxes {
		if strings.EqualFold(s.Labels[ownerLabel], wallet) {
			filtered = append(filtered, s)
		}
	}
	c.JSON(http.StatusOK, filtered)
}

func (h *Handler) handleListGeneric(_ string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// For volumes/snapshots: forward and let Daytona handle; filter TODO
		h.forward(c)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// handleCatchAll dispatches all /sandbox/:id/<action> requests.
// Gin requires a single catch-all to avoid routing-tree conflicts between
// static sub-paths and wildcard segments.
func (h *Handler) handleCatchAll(c *gin.Context) {
	action := c.Param("action") // e.g. "/start", "/stop", "/autostop", "/labels"
	method := c.Request.Method

	// ── Blocked actions ────────────────────────────────────────────────────
	if action == "/autostop" || action == "/autoarchive" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "managed by billing proxy"})
		return
	}

	// ── Lifecycle with billing hooks ───────────────────────────────────────
	switch {
	case method == http.MethodPost && action == "/start":
		h.withOwner(h.handleStart)(c)
	case method == http.MethodPost && action == "/stop":
		h.withOwner(h.handleStop)(c)
	case method == http.MethodPost && action == "/archive":
		h.withOwner(h.handleArchive)(c)

	// ── Label protection ───────────────────────────────────────────────────
	case method == http.MethodPut && action == "/labels":
		h.withOwner(h.handleLabels)(c)

	// ── Transparent proxy (owner check) ───────────────────────────────────
	default:
		h.withOwner(h.forward)(c)
	}
}

// withOwner wraps a handler with an ownership check.
func (h *Handler) withOwner(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		wallet := c.GetString("wallet_address")
		if err := CheckOwner(c.Request.Context(), h.dtona, id, wallet); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		next(c)
	}
}

// forward passes the request to Daytona as-is.
func (h *Handler) forward(c *gin.Context) {
	h.rp.ServeHTTP(safeWriter{c.Writer}, c.Request)
}

// safeWriter wraps gin.ResponseWriter and overrides CloseNotify so that the
// reverse proxy never triggers a type-assertion on the underlying writer.
// gin.ResponseWriter implements the deprecated http.CloseNotifier, but the
// concrete writer in tests (*httptest.ResponseRecorder) does not, causing a
// panic inside net/http when the interface method is called.
//
//nolint:staticcheck
type safeWriter struct{ gin.ResponseWriter }

//nolint:staticcheck
func (s safeWriter) CloseNotify() <-chan bool { return make(chan bool, 1) }

// extractID tries to parse {"id": "..."} from a JSON response body.
func extractID(body []byte) string {
	var m struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&m); err == nil {
		return m.ID
	}
	return ""
}

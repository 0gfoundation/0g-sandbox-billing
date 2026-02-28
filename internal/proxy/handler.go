package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/0gfoundation/0g-sandbox-billing/internal/billing"
	"github.com/0gfoundation/0g-sandbox-billing/internal/daytona"
)

// Handler wires up all proxy routes onto a Gin engine.
type Handler struct {
	dtona   *daytona.Client
	billing *billing.EventHandler
	rp      *httputil.ReverseProxy
	log     *zap.Logger
}

func NewHandler(dtona *daytona.Client, bh *billing.EventHandler, log *zap.Logger) *Handler {
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
func (h *Handler) Register(rg *gin.RouterGroup) {
	// ── Blocked: autostop / autoarchive endpoints ──────────────────────────
	blocked := []string{
		"/sandbox/:id/autostop",
		"/sandbox/:id/autoarchive",
	}
	for _, path := range blocked {
		rg.Any(path, func(c *gin.Context) {
			c.JSON(http.StatusForbidden, gin.H{"error": "managed by billing proxy"})
		})
	}

	// ── Create sandbox ─────────────────────────────────────────────────────
	rg.POST("/sandbox", h.handleCreate)

	// ── Lifecycle with billing hooks ───────────────────────────────────────
	rg.POST("/sandbox/:id/start", h.withOwner(h.handleStart))
	rg.POST("/sandbox/:id/stop", h.withOwner(h.handleStop))
	rg.DELETE("/sandbox/:id", h.withOwner(h.handleDelete))
	rg.POST("/sandbox/:id/archive", h.withOwner(h.handleArchive))

	// ── Label protection ───────────────────────────────────────────────────
	rg.PUT("/sandbox/:id/labels", h.withOwner(h.handleLabels))

	// ── List / paginated (filter by owner) ────────────────────────────────
	rg.GET("/sandbox", h.handleList)
	rg.GET("/sandbox/paginated", h.handleList)
	rg.GET("/volumes", h.handleListGeneric("daytona-owner"))
	rg.GET("/snapshots", h.handleListGeneric("daytona-owner"))

	// ── Catch-all: owner check + transparent proxy ─────────────────────────
	rg.Any("/sandbox/:id/*action", h.withOwner(h.forward))
	rg.Any("/sandbox/:id", h.withOwner(h.forward))
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

	// Capture sandbox ID from Daytona response for billing hook
	rec := &responseRecorder{ResponseWriter: c.Writer, body: &bytes.Buffer{}}
	h.rp.ServeHTTP(rec, c.Request)

	if rec.status >= 200 && rec.status < 300 {
		sandboxID := extractID(rec.body.Bytes())
		if sandboxID != "" {
			go h.billing.OnCreate(c.Request.Context(), sandboxID, wallet)
		}
	}
}

// ── Lifecycle ───────────────────────────────────────────────────────────────

func (h *Handler) handleStart(c *gin.Context) {
	id := c.Param("id")
	wallet := c.GetString("wallet_address")
	rec := &responseRecorder{ResponseWriter: c.Writer, body: &bytes.Buffer{}}
	h.rp.ServeHTTP(rec, c.Request)
	if rec.status >= 200 && rec.status < 300 {
		go h.billing.OnStart(c.Request.Context(), id, wallet)
	}
}

func (h *Handler) handleStop(c *gin.Context) {
	id := c.Param("id")
	rec := &responseRecorder{ResponseWriter: c.Writer, body: &bytes.Buffer{}}
	h.rp.ServeHTTP(rec, c.Request)
	if rec.status >= 200 && rec.status < 300 {
		go h.billing.OnStop(c.Request.Context(), id)
	}
}

func (h *Handler) handleDelete(c *gin.Context) {
	id := c.Param("id")
	rec := &responseRecorder{ResponseWriter: c.Writer, body: &bytes.Buffer{}}
	h.rp.ServeHTTP(rec, c.Request)
	if rec.status >= 200 && rec.status < 300 {
		go h.billing.OnDelete(c.Request.Context(), id)
	}
}

func (h *Handler) handleArchive(c *gin.Context) {
	id := c.Param("id")
	rec := &responseRecorder{ResponseWriter: c.Writer, body: &bytes.Buffer{}}
	h.rp.ServeHTTP(rec, c.Request)
	if rec.status >= 200 && rec.status < 300 {
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
	h.rp.ServeHTTP(c.Writer, c.Request)
}

// responseRecorder captures status code and body while also writing to the original writer.
type responseRecorder struct {
	gin.ResponseWriter
	status int
	body   *bytes.Buffer
}

func (r *responseRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func (r *responseRecorder) WriteHeaderNow() {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.ResponseWriter.WriteHeaderNow()
}

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

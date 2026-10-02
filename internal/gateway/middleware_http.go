package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/linkc0829/llm-platform/internal/shared"
)

// Keys under which authenticate stores the caller for later middleware.
const (
	principalKey     = "gateway.principal"
	effectiveUserKey = "gateway.effective_user"
)

// The /v1 chain runs authenticate → logUsage → admit → prepare → forward.
// Order matters: logUsage wraps admit so 429s are logged, and admit wraps
// forward so a slot is held until the upstream response is done.

// authenticate resolves the bearer token and the effective user. Failures are
// answered with 401 and, like before the chain existed, not logged.
func (h *Handler) authenticate(c *gin.Context) {
	token, ok := bearerToken(c.GetHeader("Authorization"))
	if !ok || h.resolver == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	principal, err := h.resolver.Resolve(c.Request.Context(), token)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	effectiveUser := principal.ID
	onBehalfOf := strings.TrimSpace(c.GetHeader("X-On-Behalf-Of"))
	// Fallback to principal.ID when X-On-Behalf-Of is empty on a trusted token is intentional:
	// it permits trusted service/machine-to-machine callers to act on their own behalf without impersonation.
	if principal.Trusted && onBehalfOf != "" {
		effectiveUser = onBehalfOf
	}

	c.Set(principalKey, principal)
	c.Set(effectiveUserKey, effectiveUser)
	c.Next()
}

// logUsage writes one gateway_usage line for every authenticated request,
// including ones rejected further down the chain.
func (h *Handler) logUsage(c *gin.Context) {
	principal := c.MustGet(principalKey).(shared.Principal)
	effectiveUser := c.GetString(effectiveUserKey)
	userKind := h.userKind(principal, effectiveUser)

	start := time.Now()
	metrics := &requestMetrics{}
	// The proxy's ModifyResponse and ErrorHandler read metrics from the request context.
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), metricsContextKey, metrics))

	defer func() {
		duration := time.Since(start)
		prompt, completion, model, status, ttft, errStr := metrics.snapshot()
		if status == 0 {
			status = c.Writer.Status()
		}

		fields := []zap.Field{
			zap.String("user_id", effectiveUser),
			zap.String("user_kind", userKind),
			zap.String("principal_id", principal.ID),
			zap.String("workload", defaultWorkload(principal.Workload)),
			zap.String("path", c.Request.URL.Path),
			zap.String("model", model),
			zap.Int("status", status),
			zap.Int64("prompt_tokens", prompt),
			zap.Int64("completion_tokens", completion),
			zap.Int64("latency_ms", duration.Milliseconds()),
		}
		if ttft > 0 {
			fields = append(fields, zap.Int64("ttft_ms", ttft.Milliseconds()))
		}
		if inputCount, inputChars := metrics.input(); inputCount > 0 {
			fields = append(fields, zap.Int("input_count", inputCount), zap.Int("input_chars", inputChars))
		}
		if errStr != "" {
			fields = append(fields, zap.String("error", errStr))
		}
		zap.L().Info("gateway_usage", fields...)
	}()

	c.Next()
}

// userKindUnknown marks an X-On-Behalf-Of ID that is not in auth.json.
const userKindUnknown = "unknown"

// userKind classifies the effective user without logging who they are, so
// dashboards can count people apart from test and service traffic.
func (h *Handler) userKind(principal shared.Principal, effectiveUser string) string {
	if effectiveUser == principal.ID {
		return principal.Kind()
	}
	if p, ok := h.resolver.Lookup(effectiveUser); ok {
		return p.Kind()
	}
	return userKindUnknown
}

// admit enforces the per-user and global in-flight limits without queueing.
func (h *Handler) admit(c *gin.Context) {
	release, ok, reason := h.limiter.TryAcquire(c.GetString(effectiveUserKey))
	if !ok {
		// Rejections are logged too: A2 counts them to prove overload sheds, not queues.
		metrics := metricsFrom(c)
		metrics.setError(reason)
		metrics.setStatusCode(http.StatusTooManyRequests)
		c.Header("Retry-After", "1")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": reason})
		return
	}
	defer release()
	c.Next()
}

// forward hands the request to proxy, the last step of every /v1 route.
func (h *Handler) forward(proxy *httputil.ReverseProxy) gin.HandlerFunc {
	return func(c *gin.Context) {
		proxy.ServeHTTP(c.Writer, c.Request)
	}
}

// metricsFrom returns the metrics logUsage placed in the request context.
func metricsFrom(c *gin.Context) *requestMetrics {
	return c.Request.Context().Value(metricsContextKey).(*requestMetrics)
}

// prepareChat forces stream_options.include_usage on stream requests so
// usage can be logged, caps the requested output length, and rejects requests
// for more than one sequence. An unparseable body is forwarded unchanged: the
// upstream owns chat request validation.
func (h *Handler) prepareChat(c *gin.Context) {
	metrics := metricsFrom(c)
	body, ok := readBody(c, metrics)
	if !ok {
		return
	}
	if payload, err := decodeBody(body); err == nil && payload != nil {
		changed := false
		if stream, ok := payload["stream"].(bool); ok && stream {
			metrics.isStream = true
			streamOpts, ok := payload["stream_options"].(map[string]any)
			if !ok {
				streamOpts = make(map[string]any)
				payload["stream_options"] = streamOpts
			}
			// Usage is always requested so metering is not up to the client; a
			// client that did not ask for it never sees the extra chunk.
			metrics.hideUsage = streamOpts["include_usage"] != true
			streamOpts["include_usage"] = true
			changed = true
		}
		// Only chat gets a missing limit filled in: /v1/completions already
		// defaults to 16 tokens, and filling it would raise that to the cap.
		isChat := strings.HasSuffix(c.FullPath(), "/chat/completions")
		capped, err := capOutputTokens(payload, int64(h.maxOutputTokens), isChat)
		if err == nil {
			err = checkSingleSequence(payload)
		}
		if err != nil {
			code := "invalid_max_tokens"
			if errors.Is(err, ErrMultipleSequences) {
				code = "multiple_sequences_not_supported"
			}
			metrics.setError(code)
			metrics.setStatusCode(http.StatusBadRequest)
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": code})
			return
		}
		if capped {
			changed = true
		}
		if changed {
			if newBody, err := json.Marshal(payload); err == nil {
				body = newBody
			}
		}
		if model, ok := payload["model"].(string); ok && model != "" {
			metrics.setModel(model)
		}
	}
	setBody(c, body)
}

// prepareEmbedding, unlike prepareChat, never forwards a body it cannot check
// while the model binding is on: a parse failure would otherwise bypass it.
func (h *Handler) prepareEmbedding(c *gin.Context) {
	metrics := metricsFrom(c)
	body, ok := readBody(c, metrics)
	if !ok {
		return
	}
	payload, decodeErr := decodeBody(body)

	var modelStr string
	if decodeErr == nil && payload != nil {
		if m, ok := payload["model"].(string); ok {
			modelStr = m
		}
		// Some upstreams (Google's OpenAI-compatible endpoint) return no usage
		// for embeddings, so the input size is the only cost signal left.
		metrics.setInput(countEmbeddingInput(payload["input"]))
	}
	if modelStr != "" {
		metrics.setModel(modelStr)
	}

	if h.embedModel != "" && (decodeErr != nil || modelStr == "" || modelStr != h.embedModel) {
		metrics.setError("unknown_embedding_model")
		metrics.setStatusCode(http.StatusBadRequest)
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "unknown_embedding_model"})
		return
	}
	setBody(c, body)
}

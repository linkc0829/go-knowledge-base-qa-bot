package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type contextKey string

const metricsContextKey contextKey = "gateway.metrics"

// Handler proxies an allow-list of OpenAI-compatible routes with admission
// control, stream usage extraction, and structured usage logging.
type Handler struct {
	embedModel string
	// maxOutputTokens caps max_tokens / max_completion_tokens on chat and
	// completion requests; 0 leaves them untouched.
	maxOutputTokens int
	limiter         *Limiter
	resolver        TokenResolver
	logger          *zap.Logger
	chatProxy       *httputil.ReverseProxy
	embedProxy      *httputil.ReverseProxy
	// readyURL / readyKey / readyClient back /readyz: the chat upstream's
	// model list, the cheapest call that proves it can serve.
	readyURL    string
	readyKey    string
	readyClient *http.Client
}

// NewHandler constructs a gateway Handler with one reverse proxy per upstream.
// Without an embed upstream, embeddings share the chat proxy.
// ponytail: one embed model/upstream pair. Upgrade to a model→upstream map when a second model appears.
func NewHandler(
	upstreamURL *url.URL, upstreamKey string,
	embedUpstreamURL *url.URL, embedUpstreamKey, embedModel string,
	limiter *Limiter, resolver TokenResolver, headerTimeout time.Duration, maxOutputTokens int, logger *zap.Logger,
) *Handler {
	if headerTimeout <= 0 {
		headerTimeout = 300 * time.Second
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
	}

	h := &Handler{
		embedModel:      embedModel,
		maxOutputTokens: maxOutputTokens,
		limiter:         limiter,
		resolver:        resolver,
		logger:          logger,
		chatProxy:       newProxy(transport, upstreamURL, upstreamKey, false),
		readyURL:        singleJoiningSlash(upstreamURL.String(), "/v1/models"),
		readyKey:        upstreamKey,
		// ponytail: fixed 2s; a probe slower than that is a not-ready answer anyway.
		readyClient: &http.Client{Transport: transport, Timeout: 2 * time.Second},
	}
	h.embedProxy = h.chatProxy
	if embedUpstreamURL != nil {
		// The embed base URL carries its own version path (Google: /v1beta/openai).
		h.embedProxy = newProxy(transport, embedUpstreamURL, embedUpstreamKey, true)
	}
	return h
}

// newProxy builds a reverse proxy pinned to one upstream. stripV1 drops the
// inbound /v1 prefix for upstreams whose base URL already has a version path.
func newProxy(transport http.RoundTripper, target *url.URL, key string, stripV1 bool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			if stripV1 {
				pr.Out.URL.Path = singleJoiningSlash(target.Path, strings.TrimPrefix(pr.In.URL.Path, "/v1"))
				pr.Out.URL.RawPath = ""
			}
			if key != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+key)
			} else {
				pr.Out.Header.Del("Authorization")
			}
			pr.Out.Header.Del("X-On-Behalf-Of")
			// Forwarding the caller's Accept-Encoding makes Transport hand back the
			// upstream's gzip bytes undecoded, and usage parsing then silently
			// yields zero. Dropping it lets Transport negotiate and decompress.
			pr.Out.Header.Del("Accept-Encoding")
		},
		ModifyResponse: func(resp *http.Response) error {
			if m, ok := resp.Request.Context().Value(metricsContextKey).(*requestMetrics); ok {
				m.setStatusCode(resp.StatusCode)
				if resp.StatusCode == http.StatusOK {
					if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
						resp.Body = newSSETrackingReader(resp.Body, m, time.Now())
					} else {
						// ponytail: buffers whole JSON response (~2MB per 32-text embedding batch), cap if memory matters.
						resp.Body = newJSONTrackingReader(resp.Body, m)
					}
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(err, context.Canceled) {
				if m, ok := r.Context().Value(metricsContextKey).(*requestMetrics); ok {
					m.setError("client_canceled")
					m.setStatusCode(499)
				}
				return
			}
			if errors.Is(r.Context().Err(), context.DeadlineExceeded) || isTimeoutError(err) {
				if m, ok := r.Context().Value(metricsContextKey).(*requestMetrics); ok {
					m.setError("upstream_timeout")
					m.setStatusCode(http.StatusGatewayTimeout)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusGatewayTimeout)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "upstream_timeout",
				})
				return
			}
			if m, ok := r.Context().Value(metricsContextKey).(*requestMetrics); ok {
				m.setError("bad_gateway")
				m.setStatusCode(http.StatusBadGateway)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "bad_gateway",
			})
		},
	}
}

// readBody reads the request body under the 4MB cap and aborts with
// 413 (too large), 499 (client canceled), or 400 (invalid body) on error.
func readBody(c *gin.Context, metrics *requestMetrics) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			metrics.setError("payload_too_large")
			metrics.setStatusCode(http.StatusRequestEntityTooLarge)
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "payload too large"})
			return nil, false
		}
		if errors.Is(c.Request.Context().Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			metrics.setError("client_canceled")
			metrics.setStatusCode(499)
			c.AbortWithStatus(499)
			return nil, false
		}
		metrics.setError("invalid_body")
		metrics.setStatusCode(http.StatusBadRequest)
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
		return nil, false
	}
	return body, true
}

func setBody(c *gin.Context, body []byte) {
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	c.Request.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

// decodeBody uses UseNumber so large integers survive a re-marshal.
func decodeBody(body []byte) (map[string]any, error) {
	var payload map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	err := dec.Decode(&payload)
	return payload, err
}

// Healthz serves an unauthenticated liveness probe: 200 while the process runs.
func (h *Handler) Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Readyz serves an unauthenticated readiness probe: 200 only when the chat
// upstream answers. B2 times vLLM recovery against it. The reason stays in the
// log; the probe is public and must not echo upstream detail.
func (h *Handler) Readyz(c *gin.Context) {
	if err := h.probeUpstream(c.Request.Context()); err != nil {
		h.logger.Warn("gateway_not_ready", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

func (h *Handler) probeUpstream(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.readyURL, nil)
	if err != nil {
		return fmt.Errorf("build probe: %w", err)
	}
	if h.readyKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.readyKey)
	}
	resp, err := h.readyClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe upstream: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe upstream: status %d", resp.StatusCode)
	}
	return nil
}

// countEmbeddingInput counts the texts in an embeddings "input" (a string or an
// array of strings / token arrays) and their characters. Token arrays count as
// inputs with zero characters.
func countEmbeddingInput(input any) (count, chars int) {
	switch v := input.(type) {
	case string:
		return 1, utf8.RuneCountInString(v)
	case []any:
		for _, item := range v {
			count++
			if text, ok := item.(string); ok {
				chars += utf8.RuneCountInString(text)
			}
		}
	}
	return count, chars
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func defaultWorkload(w string) string {
	if w == "" {
		return "unknown"
	}
	return w
}

func bearerToken(header string) (string, bool) {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bearer") {
		return "", false
	}
	return fields[1], true
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

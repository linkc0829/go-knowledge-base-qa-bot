package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/linkc0829/llm-platform/internal/shared"
)

type mockResolver struct {
	principals map[string]shared.Principal
}

func (m *mockResolver) Resolve(_ context.Context, token string) (shared.Principal, error) {
	p, ok := m.principals[token]
	if !ok {
		return shared.Principal{}, errors.New("invalid token")
	}
	return p, nil
}

func (m *mockResolver) Lookup(id string) (shared.Principal, bool) {
	for _, p := range m.principals {
		if p.ID == id {
			return p, true
		}
	}
	return shared.Principal{}, false
}

type closeNotifyingRecorder struct {
	*httptest.ResponseRecorder
	closed chan bool
}

func newCloseNotifyingRecorder() *closeNotifyingRecorder {
	return &closeNotifyingRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		closed:           make(chan bool, 1),
	}
}

func (c *closeNotifyingRecorder) CloseNotify() <-chan bool {
	return c.closed
}

func setupTestGateway(t *testing.T, upstreamHandler http.Handler, resolver TokenResolver, maxGlobal, maxPerUser int) (*gin.Engine, *httptest.Server) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)

	uURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	limiter := NewLimiter(maxGlobal, maxPerUser)
	h := NewHandler(uURL, "upstream-secret-key", nil, "", "", limiter, resolver, 0, 0, zap.NewNop())

	engine := gin.New()
	RegisterRoutes(engine.Group(""), h)
	return engine, upstream
}

func TestHandler_Authentication(t *testing.T) {
	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"valid-token": {ID: "user-1", Name: "alice"},
		},
	}

	mockUpstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})

	engine, _ := setupTestGateway(t, mockUpstream, resolver, 10, 5)

	t.Run("missing_token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		rec := newCloseNotifyingRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("invalid_token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer bad-token")
		rec := newCloseNotifyingRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("valid_token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer valid-token")
		rec := newCloseNotifyingRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
	})
}

func TestHandler_ConcurrencyLimit_ImmediateRejection(t *testing.T) {
	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"token-alice": {ID: "alice", Name: "alice"},
		},
	}

	holdUpstream := make(chan struct{})
	mockUpstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-holdUpstream
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	// maxPerUser = 1
	engine, _ := setupTestGateway(t, mockUpstream, resolver, 10, 1)

	// Launch first request in background
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req1.Header.Set("Authorization", "Bearer token-alice")
	rec1 := newCloseNotifyingRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.ServeHTTP(rec1, req1)
	}()
	// Release the first request and wait for it, also when an assertion below
	// fails: it logs gateway_usage when it finishes, and without the wait that
	// line lands in whichever test's observer is installed next.
	defer func() {
		close(holdUpstream)
		<-done
	}()

	time.Sleep(20 * time.Millisecond)

	core, logs := observer.New(zap.InfoLevel)
	defer zap.ReplaceGlobals(zap.New(core))()

	// Second request should immediately return 429 without waiting
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req2.Header.Set("Authorization", "Bearer token-alice")
	rec2 := newCloseNotifyingRecorder()
	engine.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("second request status = %d, want 429", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q, want 1", rec2.Header().Get("Retry-After"))
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("rejection took %v, want immediate (<100ms)", elapsed)
	}

	// A rejection missing from gateway_usage would make A2's shed-not-queue
	// check blind: overload would look like silence instead of 429s.
	rejected := logs.FilterMessage("gateway_usage").FilterField(zap.Int("status", http.StatusTooManyRequests)).All()
	if len(rejected) != 1 {
		t.Fatalf("gateway_usage entries with status 429 = %d, want 1", len(rejected))
	}
	if got := rejected[0].ContextMap()["error"]; got != "user_concurrency_limit" {
		t.Errorf("rejection error = %v, want user_concurrency_limit", got)
	}
}

func TestHandler_TrustedImpersonation(t *testing.T) {
	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"untrusted-token": {ID: "untrusted-caller", Trusted: false},
			"trusted-service": {ID: "kb-service", Trusted: true},
		},
	}

	mockUpstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify X-On-Behalf-Of was stripped before forwarding upstream
		if r.Header.Get("X-On-Behalf-Of") != "" {
			t.Errorf("X-On-Behalf-Of should be stripped upstream, got %q", r.Header.Get("X-On-Behalf-Of"))
		}
		// Verify Authorization was swapped to upstream key
		if r.Header.Get("Authorization") != "Bearer upstream-secret-key" {
			t.Errorf("upstream Authorization = %q, want Bearer upstream-secret-key", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	engine, _ := setupTestGateway(t, mockUpstream, resolver, 10, 1)

	t.Run("untrusted_token_ignores_header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer untrusted-token")
		req.Header.Set("X-On-Behalf-Of", "alice")
		rec := newCloseNotifyingRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("trusted_token_honors_header_and_enforces_user_limit", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)

		slowEngine, _ := setupTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			<-hold
			w.WriteHeader(http.StatusOK)
		}), resolver, 10, 1)

		// First request on behalf of alice
		req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req1.Header.Set("Authorization", "Bearer trusted-service")
		req1.Header.Set("X-On-Behalf-Of", "alice")
		rec1 := newCloseNotifyingRecorder()
		go slowEngine.ServeHTTP(rec1, req1)

		time.Sleep(20 * time.Millisecond)

		// Second request also on behalf of alice -> should hit 429 for alice
		req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req2.Header.Set("Authorization", "Bearer trusted-service")
		req2.Header.Set("X-On-Behalf-Of", "alice")
		rec2 := newCloseNotifyingRecorder()
		slowEngine.ServeHTTP(rec2, req2)

		if rec2.Code != http.StatusTooManyRequests {
			t.Errorf("second request status = %d, want 429", rec2.Code)
		}

		// Third request on behalf of bob -> should succeed because bob != alice
		req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req3.Header.Set("Authorization", "Bearer trusted-service")
		req3.Header.Set("X-On-Behalf-Of", "bob")
		rec3 := newCloseNotifyingRecorder()
		go slowEngine.ServeHTTP(rec3, req3)

		time.Sleep(20 * time.Millisecond)
		// bob was allowed in flight!
	})
}

func TestHandler_StreamBodyInjection_And_UsageExtraction(t *testing.T) {
	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"tok": {ID: "u1"},
		},
	}

	var upstreamReceivedBody []byte
	mockUpstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamReceivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Emit SSE chunks with delta content and final usage
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"model":"gpt-4o","choices":[{"delta":{"content":"Hi"}}]}`)
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":" there!"}}]}`)
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"choices":[],"usage":{"prompt_tokens":15,"completion_tokens":8,"total_tokens":23}}`)
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})

	engine, _ := setupTestGateway(t, mockUpstream, resolver, 10, 5)

	reqBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer tok")
	rec := newCloseNotifyingRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Verify upstream received stream_options.include_usage = true
	var receivedMap map[string]any
	if err := json.Unmarshal(upstreamReceivedBody, &receivedMap); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	opts, ok := receivedMap["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Errorf("stream_options.include_usage was not injected: %#v", receivedMap["stream_options"])
	}

	// Verify client received response chunks
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "Hi") || !strings.Contains(bodyStr, "there!") {
		t.Errorf("client body missing expected text chunks: %s", bodyStr)
	}
}

// Metering must not be up to the client: every stream is sent upstream with
// include_usage=true, whatever the client asked. A client that did not ask for
// usage must not see the extra choices-less chunk either, because clients that
// read choices[0] on every chunk break on it (PR #7 review).
func TestHandler_StreamUsageAlwaysMeteredAndHiddenUnlessRequested(t *testing.T) {
	resolver := &mockResolver{principals: map[string]shared.Principal{"tok": {ID: "u1"}}}
	const usageChunk = `{"choices":[],"usage":{"prompt_tokens":15,"completion_tokens":8,"total_tokens":23}}`

	tests := []struct {
		name          string
		streamOptions string
		wantUsage     bool
	}{
		{name: "not_requested", streamOptions: ``, wantUsage: false},
		{name: "explicit_false", streamOptions: `,"stream_options":{"include_usage":false}`, wantUsage: false},
		{name: "explicit_true", streamOptions: `,"stream_options":{"include_usage":true}`, wantUsage: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamBody []byte
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", usageChunk)
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			})
			engine, _ := setupTestGateway(t, upstream, resolver, 10, 5)

			body := `{"model":"gpt-4o","stream":true` + tt.streamOptions + `,"messages":[]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer tok")
			rec, fields := lastUsage(t, engine, req)

			var sent struct {
				StreamOptions map[string]any `json:"stream_options"`
			}
			if err := json.Unmarshal(upstreamBody, &sent); err != nil || sent.StreamOptions["include_usage"] != true {
				t.Errorf("upstream stream_options = %v, want include_usage true", sent.StreamOptions)
			}
			if fields["prompt_tokens"] != int64(15) || fields["completion_tokens"] != int64(8) {
				t.Errorf("logged tokens = %v/%v, want 15/8", fields["prompt_tokens"], fields["completion_tokens"])
			}
			got := rec.Body.String()
			if strings.Contains(got, `"usage"`) != tt.wantUsage {
				t.Errorf("client saw usage chunk = %t, want %t; body: %q", !tt.wantUsage, tt.wantUsage, got)
			}
			if !strings.Contains(got, `"Hi"`) || !strings.Contains(got, "[DONE]") {
				t.Errorf("content or [DONE] missing: %q", got)
			}
		})
	}
}

func TestHandler_PayloadTooLarge(t *testing.T) {
	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"tok": {ID: "u1"},
		},
	}

	mockUpstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	engine, _ := setupTestGateway(t, mockUpstream, resolver, 10, 5)

	// Create payload > 4MB
	oversized := bytes.Repeat([]byte("a"), (4<<20)+1024)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(oversized))
	req.Header.Set("Authorization", "Bearer tok")
	rec := newCloseNotifyingRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 Payload Too Large", rec.Code)
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) {
	return 0, errors.New("simulated network read failure")
}

func TestHandler_ReadBodyError_ReturnsBadRequest(t *testing.T) {
	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"tok": {ID: "u1"},
		},
	}
	mockUpstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	engine, _ := setupTestGateway(t, mockUpstream, resolver, 10, 5)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", failReader{})
	req.Header.Set("Authorization", "Bearer tok")

	rec, usageFields := lastUsage(t, engine, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 Bad Request", rec.Code)
	}
	var errResp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp["error"] != "invalid_body" {
		t.Errorf("error = %q, want invalid_body", errResp["error"])
	}
	if errField, ok := usageFields["error"].(string); !ok || errField != "invalid_body" {
		t.Errorf("logged error field = %v, want invalid_body", usageFields["error"])
	}
}

type cancelReader struct{}

func (cancelReader) Read([]byte) (int, error) {
	return 0, context.Canceled
}

func TestHandler_ReadBodyCanceled_Returns499(t *testing.T) {
	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"tok": {ID: "u1"},
		},
	}
	mockUpstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	engine, _ := setupTestGateway(t, mockUpstream, resolver, 10, 5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", cancelReader{}).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer tok")

	rec, usageFields := lastUsage(t, engine, req)

	if rec.Code != 499 {
		t.Errorf("status = %d, want 499 Client Closed Request", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("expected empty body for 499, got %q", rec.Body.String())
	}
	if errField, ok := usageFields["error"].(string); !ok || errField != "client_canceled" {
		t.Errorf("logged error field = %v, want client_canceled", usageFields["error"])
	}
}

func TestHandler_ClientCancellationPropagatesAndReleasesSlot(t *testing.T) {
	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"tok": {ID: "u1"},
		},
	}

	upstreamCanceled := make(chan struct{})
	var upstreamReceived atomic.Bool

	mockUpstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamReceived.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"Start"}}]}`)
		if flusher != nil {
			flusher.Flush()
		}

		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-time.After(2 * time.Second):
		}
	})

	engine, upstream := setupTestGateway(t, mockUpstream, resolver, 1, 1)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer tok")

	// Make call through gateway
	gwServer := httptest.NewServer(engine)
	defer gwServer.Close()

	gwReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, gwServer.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	gwReq.Header.Set("Authorization", "Bearer tok")

	client := &http.Client{}
	resp, err := client.Do(gwReq)
	if err != nil {
		t.Fatalf("client Do error: %v", err)
	}

	// Read one chunk then cancel
	buf := make([]byte, 128)
	_, _ = resp.Body.Read(buf)
	cancel() // Cancel client request
	_ = resp.Body.Close()

	select {
	case <-upstreamCanceled:
		// Upstream context cancellation verified!
	case <-time.After(1 * time.Second):
		t.Fatal("upstream did not receive context cancellation")
	}
}

// A client that drops mid-stream got headers with 200 already, but it never got
// an answer. Logged as 200 it inflates success counts and hides abandonment,
// which A2 and the usage report rely on (found by cmd/gwload).
func TestHandler_MidStreamCancelLoggedAs499(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(zap.NewNop()) })

	resolver := &mockResolver{principals: map[string]shared.Principal{"tok": {ID: "u1"}}}
	upstreamDone := make(chan struct{})
	mockUpstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Start\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	engine, _ := setupTestGateway(t, mockUpstream, resolver, 10, 5)
	gwServer := httptest.NewServer(engine)
	defer gwServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gwServer.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client Do error: %v", err)
	}
	_, _ = resp.Body.Read(make([]byte, 128))
	cancel()
	_ = resp.Body.Close()
	<-upstreamDone

	deadline := time.Now().Add(time.Second)
	for logs.FilterMessage("gateway_usage").Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	entries := logs.FilterMessage("gateway_usage").All()
	if len(entries) != 1 {
		t.Fatalf("gateway_usage entries = %d, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["status"] != int64(499) || fields["error"] != "client_canceled" {
		t.Errorf("status = %v, error = %v, want 499 client_canceled", fields["status"], fields["error"])
	}
}

func TestSSETrackingReader_ChunkSplitAcrossReads(t *testing.T) {
	metrics := &requestMetrics{}
	startTime := time.Now()

	part1 := "data: {\"model\":\"test-model\",\"choices\":[{\"delta\":{\"c"
	part2 := "ontent\":\"hello\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20}}\n\n"

	pr, pw := io.Pipe()
	reader := newSSETrackingReader(pr, metrics, startTime)

	go func() {
		_, _ = pw.Write([]byte(part1))
		time.Sleep(10 * time.Millisecond)
		_, _ = pw.Write([]byte(part2))
		_ = pw.Close()
	}()

	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}

	if string(out) != part1+part2 {
		t.Errorf("read mismatch: %q", string(out))
	}

	prompt, comp, model, _, ttft, _ := metrics.snapshot()
	if model != "test-model" {
		t.Errorf("model = %q, want test-model", model)
	}
	if prompt != 10 || comp != 20 {
		t.Errorf("tokens = (%d, %d), want (10, 20)", prompt, comp)
	}
	if ttft <= 0 {
		t.Errorf("expected ttft > 0, got %v", ttft)
	}
}

func TestSSETrackingReader_CompletionsStreamRecordsTTFT(t *testing.T) {
	metrics := &requestMetrics{}
	startTime := time.Now().Add(-10 * time.Millisecond)

	stream := "data: {\"model\":\"text-davinci-003\",\"choices\":[{\"text\":\"hello world\"}]}\n\ndata: [DONE]\n\n"
	reader := newSSETrackingReader(io.NopCloser(strings.NewReader(stream)), metrics, startTime)

	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if string(out) != stream {
		t.Errorf("read mismatch: %q", string(out))
	}

	_, _, model, _, ttft, _ := metrics.snapshot()
	if model != "text-davinci-003" {
		t.Errorf("model = %q, want text-davinci-003", model)
	}
	if ttft <= 0 {
		t.Errorf("expected ttft > 0 for completions stream, got %v", ttft)
	}
}

func TestHandler_TransportConfiguration(t *testing.T) {
	uURL, _ := url.Parse("http://localhost:8080")

	// Custom timeout
	hCustom := NewHandler(uURL, "key", nil, "", "", NewLimiter(10, 5), nil, 120*time.Second, 0, zap.NewNop())
	trCustom, ok := hCustom.chatProxy.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", hCustom.chatProxy.Transport)
	}
	if trCustom.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 120s", trCustom.ResponseHeaderTimeout)
	}
	if trCustom.DialContext == nil {
		t.Errorf("DialContext should be configured")
	}

	// Default fallback to 300s when <= 0
	hDefault := NewHandler(uURL, "key", nil, "", "", NewLimiter(10, 5), nil, 0, 0, zap.NewNop())
	trDefault, ok := hDefault.chatProxy.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", hDefault.chatProxy.Transport)
	}
	if trDefault.ResponseHeaderTimeout != 300*time.Second {
		t.Errorf("default ResponseHeaderTimeout = %v, want 300s", trDefault.ResponseHeaderTimeout)
	}
}

func TestHandler_LargeIntegerPreserved(t *testing.T) {
	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"tok": {ID: "u1"},
		},
	}

	var upstreamReceivedBody []byte
	mockUpstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamReceivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})

	engine, _ := setupTestGateway(t, mockUpstream, resolver, 10, 5)

	// 9007199254740993 > 2^53 (max safe float64 integer), would become 9007199254740992 or scientific if cast to float64
	largeIntJSON := `{"model":"gpt-4o","stream":true,"seed":9007199254740993}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(largeIntJSON))
	req.Header.Set("Authorization", "Bearer tok")
	rec := newCloseNotifyingRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if !strings.Contains(string(upstreamReceivedBody), "9007199254740993") {
		t.Errorf("expected 9007199254740993 preserved in upstream body, got: %s", string(upstreamReceivedBody))
	}
}

func TestHandler_UpstreamFailure_Sanitized502(t *testing.T) {
	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"tok": {ID: "u1"},
		},
	}

	// Create an upstream that closes the connection immediately
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("webserver doesn't support hijacking")
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer upstream.Close()

	uURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	limiter := NewLimiter(10, 5)
	h := NewHandler(uURL, "upstream-secret-key", nil, "", "", limiter, resolver, 0, 0, zap.NewNop())

	engine := gin.New()
	RegisterRoutes(engine.Group(""), h)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := newCloseNotifyingRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q, want application/json", rec.Header().Get("Content-Type"))
	}

	var errResp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v, raw: %s", err, rec.Body.String())
	}
	if errResp["error"] != "bad_gateway" {
		t.Errorf("error = %q, want bad_gateway", errResp["error"])
	}
	// Verify raw network error or upstream IP/URL is not leaked
	if strings.Contains(rec.Body.String(), upstream.URL) || strings.Contains(rec.Body.String(), "dial") {
		t.Errorf("error response leaked internal details: %s", rec.Body.String())
	}
}

func TestHandler_UpstreamTimeout_504(t *testing.T) {
	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"tok": {ID: "u1"},
		},
	}

	// Upstream handler sleeps longer than gateway header timeout
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer mockUpstream.Close()

	uURL, err := url.Parse(mockUpstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	// Configure a short 30ms response header timeout
	limiter := NewLimiter(10, 5)
	h := NewHandler(uURL, "upstream-secret-key", nil, "", "", limiter, resolver, 30*time.Millisecond, 0, zap.NewNop())

	engine := gin.New()
	RegisterRoutes(engine.Group(""), h)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := newCloseNotifyingRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504 Gateway Timeout", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q, want application/json", rec.Header().Get("Content-Type"))
	}

	var errResp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v, raw: %s", err, rec.Body.String())
	}
	if errResp["error"] != "upstream_timeout" {
		t.Errorf("error = %q, want upstream_timeout", errResp["error"])
	}
}

func TestHandler_Embeddings_RoutingAndModelBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	resolver := &mockResolver{
		principals: map[string]shared.Principal{
			"tok-trusted":   {ID: "kb-service", Trusted: true},
			"tok-untrusted": {ID: "regular-user", Trusted: false},
		},
	}

	var (
		chatCalls  int32
		embedCalls int32
		lastPath   string
		lastAuth   string
		lastXOnBe  string
	)

	chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&chatCalls, 1)
		lastPath = r.URL.Path
		lastAuth = r.Header.Get("Authorization")
		lastXOnBe = r.Header.Get("X-On-Behalf-Of")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[],"model":"chat-model"}`))
	}))
	defer chatServer.Close()

	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&embedCalls, 1)
		lastPath = r.URL.Path
		lastAuth = r.Header.Get("Authorization")
		lastXOnBe = r.Header.Get("X-On-Behalf-Of")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"index":0,"embedding":[0.1,0.2]}],"model":"gemini-embedding-2","usage":{"prompt_tokens":128,"total_tokens":128}}`))
	}))
	defer embedServer.Close()

	chatURL, err := url.Parse(chatServer.URL)
	if err != nil {
		t.Fatalf("parse chat server URL: %v", err)
	}
	embedURL, err := url.Parse(embedServer.URL + "/v1beta/openai")
	if err != nil {
		t.Fatalf("parse embed server URL: %v", err)
	}

	// 目的：選錯 upstream，等於把流量和 key 送到錯的供應商；放行不符的模型，就會拿不相容的向量去比對，而且沒有任何人會發現。
	t.Run("route_to_embed_upstream_with_stripped_v1", func(t *testing.T) {
		core, logs := observer.New(zap.InfoLevel)
		zap.ReplaceGlobals(zap.New(core))

		h := NewHandler(
			chatURL, "chat-key",
			embedURL, "embed-key", "gemini-embedding-2",
			NewLimiter(10, 5), resolver, 0, 0, zap.NewNop(),
		)
		r := gin.New()
		RegisterRoutes(r.Group(""), h)

		atomic.StoreInt32(&chatCalls, 0)
		atomic.StoreInt32(&embedCalls, 0)

		req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"gemini-embedding-2","input":["test"]}`))
		req.Header.Set("Authorization", "Bearer tok-trusted")
		req.Header.Set("X-On-Behalf-Of", "alice")
		rec := newCloseNotifyingRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
		}
		if atomic.LoadInt32(&embedCalls) != 1 {
			t.Errorf("embedCalls = %d, want 1", atomic.LoadInt32(&embedCalls))
		}
		if atomic.LoadInt32(&chatCalls) != 0 {
			t.Errorf("chatCalls = %d, want 0", atomic.LoadInt32(&chatCalls))
		}
		// Upstream path must be /v1beta/openai/embeddings, never /v1beta/openai/v1/embeddings.
		if lastPath != "/v1beta/openai/embeddings" {
			t.Errorf("upstream path = %q, want /v1beta/openai/embeddings", lastPath)
		}
		if lastAuth != "Bearer embed-key" {
			t.Errorf("upstream auth = %q, want Bearer embed-key", lastAuth)
		}
		if lastXOnBe != "" {
			t.Errorf("upstream X-On-Behalf-Of = %q, want stripped (empty)", lastXOnBe)
		}

		// Verify usage.prompt_tokens record.
		usageEntries := logs.FilterMessage("gateway_usage").FilterField(zap.Int("status", http.StatusOK)).All()
		if len(usageEntries) == 0 {
			t.Fatal("expected gateway_usage log entry")
		}
		ctxMap := usageEntries[len(usageEntries)-1].ContextMap()
		if ctxMap["user_id"] != "alice" {
			t.Errorf("user_id = %v, want alice", ctxMap["user_id"])
		}
		if ctxMap["prompt_tokens"] != int64(128) {
			t.Errorf("prompt_tokens = %v, want 128", ctxMap["prompt_tokens"])
		}
		if ctxMap["completion_tokens"] != int64(0) {
			t.Errorf("completion_tokens = %v, want 0", ctxMap["completion_tokens"])
		}
	})

	t.Run("chat_still_goes_to_chat_upstream", func(t *testing.T) {
		h := NewHandler(
			chatURL, "chat-key",
			embedURL, "embed-key", "gemini-embedding-2",
			NewLimiter(10, 5), resolver, 0, 0, zap.NewNop(),
		)
		r := gin.New()
		RegisterRoutes(r.Group(""), h)

		atomic.StoreInt32(&chatCalls, 0)
		atomic.StoreInt32(&embedCalls, 0)

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat-model","messages":[]}`))
		req.Header.Set("Authorization", "Bearer tok-trusted")
		rec := newCloseNotifyingRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if atomic.LoadInt32(&chatCalls) != 1 {
			t.Errorf("chatCalls = %d, want 1", atomic.LoadInt32(&chatCalls))
		}
		if atomic.LoadInt32(&embedCalls) != 0 {
			t.Errorf("embedCalls = %d, want 0", atomic.LoadInt32(&embedCalls))
		}
	})

	t.Run("fallback_to_chat_upstream_when_no_embed_upstream", func(t *testing.T) {
		h := NewHandler(
			chatURL, "chat-key",
			nil, "", "",
			NewLimiter(10, 5), resolver, 0, 0, zap.NewNop(),
		)
		r := gin.New()
		RegisterRoutes(r.Group(""), h)

		atomic.StoreInt32(&chatCalls, 0)

		req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"any-model"}`))
		req.Header.Set("Authorization", "Bearer tok-trusted")
		rec := newCloseNotifyingRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if atomic.LoadInt32(&chatCalls) != 1 {
			t.Errorf("chatCalls = %d, want 1", atomic.LoadInt32(&chatCalls))
		}
		if lastPath != "/v1/embeddings" {
			t.Errorf("fallback path = %q, want /v1/embeddings", lastPath)
		}
	})

	t.Run("model_binding_rejections_and_logging", func(t *testing.T) {
		cases := []struct {
			name        string
			body        string
			expectModel string
		}{
			{"mismatched_model", `{"model":"wrong-model","input":["hi"]}`, "wrong-model"},
			{"invalid_json", `{"model":`, ""},
			{"missing_model", `{"input":["hi"]}`, ""},
			{"empty_model", `{"model":"","input":["hi"]}`, ""},
			{"non_string_model", `{"model":12345,"input":["hi"]}`, ""},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				core, logs := observer.New(zap.InfoLevel)
				zap.ReplaceGlobals(zap.New(core))

				h := NewHandler(
					chatURL, "chat-key",
					embedURL, "embed-key", "gemini-embedding-2",
					NewLimiter(10, 5), resolver, 0, 0, zap.NewNop(),
				)
				r := gin.New()
				RegisterRoutes(r.Group(""), h)

				atomic.StoreInt32(&embedCalls, 0)

				req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(tc.body))
				req.Header.Set("Authorization", "Bearer tok-trusted")
				rec := newCloseNotifyingRecorder()
				r.ServeHTTP(rec, req)

				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400", rec.Code)
				}
				if !strings.Contains(rec.Body.String(), `"error":"unknown_embedding_model"`) {
					t.Errorf("body = %s, want unknown_embedding_model", rec.Body.String())
				}
				if atomic.LoadInt32(&embedCalls) != 0 {
					t.Errorf("embedCalls = %d, want 0 (must not forward)", atomic.LoadInt32(&embedCalls))
				}

				entries := logs.FilterMessage("gateway_usage").FilterField(zap.Int("status", http.StatusBadRequest)).All()
				if len(entries) == 0 {
					t.Fatal("expected gateway_usage log entry for 400 rejection")
				}
				ctxMap := entries[len(entries)-1].ContextMap()
				if ctxMap["error"] != "unknown_embedding_model" {
					t.Errorf("error = %v, want unknown_embedding_model", ctxMap["error"])
				}
				if tc.expectModel != "" && ctxMap["model"] != tc.expectModel {
					t.Errorf("model = %v, want %s", ctxMap["model"], tc.expectModel)
				}
			})
		}
	})

	// Only allow-listed routes reach an upstream: anything else would be sent
	// with the gateway's own upstream key, and a non-POST embeddings call
	// would skip the model binding that only runs on the body.
	t.Run("unlisted_route_never_forwarded", func(t *testing.T) {
		h := NewHandler(
			chatURL, "chat-key",
			embedURL, "embed-key", "gemini-embedding-2",
			NewLimiter(10, 5), resolver, 0, 0, zap.NewNop(),
		)
		r := gin.New()
		RegisterRoutes(r.Group(""), h)

		for _, tc := range []struct{ method, path string }{
			{http.MethodGet, "/v1/embeddings"},
			{http.MethodPost, "/v1/foo/embeddings"},
			{http.MethodPost, "/v1/files"},
			{http.MethodGet, "/v1/chat/completions"},
		} {
			atomic.StoreInt32(&chatCalls, 0)
			atomic.StoreInt32(&embedCalls, 0)

			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"model":"gemini-embedding-2"}`))
			req.Header.Set("Authorization", "Bearer tok-trusted")
			rec := newCloseNotifyingRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: status = %d, want 404", tc.method, tc.path, rec.Code)
			}
			if calls := atomic.LoadInt32(&chatCalls) + atomic.LoadInt32(&embedCalls); calls != 0 {
				t.Errorf("%s %s: upstream calls = %d, want 0", tc.method, tc.path, calls)
			}
		}
	})

	t.Run("models_goes_to_chat_upstream", func(t *testing.T) {
		h := NewHandler(
			chatURL, "chat-key",
			embedURL, "embed-key", "gemini-embedding-2",
			NewLimiter(10, 5), resolver, 0, 0, zap.NewNop(),
		)
		r := gin.New()
		RegisterRoutes(r.Group(""), h)

		atomic.StoreInt32(&chatCalls, 0)
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer tok-untrusted")
		rec := newCloseNotifyingRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK || atomic.LoadInt32(&chatCalls) != 1 {
			t.Fatalf("status = %d, chatCalls = %d, want 200 and 1", rec.Code, atomic.LoadInt32(&chatCalls))
		}
		if lastPath != "/v1/models" || lastAuth != "Bearer chat-key" {
			t.Errorf("path = %q, auth = %q, want /v1/models with chat key", lastPath, lastAuth)
		}
	})

	t.Run("embed_model_only_enforces_binding_and_forwards_to_chat", func(t *testing.T) {
		h := NewHandler(
			chatURL, "chat-key",
			nil, "", "local-embed-model",
			NewLimiter(10, 5), resolver, 0, 0, zap.NewNop(),
		)
		r := gin.New()
		RegisterRoutes(r.Group(""), h)

		// 1. Matching model routes to chat upstream
		atomic.StoreInt32(&chatCalls, 0)
		req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"local-embed-model"}`))
		req.Header.Set("Authorization", "Bearer tok-trusted")
		rec := newCloseNotifyingRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if atomic.LoadInt32(&chatCalls) != 1 {
			t.Errorf("chatCalls = %d, want 1", atomic.LoadInt32(&chatCalls))
		}
		if lastPath != "/v1/embeddings" {
			t.Errorf("path = %q, want /v1/embeddings", lastPath)
		}

		// 2. Mismatched model rejects with 400
		atomic.StoreInt32(&chatCalls, 0)
		reqBad := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"wrong-model"}`))
		reqBad.Header.Set("Authorization", "Bearer tok-trusted")
		recBad := newCloseNotifyingRecorder()
		r.ServeHTTP(recBad, reqBad)

		if recBad.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", recBad.Code)
		}
		if atomic.LoadInt32(&chatCalls) != 0 {
			t.Errorf("chatCalls = %d, want 0", atomic.LoadInt32(&chatCalls))
		}
	})
}

// lastUsage sends one request through the gateway and returns the context of
// the gateway_usage line it logged.
func lastUsage(t *testing.T, engine *gin.Engine, req *http.Request) (*closeNotifyingRecorder, map[string]any) {
	t.Helper()
	core, logs := observer.New(zap.InfoLevel)
	defer zap.ReplaceGlobals(zap.New(core))()

	rec := newCloseNotifyingRecorder()
	engine.ServeHTTP(rec, req)
	entries := logs.FilterMessage("gateway_usage").All()
	if len(entries) != 1 {
		t.Fatalf("gateway_usage entries = %d, want 1", len(entries))
	}
	return rec, entries[0].ContextMap()
}

// user_kind is what the dashboard filters on to count people; it must follow
// the effective user (X-On-Behalf-Of), not the KB's trusted token.
func TestHandler_UsageLogsUserKind(t *testing.T) {
	resolver := &mockResolver{principals: map[string]shared.Principal{
		"alice-tok": {ID: "p_alice", Name: "alice"},
		"kb-tok":    {ID: "p_kb", Name: "kb", Trusted: true},
		"eval-tok":  {ID: "p_eval", Name: "eval-runner"},
	}}
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"model":"m"}`))
	})
	engine, _ := setupTestGateway(t, upstream, resolver, 10, 5)

	tests := []struct {
		name, token, onBehalfOf, wantUser, wantKind string
	}{
		{"direct_call", "alice-tok", "", "p_alice", shared.KindUser},
		{"service_own_call", "kb-tok", "", "p_kb", shared.KindService},
		{"on_behalf_of_known_test_user", "kb-tok", "p_eval", "p_eval", shared.KindTest},
		{"on_behalf_of_unknown_id", "kb-tok", "p_gone", "p_gone", userKindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
			req.Header.Set("Authorization", "Bearer "+tt.token)
			if tt.onBehalfOf != "" {
				req.Header.Set("X-On-Behalf-Of", tt.onBehalfOf)
			}
			_, usage := lastUsage(t, engine, req)
			if usage["user_id"] != tt.wantUser || usage["user_kind"] != tt.wantKind {
				t.Errorf("user_id/user_kind = %v/%v, want %s/%s", usage["user_id"], usage["user_kind"], tt.wantUser, tt.wantKind)
			}
			// Names stay out of the log: Grafana viewers can query it directly.
			for k, v := range usage {
				if s, _ := v.(string); s == "alice" || s == "kb" || s == "eval-runner" {
					t.Errorf("field %s leaks principal name %q", k, s)
				}
			}
		})
	}
}

// A client that sends Accept-Encoding: gzip (every Go http.Client does) used to
// have the header forwarded, so Transport returned the upstream's gzip bytes
// undecoded and usage parsing silently logged zero tokens. Chat hid this only
// because vLLM does not compress; Google does.
func TestHandler_GzipUpstreamUsageStillParsed(t *testing.T) {
	resolver := &mockResolver{principals: map[string]shared.Principal{"tok": {ID: "alice"}}}
	body := `{"choices":[{"message":{"content":"hi"}}],"model":"m","usage":{"prompt_tokens":42,"completion_tokens":7}}`
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte(body))
		_ = zw.Close()
	})
	engine, _ := setupTestGateway(t, upstream, resolver, 10, 5)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Accept-Encoding", "gzip")
	rec, usage := lastUsage(t, engine, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if usage["prompt_tokens"] != int64(42) || usage["completion_tokens"] != int64(7) {
		t.Errorf("tokens = %v/%v, want 42/7", usage["prompt_tokens"], usage["completion_tokens"])
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Errorf("client body is not plain JSON: %q", rec.Body.String())
	}
}

// Google's OpenAI-compatible embeddings endpoint returns no usage at all, so
// the input size is the only cost signal A3 has for embeddings.
func TestHandler_EmbeddingInputCounted(t *testing.T) {
	resolver := &mockResolver{principals: map[string]shared.Principal{"tok": {ID: "alice"}}}
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"index":0,"embedding":[0.1]}],"model":"gemini-embedding-2"}`))
	})

	tests := []struct {
		name      string
		body      string
		wantCount int
		wantChars int
	}{
		{"array_counts_runes_not_bytes", `{"model":"gemini-embedding-2","input":["你好","abc"]}`, 2, 5},
		{"single_string", `{"model":"gemini-embedding-2","input":"hello"}`, 1, 5},
		{"token_arrays_count_without_chars", `{"model":"gemini-embedding-2","input":[[1,2,3]]}`, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No GATEWAY_EMBED_MODEL: counting must not depend on model binding.
			engine, _ := setupTestGateway(t, upstream, resolver, 10, 5)
			req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer tok")
			rec, usage := lastUsage(t, engine, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if usage["input_count"] != int64(tt.wantCount) || usage["input_chars"] != int64(tt.wantChars) {
				t.Errorf("input_count/input_chars = %v/%v, want %d/%d", usage["input_count"], usage["input_chars"], tt.wantCount, tt.wantChars)
			}
			if usage["prompt_tokens"] != int64(0) {
				t.Errorf("prompt_tokens = %v, want 0 (upstream reported none)", usage["prompt_tokens"])
			}
		})
	}
}

// /readyz is what B2 polls to time vLLM recovery, so it must fail whenever the
// chat upstream cannot serve — including when it hangs rather than refuses —
// while /healthz keeps answering for the live process. The probe is public, so
// it must not echo upstream text.
func TestHandler_Readyz(t *testing.T) {
	tests := []struct {
		name       string
		upstream   func(w http.ResponseWriter, r *http.Request, release <-chan struct{})
		wantStatus int
	}{
		{name: "upstream_serving", wantStatus: http.StatusOK, upstream: func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
			_, _ = io.WriteString(w, `{"data":[]}`)
		}},
		{name: "upstream_loading", wantStatus: http.StatusServiceUnavailable, upstream: func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
			http.Error(w, "model still loading at /secret/path", http.StatusServiceUnavailable)
		}},
		{name: "upstream_hangs", wantStatus: http.StatusServiceUnavailable, upstream: func(_ http.ResponseWriter, _ *http.Request, release <-chan struct{}) {
			<-release
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			seen := make(chan [2]string, 1)
			engine, _ := setupTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- [2]string{r.URL.Path, r.Header.Get("Authorization")}
				tt.upstream(w, r, release)
			}), &mockResolver{}, 10, 5)
			t.Cleanup(func() { close(release) })

			start := time.Now()
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if w.Code != tt.wantStatus {
				t.Fatalf("GET /readyz = %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Errorf("GET /readyz took %v, want within the 2s probe timeout", elapsed)
			}
			if got := <-seen; got != [2]string{"/v1/models", "Bearer upstream-secret-key"} {
				t.Errorf("probe hit %q with auth %q, want /v1/models with the upstream key", got[0], got[1])
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Errorf("GET /readyz body leaks upstream detail: %s", w.Body.String())
			}

			w = httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if w.Code != http.StatusOK {
				t.Errorf("GET /healthz = %d, want 200 regardless of upstream", w.Code)
			}
		})
	}
}

// The cap only protects the backend if it reaches the upstream body, on the
// routes clients actually use, without inflating /v1/completions' default.
func TestHandler_OutputTokensCapped(t *testing.T) {
	resolver := &mockResolver{principals: map[string]shared.Principal{"tok": {ID: "u1"}}}

	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = nil
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(upstream.Close)
	uURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	gin.SetMode(gin.TestMode)
	h := NewHandler(uURL, "key", nil, "", "", NewLimiter(10, 5), resolver, 0, 1000, zap.NewNop())
	engine := gin.New()
	RegisterRoutes(engine.Group(""), h)

	tests := []struct {
		name string
		path string
		body string
		want any // max_tokens the upstream sees; nil means absent
	}{
		{name: "chat_over_cap", path: "/v1/chat/completions", body: `{"model":"m","max_tokens":16000}`, want: float64(1000)},
		{name: "chat_missing_filled", path: "/v1/chat/completions", body: `{"model":"m"}`, want: float64(1000)},
		{name: "chat_stream_over_cap", path: "/v1/chat/completions", body: `{"model":"m","stream":true,"max_tokens":16000}`, want: float64(1000)},
		{name: "completions_over_cap", path: "/v1/completions", body: `{"model":"m","max_tokens":16000}`, want: float64(1000)},
		{name: "completions_missing_kept", path: "/v1/completions", body: `{"model":"m"}`, want: nil},
		{name: "chat_numeric_string_over_cap", path: "/v1/chat/completions", body: `{"model":"m","max_tokens":"90000"}`, want: float64(1000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer tok")
			rec := newCloseNotifyingRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := received["max_tokens"]; got != tt.want {
				t.Errorf("upstream max_tokens = %v, want %v", got, tt.want)
			}
		})
	}
}

// Requests the gateway cannot bound must stop here with 400, never reach the
// upstream: an unreadable output limit, or a fan-out to several sequences.
func TestHandler_UnboundedRequestsRejected(t *testing.T) {
	resolver := &mockResolver{principals: map[string]shared.Principal{"tok": {ID: "u1"}}}
	var upstreamHits atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})
	gin.SetMode(gin.TestMode)
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	uURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	h := NewHandler(uURL, "key", nil, "", "", NewLimiter(10, 5), resolver, 0, 1000, zap.NewNop())
	engine := gin.New()
	RegisterRoutes(engine.Group(""), h)

	tests := []struct {
		name string
		path string
		body string
		code string
	}{
		{name: "non_numeric_max_tokens", path: "/v1/chat/completions", body: `{"model":"m","max_tokens":"lots"}`, code: "invalid_max_tokens"},
		{name: "chat_n_five", path: "/v1/chat/completions", body: `{"model":"m","n":5}`, code: "multiple_sequences_not_supported"},
		{name: "completions_prompt_list", path: "/v1/completions", body: `{"model":"m","prompt":["a","b"]}`, code: "multiple_sequences_not_supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := upstreamHits.Load()
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer tok")
			rec := newCloseNotifyingRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tt.code) {
				t.Errorf("body = %s, want error %s", rec.Body.String(), tt.code)
			}
			if upstreamHits.Load() != before {
				t.Error("request reached the upstream")
			}
		})
	}
}

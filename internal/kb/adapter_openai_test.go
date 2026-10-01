package kb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIClientEmbedBatchesRequests(t *testing.T) {
	var batches [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("Embed() Authorization = %q, want Bearer test", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("Embed() request path = %q, want %q", r.URL.Path, "/v1/embeddings")
		}
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("Embed() decode request = %v, want nil", err)
			return
		}
		batches = append(batches, request.Input)
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			data[i] = map[string]any{"index": i, "embedding": []float64{float64(len(batches)), float64(i)}}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Errorf("Embed() encode response = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)

	texts := make([]string, 65)
	for i := range texts {
		texts[i] = fmt.Sprintf("text-%d", i)
	}
	client := NewOpenAIClient("test", server.URL+"/v1", "chat", "embed", ChatOptions{})
	vectors, err := client.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed() error = %v, want nil", err)
	}

	wantSizes := []int{32, 32, 1}
	if len(batches) != len(wantSizes) {
		t.Fatalf("Embed() batches = %d, want %d", len(batches), len(wantSizes))
	}
	for i, wantSize := range wantSizes {
		if len(batches[i]) != wantSize {
			t.Errorf("Embed() batch %d size = %d, want %d", i+1, len(batches[i]), wantSize)
		}
	}
	if len(vectors) != len(texts) {
		t.Fatalf("Embed() vectors = %d, want %d", len(vectors), len(texts))
	}
	for i, vector := range vectors {
		wantBatch := float32(i/embeddingBatchSize + 1)
		wantIndex := float32(i % embeddingBatchSize)
		if len(vector) != 2 || vector[0] != wantBatch || vector[1] != wantIndex {
			t.Errorf("Embed() vector %d = %v, want [%v %v]", i, vector, wantBatch, wantIndex)
		}
	}
}

func TestGroundedPromptIncludesEvidenceClasses(t *testing.T) {
	sections := []Section{
		mustSection(t, "login-ui_inventory.md", "畫面:登入", "控制項", nil),
		mustSection(t, "login-procedure.md", "步驟 1", "- **動作證據**:`recorded`", nil),
		mustSection(t, "login-procedure.md", "步驟 1a", "- **動作證據**:`vision_inferred`", nil),
		mustSection(t, "login-procedure.md", "步驟 2", "- **動作證據**:`inferred`", nil),
		mustSection(t, "guide.md", "說明", "一般說明", nil),
	}

	got := groundedPrompt("如何登入？", sections)
	for _, want := range []string{
		"[login-ui_inventory.md#畫面-登入] (evidence: ui_inventory)",
		"[login-procedure.md#步驟-1] (evidence: procedure)",
		"[login-procedure.md#步驟-1a] (evidence: procedure_visual)",
		"[login-procedure.md#步驟-2] (evidence: procedure_inferred)",
		"[guide.md#說明] (evidence: general)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("groundedPrompt() = %q, want substring %q", got, want)
		}
	}
}

func TestGroundingSystemWhitelistsProcedureEvidence(t *testing.T) {
	for _, want := range []string{
		"Only sections tagged evidence: procedure or procedure_visual may support an action order",
		"ui_inventory, procedure_unlabeled, procedure_inferred, or general cannot support steps or sequence",
		"A procedure_visual section is a coordinate-and-screenshot visual review",
		"never which control was clicked",
		// Without this the steps-only guardrail reads as a blanket ban and the model
		// refuses button-location questions even with the right section in the top 3.
		"ui_inventory sections are the authoritative record of which controls a screen contains",
		"the steps-only restriction above does not apply",
		"Reply in the same language as the question",
		// Without this the model recites the verified symbol and then refuses because the
		// context does not restate the question's wording.
		"refuse when the answer is absent, never when it is present but phrased differently",
	} {
		if !strings.Contains(groundingSystem, want) {
			t.Errorf("groundingSystem = %q, want substring %q", groundingSystem, want)
		}
	}
}

func mustSection(t *testing.T, file, heading, body string, meta map[string]string) Section {
	t.Helper()
	section, err := NewSection(file, heading, body, meta, nil)
	if err != nil {
		t.Fatalf("NewSection(%q, %q) error = %v, want nil", file, heading, err)
	}
	return section
}

// temperature=0 is the whole point of pinning decoding: the openai-go field is
// tagged omitzero, so a plain float would drop out of the request and hand the
// run back to the served model's own generation_config.
func TestAnswerSendsDecodingParams(t *testing.T) {
	tests := []struct {
		name          string
		options       ChatOptions
		wantTemp      float64
		wantMaxTokens any
		wantEffort    any
	}{
		{name: "greedy_with_limit", options: ChatOptions{Temperature: 0, MaxTokens: 1024}, wantTemp: 0, wantMaxTokens: float64(1024)},
		{name: "zero_max_tokens_omits_field", options: ChatOptions{Temperature: 0.2}, wantTemp: 0.2, wantMaxTokens: nil},
		// A thinking model left to its default can spend the whole max_tokens
		// budget on reasoning and return an empty answer.
		{name: "reasoning_effort_sent", options: ChatOptions{ReasoningEffort: "none"}, wantEffort: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var request map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("Answer() decode request = %v, want nil", err)
				}
				writeSSEAnswer(w)
			}))
			t.Cleanup(server.Close)

			client := NewOpenAIClient("key", server.URL+"/v1", "chat", "embed", tt.options)
			if _, err := client.Answer(context.Background(), "question", nil, nil); err != nil {
				t.Fatalf("Answer() error = %v", err)
			}
			if got, ok := request["temperature"]; !ok || got != tt.wantTemp {
				t.Errorf("temperature = %#v (present %t), want %v", got, ok, tt.wantTemp)
			}
			if got := request["max_tokens"]; got != tt.wantMaxTokens {
				t.Errorf("max_tokens = %#v, want %#v", got, tt.wantMaxTokens)
			}
			if got := request["reasoning_effort"]; got != tt.wantEffort {
				t.Errorf("reasoning_effort = %#v, want %#v", got, tt.wantEffort)
			}
		})
	}
}

func TestOpenAIClient_Answer_XOnBehalfOf(t *testing.T) {
	tests := []struct {
		name        string
		forwardUser bool
		ctxUserID   string
		wantHeader  string
	}{
		{
			name:        "forward_enabled_with_user_in_ctx",
			forwardUser: true,
			ctxUserID:   "p_alice",
			wantHeader:  "p_alice",
		},
		{
			name:        "forward_disabled_with_user_in_ctx",
			forwardUser: false,
			ctxUserID:   "p_alice",
			wantHeader:  "",
		},
		{
			name:        "forward_enabled_empty_user",
			forwardUser: true,
			ctxUserID:   "",
			wantHeader:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedHeader string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedHeader = r.Header.Get("X-On-Behalf-Of")
				writeSSEAnswer(w)
			}))
			t.Cleanup(server.Close)

			client := NewOpenAIClient("key", server.URL+"/v1", "chat", "embed", ChatOptions{ForwardUser: tt.forwardUser})
			ctx := context.Background()
			if tt.ctxUserID != "" {
				ctx = withPrincipalID(ctx, tt.ctxUserID)
			}
			if _, err := client.Answer(ctx, "question", nil, nil); err != nil {
				t.Fatalf("Answer() error = %v", err)
			}
			if receivedHeader != tt.wantHeader {
				t.Errorf("X-On-Behalf-Of = %q, want %q", receivedHeader, tt.wantHeader)
			}
		})
	}
}

func TestOpenAIClient_Embed_XOnBehalfOf(t *testing.T) {
	tests := []struct {
		name        string
		forwardUser bool
		ctxUserID   string
		wantHeader  string
	}{
		{
			name:        "forward_enabled_with_user_in_ctx",
			forwardUser: true,
			ctxUserID:   "p_alice",
			wantHeader:  "p_alice",
		},
		{
			name:        "forward_disabled_with_user_in_ctx",
			forwardUser: false,
			ctxUserID:   "p_alice",
			wantHeader:  "",
		},
		{
			name:        "forward_enabled_empty_user",
			forwardUser: true,
			ctxUserID:   "",
			wantHeader:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedHeader string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedHeader = r.Header.Get("X-On-Behalf-Of")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": []map[string]any{
						{"index": 0, "embedding": []float64{0.1, 0.2}},
					},
				})
			}))
			t.Cleanup(server.Close)

			client := NewOpenAIClient("key", server.URL+"/v1", "chat", "embed", ChatOptions{ForwardUser: tt.forwardUser})
			ctx := context.Background()
			if tt.ctxUserID != "" {
				ctx = withPrincipalID(ctx, tt.ctxUserID)
			}
			if _, err := client.Embed(ctx, []string{"hello"}); err != nil {
				t.Fatalf("Embed() error = %v", err)
			}
			if receivedHeader != tt.wantHeader {
				t.Errorf("X-On-Behalf-Of = %q, want %q", receivedHeader, tt.wantHeader)
			}
		})
	}
}

// writeSSEAnswer replies the way an OpenAI-compatible backend streams with
// include_usage: content chunks, then a choices-less usage chunk.
func writeSSEAnswer(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, `data: {"id":"c","model":"served-model","choices":[{"index":0,"delta":{"content":"ans"}}]}`+"\n\n")
	_, _ = io.WriteString(w, `data: {"id":"c","model":"served-model","choices":[{"index":0,"delta":{"content":"wer"}}]}`+"\n\n")
	_, _ = io.WriteString(w, `data: {"id":"c","model":"served-model","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":2,"total_tokens":122}}`+"\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// A5 compares backends per question on the model that actually served it, its
// token counts and its TTFT. The request must ask for usage, or a streamed reply
// carries no token counts at all and the eval record reads zero.
func TestAnswerReportsServedModelUsageAndTTFT(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request = %v", err)
		}
		writeSSEAnswer(w)
	}))
	t.Cleanup(server.Close)

	client := NewOpenAIClient("key", server.URL+"/v1", "configured-model", "embed", ChatOptions{})
	got, err := client.Answer(context.Background(), "question", nil, nil)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if opts, _ := request["stream_options"].(map[string]any); request["stream"] != true || opts["include_usage"] != true {
		t.Errorf("request stream=%v stream_options=%v, want stream with include_usage", request["stream"], request["stream_options"])
	}
	if got.Text != "answer" || got.Model != "served-model" || got.PromptTokens != 120 || got.CompletionTokens != 2 {
		t.Errorf("Answer() = %+v, want text answer, served-model, 120/2 tokens", got)
	}
	if got.TTFT <= 0 || got.Duration < got.TTFT {
		t.Errorf("TTFT %v, Duration %v: want 0 < TTFT <= Duration", got.TTFT, got.Duration)
	}
}

// K2 at reasoning_effort=high can think until max_tokens and stream no content.
// That must not reach the service as text: an empty answer there is reported
// grounded=true with sources, a blank "success" instead of a retryable 503.
func TestAnswerEmptyContentIsTransient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"length"}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"c","model":"m","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":4096,"total_tokens":4216}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	client := NewOpenAIClient("key", server.URL+"/v1", "chat", "embed", ChatOptions{})
	_, err := client.Answer(context.Background(), "question", nil, nil)
	if !errors.Is(err, ErrLLMUnavailable) {
		t.Fatalf("Answer() error = %v, want ErrLLMUnavailable", err)
	}
}

// Streaming must not hide an upstream 429: the handler maps ErrLLMRateLimited to
// a 429 with the upstream's Retry-After, which the eval harness waits on.
func TestAnswerStreamingKeepsRateLimitClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	t.Cleanup(server.Close)

	client := NewOpenAIClient("key", server.URL+"/v1", "chat", "embed", ChatOptions{})
	_, err := client.Answer(context.Background(), "question", nil, nil)
	if !errors.Is(err, ErrLLMRateLimited) || RetryAfterOf(err) != "0" {
		t.Fatalf("Answer() error = %v (retry-after %q), want ErrLLMRateLimited with the upstream hint", err, RetryAfterOf(err))
	}
}

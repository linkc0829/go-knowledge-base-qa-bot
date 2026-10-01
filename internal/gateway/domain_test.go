package gateway

import (
	"encoding/json"
	"testing"
)

func TestLimiter_PerUserLimit(t *testing.T) {
	limiter := NewLimiter(10, 2)

	r1, ok, reason := limiter.TryAcquire("alice")
	if !ok {
		t.Fatalf("first acquire failed: %s", reason)
	}
	defer r1()

	r2, ok, reason := limiter.TryAcquire("alice")
	if !ok {
		t.Fatalf("second acquire failed: %s", reason)
	}
	defer r2()

	_, ok, reason = limiter.TryAcquire("alice")
	if ok {
		t.Fatal("third acquire for alice should fail")
	}
	if reason != "user_concurrency_limit" {
		t.Errorf("expected reason user_concurrency_limit, got %q", reason)
	}

	// Another user should still be able to acquire
	rBob, ok, reason := limiter.TryAcquire("bob")
	if !ok {
		t.Fatalf("bob acquire failed: %s", reason)
	}
	defer rBob()
}

func TestLimiter_GlobalLimit(t *testing.T) {
	limiter := NewLimiter(2, 5)

	r1, ok, reason := limiter.TryAcquire("u1")
	if !ok {
		t.Fatalf("u1 acquire failed: %s", reason)
	}
	defer r1()

	r2, ok, reason := limiter.TryAcquire("u2")
	if !ok {
		t.Fatalf("u2 acquire failed: %s", reason)
	}
	defer r2()

	_, ok, reason = limiter.TryAcquire("u3")
	if ok {
		t.Fatal("u3 acquire should fail when global limit reached")
	}
	if reason != "global_concurrency_limit" {
		t.Errorf("expected reason global_concurrency_limit, got %q", reason)
	}
}

func TestLimiter_ReleaseRestoresCapacity(t *testing.T) {
	limiter := NewLimiter(5, 1)

	r1, ok, _ := limiter.TryAcquire("alice")
	if !ok {
		t.Fatal("initial acquire failed")
	}

	_, ok, _ = limiter.TryAcquire("alice")
	if ok {
		t.Fatal("second acquire while in-flight should fail")
	}

	r1() // Release capacity

	r2, ok, _ := limiter.TryAcquire("alice")
	if !ok {
		t.Fatal("acquire after release failed")
	}
	defer r2()
}

func TestLimiter_IdempotentReleaseDoesNotUnderflow(t *testing.T) {
	limiter := NewLimiter(5, 2)

	r1, ok, _ := limiter.TryAcquire("alice")
	if !ok {
		t.Fatal("acquire failed")
	}

	// Repeated releases must be idempotent
	r1()
	r1()
	r1()

	global, users := limiter.Snapshot()
	if global < 0 {
		t.Fatalf("global count underflowed to %d (allows bypassing limits)", global)
	}
	if users["alice"] < 0 {
		t.Fatalf("user count underflowed to %d (allows bypassing limits)", users["alice"])
	}
}

func TestLimiter_MapCleanupOnZero(t *testing.T) {
	limiter := NewLimiter(5, 2)

	r1, ok, _ := limiter.TryAcquire("alice")
	if !ok {
		t.Fatal("acquire failed")
	}

	r1()

	_, users := limiter.Snapshot()
	if _, exists := users["alice"]; exists {
		t.Fatal("expected alice to be removed from map when count hits 0 to prevent memory leak")
	}
}

// The cap exists so one request cannot hold a backend slot for minutes (a
// reasoning model stuck thinking). Each case pins one way it could stop doing
// that, or start breaking requests it should leave alone.
func TestCapOutputTokens(t *testing.T) {
	const limit = 100
	tests := []struct {
		name        string
		body        string
		limit       int64
		fillMissing bool
		wantChanged bool
		want        map[string]string // key → JSON value after; "" means absent
	}{
		{name: "over_limit_clamped", body: `{"max_tokens":5000}`, limit: limit, wantChanged: true, want: map[string]string{"max_tokens": "100"}},
		{name: "one_over_limit_clamped", body: `{"max_tokens":101}`, limit: limit, wantChanged: true, want: map[string]string{"max_tokens": "100"}},
		// Float and exponent forms fail Int64 but upstreams accept them.
		{name: "float_over_limit_clamped", body: `{"max_tokens":5000.0}`, limit: limit, wantChanged: true, want: map[string]string{"max_tokens": "100"}},
		{name: "exponent_over_limit_clamped", body: `{"max_tokens":1e9}`, limit: limit, wantChanged: true, want: map[string]string{"max_tokens": "100"}},
		{name: "float_under_limit_kept", body: `{"max_tokens":20.0}`, limit: limit, want: map[string]string{"max_tokens": "20.0"}},
		{name: "at_limit_kept", body: `{"max_tokens":100}`, limit: limit, want: map[string]string{"max_tokens": "100"}},
		{name: "under_limit_kept", body: `{"max_tokens":20}`, limit: limit, want: map[string]string{"max_tokens": "20"}},
		{name: "max_completion_tokens_clamped", body: `{"max_completion_tokens":5000}`, limit: limit, fillMissing: true, wantChanged: true,
			want: map[string]string{"max_completion_tokens": "100", "max_tokens": ""}},
		{name: "both_fields_clamped", body: `{"max_tokens":5000,"max_completion_tokens":7000}`, limit: limit, wantChanged: true,
			want: map[string]string{"max_tokens": "100", "max_completion_tokens": "100"}},
		{name: "missing_filled_for_chat", body: `{}`, limit: limit, fillMissing: true, wantChanged: true, want: map[string]string{"max_tokens": "100"}},
		{name: "null_filled_for_chat", body: `{"max_tokens":null}`, limit: limit, fillMissing: true, wantChanged: true, want: map[string]string{"max_tokens": "100"}},
		// /v1/completions defaults to 16 tokens; filling would raise it to the cap.
		{name: "missing_kept_for_completions", body: `{}`, limit: limit, want: map[string]string{"max_tokens": ""}},
		{name: "non_integer_left_for_upstream", body: `{"max_tokens":"lots"}`, limit: limit, fillMissing: true, want: map[string]string{"max_tokens": `"lots"`}},
		{name: "disabled", body: `{"max_tokens":5000}`, limit: 0, fillMissing: true, want: map[string]string{"max_tokens": "5000"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := decodeBody([]byte(tt.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := capOutputTokens(payload, tt.limit, tt.fillMissing); got != tt.wantChanged {
				t.Errorf("changed = %v, want %v", got, tt.wantChanged)
			}
			for key, want := range tt.want {
				v, ok := payload[key]
				if want == "" {
					if ok {
						t.Errorf("%s = %v, want absent", key, v)
					}
					continue
				}
				raw, _ := json.Marshal(v)
				if string(raw) != want {
					t.Errorf("%s = %s, want %s", key, raw, want)
				}
			}
		})
	}
}

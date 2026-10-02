package gateway

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Limiter coordinates inflight request limits globally and per effective user.
// It implements zero-queue admission control: requests that cannot acquire a
// slot immediately are rejected.
//
// ponytail: in-memory counter, restart resets to zero (A2 verifies this). Multi-instance needs Redis.
type Limiter struct {
	mu             sync.Mutex
	globalInflight int
	maxGlobal      int
	userInflight   map[string]int
	maxPerUser     int
}

// NewLimiter initializes an admission limiter with global and per-user bounds.
// A limit <= 0 means unlimited.
func NewLimiter(maxGlobal, maxPerUser int) *Limiter {
	return &Limiter{
		maxGlobal:    maxGlobal,
		maxPerUser:   maxPerUser,
		userInflight: make(map[string]int),
	}
}

// TryAcquire attempts to claim one concurrent slot for user. If either the
// per-user or global limit is reached, it returns ok=false and the rejection
// reason ("user_concurrency_limit" or "global_concurrency_limit").
// When ok=true, release is guaranteed to be idempotent and thread-safe via sync.Once.
func (l *Limiter) TryAcquire(user string) (release func(), ok bool, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.maxPerUser > 0 && l.userInflight[user] >= l.maxPerUser {
		return nil, false, "user_concurrency_limit"
	}
	if l.maxGlobal > 0 && l.globalInflight >= l.maxGlobal {
		return nil, false, "global_concurrency_limit"
	}

	l.globalInflight++
	l.userInflight[user]++

	var once sync.Once
	release = func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.globalInflight > 0 {
				l.globalInflight--
			}
			if count, exists := l.userInflight[user]; exists {
				if count <= 1 {
					delete(l.userInflight, user)
				} else {
					l.userInflight[user]--
				}
			}
		})
	}
	return release, true, ""
}

// Snapshot returns the current inflight counts for diagnostics.
func (l *Limiter) Snapshot() (global int, users map[string]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	users = make(map[string]int, len(l.userInflight))
	for k, v := range l.userInflight {
		users[k] = v
	}
	return l.globalInflight, users
}

// capOutputTokens clamps max_tokens and max_completion_tokens in a decoded
// request body to limit, so one runaway generation (a reasoning model that
// never stops thinking) cannot hold a backend slot for minutes. With
// fillMissing, a body that sets neither gets max_tokens = limit. limit <= 0
// disables the cap. It reports whether payload changed, or
// ErrInvalidMaxTokens for a string the cap cannot read: vLLM coerces numeric
// strings ("90000"), so forwarding one unchecked would bypass the cap.
func capOutputTokens(payload map[string]any, limit int64, fillMissing bool) (bool, error) {
	if limit <= 0 {
		return false, nil
	}
	changed, found := false, false
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		v, ok := payload[key]
		if !ok || v == nil {
			continue
		}
		found = true
		n, numeric, err := numberValue(v)
		if err != nil {
			return false, fmt.Errorf("%s: %w", key, ErrInvalidMaxTokens)
		}
		if numeric && n > float64(limit) {
			payload[key] = limit
			changed = true
		}
	}
	if !found && fillMissing {
		payload["max_tokens"] = limit
		changed = true
	}
	return changed, nil
}

// checkSingleSequence rejects requests that make the upstream run more than
// one sequence: n > 1, or a /v1/completions prompt list. The in-flight cap
// counts HTTP requests and equals vLLM's --max-num-seqs, so one request
// fanning out to five sequences would queue inside vLLM again.
func checkSingleSequence(payload map[string]any) error {
	if v, ok := payload["n"]; ok && v != nil {
		n, numeric, err := numberValue(v)
		if err != nil || (numeric && n > 1) {
			return ErrMultipleSequences
		}
	}
	// A list of numbers is one tokenized prompt; a list of strings or of
	// token lists is several prompts.
	if prompts, ok := payload["prompt"].([]any); ok && len(prompts) > 1 {
		if _, tokenized := prompts[0].(json.Number); !tokenized {
			return ErrMultipleSequences
		}
	}
	return nil
}

// numberValue reads a JSON number, or a string the way vLLM's lax validation
// would coerce it. numeric is false for other types (bool, objects), which
// the upstream validates itself; err is set for a string that is not a number.
func numberValue(v any) (n float64, numeric bool, err error) {
	switch x := v.(type) {
	case json.Number:
		// Float64 also covers 1e9 and 100000.0, which Int64 rejects.
		n, err = x.Float64()
		return n, err == nil, nil
	case string:
		n, err = strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false, err
		}
		return n, true, nil
	}
	return 0, false, nil
}

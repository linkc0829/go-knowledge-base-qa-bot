package gateway

import (
	"encoding/json"
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
// fillMissing, a body that sets neither gets max_tokens = limit. Values that
// are not numbers are left for the upstream to reject. limit <= 0 disables
// the cap. It reports whether payload changed.
func capOutputTokens(payload map[string]any, limit int64, fillMissing bool) bool {
	if limit <= 0 {
		return false
	}
	changed, found := false, false
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		v, ok := payload[key]
		if !ok || v == nil {
			continue
		}
		found = true
		n, ok := v.(json.Number)
		if !ok {
			continue
		}
		// 1e9 and 100000.0 fail Int64 but upstreams accept them as integers,
		// so fall back to Float64 or they would bypass the cap.
		over := false
		if i, err := n.Int64(); err == nil {
			over = i > limit
		} else if f, err := n.Float64(); err == nil {
			over = f > float64(limit)
		}
		if over {
			payload[key] = limit
			changed = true
		}
	}
	if !found && fillMissing {
		payload["max_tokens"] = limit
		changed = true
	}
	return changed
}

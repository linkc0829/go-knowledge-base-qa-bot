// Command gwload is the A2 behaviour test for cmd/gateway. It starts a mock
// upstream with injectable delay, runs the real gateway binary in front of it,
// and checks the protections the gateway exists for: per-user and global
// admission, shed-not-queue, cancellation and timeout propagation, restart
// reset, and that gateway_usage agrees with what clients saw.
//
// The mock is picked per request by model name: "fast" answers at once, "slow"
// streams for -hold, "hang" never sends headers.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/linkc0829/llm-platform/internal/auth"
)

type config struct {
	gatewayBin    string
	out           string
	perUser       int
	global        int
	hold          time.Duration
	headerTimeout time.Duration
	// fastReject is the ceiling for a 429: anything slower looks like queueing.
	fastReject time.Duration
}

type check struct {
	Name   string         `json:"name"`
	Pass   bool           `json:"pass"`
	Detail string         `json:"detail"`
	Data   map[string]any `json:"data,omitempty"`
}

type report struct {
	Date   string         `json:"date"`
	Config map[string]any `json:"config"`
	Checks []check        `json:"checks"`
	Pass   bool           `json:"pass"`
}

func main() {
	var cfg config
	flag.StringVar(&cfg.gatewayBin, "gateway-bin", "", "gateway binary; built from ./cmd/gateway when empty")
	flag.StringVar(&cfg.out, "out", "", "JSON report path (default metrics/gateway-a2-<date>.json)")
	flag.IntVar(&cfg.perUser, "per-user", 2, "GATEWAY_MAX_INFLIGHT_PER_USER")
	flag.IntVar(&cfg.global, "global", 6, "GATEWAY_MAX_INFLIGHT")
	flag.DurationVar(&cfg.hold, "hold", 2*time.Second, "how long a slow stream holds its slot")
	flag.DurationVar(&cfg.headerTimeout, "header-timeout", 2*time.Second, "GATEWAY_UPSTREAM_HEADER_TIMEOUT")
	// Client-side latency includes a fresh TCP connect; queueing would show up
	// as roughly -hold, an order of magnitude above this.
	flag.DurationVar(&cfg.fastReject, "fast-reject", 250*time.Millisecond, "max latency for a 429")
	flag.Parse()
	if cfg.out == "" {
		cfg.out = filepath.Join("metrics", "gateway-a2-"+time.Now().Format("20060102")+".json")
	}

	rep, err := run(cfg)
	if err != nil {
		log.Fatalf("gwload: %v", err)
	}
	for _, c := range rep.Checks {
		mark := "PASS"
		if !c.Pass {
			mark = "FAIL"
		}
		fmt.Printf("%s  %-28s %s\n", mark, c.Name, c.Detail)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.out), 0o755); err == nil {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(cfg.out, b, 0o600); err != nil {
			log.Printf("write report: %v", err)
		} else {
			fmt.Println("report:", cfg.out)
		}
	}
	if !rep.Pass {
		os.Exit(1)
	}
}

func run(cfg config) (*report, error) {
	if cfg.global <= cfg.perUser {
		return nil, errors.New("-global must exceed -per-user, or the global check cannot tell the limits apart")
	}
	dir, err := os.MkdirTemp("", "gwload-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	if cfg.gatewayBin == "" {
		cfg.gatewayBin = filepath.Join(dir, "gateway.exe")
		//nolint:gosec // G204: local test tool; the output path is in its own temp dir.
		if out, err := exec.Command("go", "build", "-o", cfg.gatewayBin, "./cmd/gateway").CombinedOutput(); err != nil {
			return nil, fmt.Errorf("build gateway: %w\n%s", err, out)
		}
	}

	// One user per concurrent request in the global check, plus spares.
	users := cfg.global + 3
	tokens, err := writeAuthFile(filepath.Join(dir, "auth.json"), users)
	if err != nil {
		return nil, err
	}

	up := newMock(cfg.hold)
	defer up.srv.Close()

	gw := &gateway{cfg: cfg, dir: dir, upstream: up.srv.URL}
	if err := gw.start(); err != nil {
		return nil, err
	}
	defer gw.stop()

	r := &runner{cfg: cfg, gw: gw, up: up, tokens: tokens}
	checks := []check{
		r.perUserLimit(),
		r.globalLimit(),
		r.cancellationReleasesSlot(),
		r.upstreamTimeout(),
		r.restartResets(),
	}
	checks = append(checks,
		check{
			Name:   "upstream_never_over_global",
			Pass:   int(up.maxInflight.Load()) <= cfg.global,
			Detail: fmt.Sprintf("mock saw at most %d concurrent requests (global limit %d)", up.maxInflight.Load(), cfg.global),
		},
		r.logConsistency(filepath.Join(dir, "gateway.log")),
	)

	rep := &report{
		Date: time.Now().Format(time.RFC3339),
		Config: map[string]any{
			"per_user": cfg.perUser, "global": cfg.global, "hold": cfg.hold.String(),
			"header_timeout": cfg.headerTimeout.String(), "fast_reject": cfg.fastReject.String(),
		},
		Checks: checks,
		Pass:   true,
	}
	for _, c := range checks {
		rep.Pass = rep.Pass && c.Pass
	}
	return rep, nil
}

func writeAuthFile(path string, n int) ([]string, error) {
	store, err := auth.NewBootstrapStore(path, zap.NewNop())
	if err != nil {
		return nil, err
	}
	tokens := make([]string, n)
	for i := range tokens {
		_, tok, err := store.CreateToken(context.Background(), "gwload", auth.TokenSpec{Name: fmt.Sprintf("gwload-u%d", i)})
		if err != nil {
			return nil, fmt.Errorf("create token: %w", err)
		}
		tokens[i] = tok
	}
	return tokens, nil
}

// --- mock upstream ---------------------------------------------------------

type mock struct {
	srv         *httptest.Server
	hold        time.Duration
	inflight    atomic.Int64
	maxInflight atomic.Int64
	canceled    atomic.Int64 // requests the upstream saw cancelled before finishing
}

func newMock(hold time.Duration) *mock {
	m := &mock{hold: hold}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	return m
}

func (m *mock) serve(w http.ResponseWriter, r *http.Request) {
	n := m.inflight.Add(1)
	defer m.inflight.Add(-1)
	for {
		old := m.maxInflight.Load()
		if n <= old || m.maxInflight.CompareAndSwap(old, n) {
			break
		}
	}

	var req struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	usage := `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`

	switch req.Model {
	case "hang":
		<-r.Context().Done()
		m.canceled.Add(1)
	case "slow":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		deadline := time.After(m.hold)
		for {
			select {
			case <-r.Context().Done():
				m.canceled.Add(1)
				return
			case <-deadline:
				fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":%s}\n\ndata: [DONE]\n\n", usage)
				fl.Flush()
				return
			case <-tick.C:
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
				fl.Flush()
			}
		}
	default:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"fast","choices":[],"usage":%s}`, usage)
	}
}

// waitIdle waits for the mock to drain; a request still open afterwards is an
// orphan holding upstream capacity.
func (m *mock) waitIdle(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if m.inflight.Load() == 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return m.inflight.Load() == 0
}

// --- gateway process -------------------------------------------------------

type gateway struct {
	cfg      config
	dir      string
	upstream string
	url      string
	cmd      *exec.Cmd
}

func (g *gateway) start() error {
	port, err := freePort()
	if err != nil {
		return err
	}
	g.url = fmt.Sprintf("http://127.0.0.1:%d", port)
	cmd := exec.Command(g.cfg.gatewayBin) //nolint:gosec // G204: running the operator-chosen gateway binary is the point.
	// Runs in the temp dir so the repo's .env is not read; inherited GATEWAY_,
	// KB_ and LOG_ variables are dropped so the user's shell cannot skew limits.
	cmd.Dir = g.dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GATEWAY_") && !strings.HasPrefix(kv, "KB_") && !strings.HasPrefix(kv, "LOG_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env,
		"GATEWAY_UPSTREAM_BASE_URL="+g.upstream,
		"GATEWAY_UPSTREAM_API_KEY=mock",
		"KB_AUTH_FILE="+filepath.Join(g.dir, "auth.json"),
		fmt.Sprintf("GATEWAY_PORT=%d", port),
		fmt.Sprintf("GATEWAY_MAX_INFLIGHT=%d", g.cfg.global),
		fmt.Sprintf("GATEWAY_MAX_INFLIGHT_PER_USER=%d", g.cfg.perUser),
		"GATEWAY_UPSTREAM_HEADER_TIMEOUT="+g.cfg.headerTimeout.String(),
		"GATEWAY_LOG_OUTPUT="+filepath.Join(g.dir, "gateway.log"),
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start gateway: %w", err)
	}
	g.cmd = cmd
	for i := 0; i < 100; i++ {
		if resp, err := http.Get(g.url + "/healthz"); err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("gateway did not become healthy within 10s")
}

func (g *gateway) stop() {
	if g.cmd != nil && g.cmd.Process != nil {
		_ = g.cmd.Process.Kill()
		_ = g.cmd.Wait()
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// --- client side -------------------------------------------------------------

type result struct {
	status   int
	latency  time.Duration // time to response headers
	reason   string        // "error" field of a non-200 body
	retry    string
	complete bool // 200 and the whole body read: the gateway will log it as 200
	err      error
}

type runner struct {
	cfg    config
	gw     *gateway
	up     *mock
	tokens []string

	mu        sync.Mutex
	completed int // client-side 200s that finished, for the log check
	rejected  int // 429s
	timeouts  int // 504s
	canceled  int // requests the client cancelled after the gateway admitted them
}

func (r *runner) call(ctx context.Context, user int, model string) result {
	stream := model == "slow"
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":%t}`, model, stream)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.gw.url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.tokens[user])
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return result{err: err, latency: time.Since(start)}
	}
	defer resp.Body.Close()
	res := result{status: resp.StatusCode, latency: time.Since(start), retry: resp.Header.Get("Retry-After")}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		res.reason = e.Error
		r.mu.Lock()
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			r.rejected++
		case http.StatusGatewayTimeout:
			r.timeouts++
		}
		r.mu.Unlock()
		return res
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		res.err = err
		return res
	}
	res.complete = true
	r.mu.Lock()
	r.completed++
	r.mu.Unlock()
	return res
}

// burst fires one request per entry in users at the same moment.
func (r *runner) burst(users []int, model string) []result {
	out := make([]result, len(users))
	var wg sync.WaitGroup
	for i, u := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = r.call(context.Background(), u, model)
		}()
	}
	wg.Wait()
	return out
}

func (r *runner) perUserLimit() check {
	users := make([]int, r.cfg.perUser+3)
	return r.admission("per_user_limit", r.burst(users, "slow"), r.cfg.perUser, "user_concurrency_limit")
}

func (r *runner) globalLimit() check {
	users := make([]int, r.cfg.global+3)
	for i := range users {
		users[i] = i
	}
	return r.admission("global_limit", r.burst(users, "slow"), r.cfg.global, "global_concurrency_limit")
}

// admission checks a burst: exactly want requests admitted, every other one
// rejected fast with Retry-After and the expected reason.
func (r *runner) admission(name string, res []result, want int, reason string) check {
	var ok, rej int
	var maxRej, minOK time.Duration
	var problems []string
	for _, x := range res {
		switch {
		case x.status == http.StatusOK && x.complete:
			ok++
			if total := x.latency; minOK == 0 || total < minOK {
				minOK = total
			}
		case x.status == http.StatusTooManyRequests:
			rej++
			maxRej = max(maxRej, x.latency)
			if x.retry == "" {
				problems = append(problems, "429 without Retry-After")
			}
			if x.reason != reason {
				problems = append(problems, "429 reason "+x.reason)
			}
		default:
			problems = append(problems, fmt.Sprintf("unexpected status=%d err=%v", x.status, x.err))
		}
	}
	pass := ok == want && rej == len(res)-want && maxRej <= r.cfg.fastReject && len(problems) == 0
	return check{
		Name: name,
		Pass: pass,
		Detail: fmt.Sprintf("%d sent: %d admitted (want %d), %d rejected, slowest 429 %s (limit %s)",
			len(res), ok, want, rej, maxRej.Round(time.Millisecond), r.cfg.fastReject),
		Data: map[string]any{"admitted": ok, "rejected": rej, "max_reject_ms": maxRej.Milliseconds(), "problems": problems},
	}
}

// cancellationReleasesSlot: a client that gives up must free its slot and the
// upstream request, or abandoned calls would slowly lock a user out.
func (r *runner) cancellationReleasesSlot() check {
	const user = 1
	before := r.up.canceled.Load()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < r.cfg.perUser; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.call(ctx, user, "slow")
		}()
	}
	time.Sleep(300 * time.Millisecond) // streams are open and holding both slots
	cancel()
	wg.Wait()
	r.mu.Lock()
	r.canceled += r.cfg.perUser
	r.mu.Unlock()

	idle := r.up.waitIdle(2 * time.Second)
	propagated := r.up.canceled.Load() - before
	res := r.burstUser(user, r.cfg.perUser, "fast")
	var ok int
	for _, x := range res {
		if x.status == http.StatusOK {
			ok++
		}
	}
	return check{
		Name: "cancellation_releases_slot",
		Pass: idle && propagated == int64(r.cfg.perUser) && ok == r.cfg.perUser,
		Detail: fmt.Sprintf("%d streams cancelled: upstream saw %d cancellations, drained=%t; next %d requests admitted %d",
			r.cfg.perUser, propagated, idle, r.cfg.perUser, ok),
	}
}

func (r *runner) burstUser(user, n int, model string) []result {
	users := make([]int, n)
	for i := range users {
		users[i] = user
	}
	return r.burst(users, model)
}

// upstreamTimeout: a backend that never answers must become a 504 near the
// header timeout, with the upstream call cancelled rather than left running.
func (r *runner) upstreamTimeout() check {
	before := r.up.canceled.Load()
	x := r.call(context.Background(), 2, "hang")
	idle := r.up.waitIdle(2 * time.Second)
	near := x.latency >= r.cfg.headerTimeout && x.latency < r.cfg.headerTimeout+time.Second
	return check{
		Name: "upstream_timeout",
		Pass: x.status == http.StatusGatewayTimeout && x.reason == "upstream_timeout" && near && idle && r.up.canceled.Load()-before == 1,
		Detail: fmt.Sprintf("status %d %q after %s (timeout %s), upstream cancelled=%d drained=%t",
			x.status, x.reason, x.latency.Round(time.Millisecond), r.cfg.headerTimeout, r.up.canceled.Load()-before, idle),
	}
}

// restartResets: in-flight counters live in memory, so a restart must start
// from zero instead of locking out users whose requests died with the process.
func (r *runner) restartResets() check {
	const user = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < r.cfg.perUser; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Dies with the process: never logged, never counted as complete.
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.gw.url+"/v1/chat/completions",
				strings.NewReader(`{"model":"slow","stream":true}`))
			req.Header.Set("Authorization", "Bearer "+r.tokens[user])
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	r.gw.stop()
	wg.Wait()
	idle := r.up.waitIdle(2 * time.Second)
	if err := r.gw.start(); err != nil {
		return check{Name: "restart_resets", Detail: err.Error()}
	}
	res := r.burstUser(user, r.cfg.perUser, "fast")
	var ok int
	for _, x := range res {
		if x.status == http.StatusOK {
			ok++
		}
	}
	return check{
		Name:   "restart_resets",
		Pass:   ok == r.cfg.perUser && idle,
		Detail: fmt.Sprintf("killed gateway with %d streams open: upstream drained=%t; after restart %d/%d admitted", r.cfg.perUser, idle, ok, r.cfg.perUser),
	}
}

// logConsistency: A3 bills from gateway_usage and A2 counts 429s from it, so
// every outcome a client saw must be there, and nothing extra.
func (r *runner) logConsistency(path string) check {
	r.mu.Lock()
	want := map[int]int{200: r.completed, 429: r.rejected, 504: r.timeouts, 499: r.canceled}
	r.mu.Unlock()
	// The gateway writes gateway_usage after the response ends, so the last
	// request's line can land just after the client is done (CI saw 11/12).
	var counts map[int]int
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var err error
		if counts, err = usageCounts(path); err != nil {
			return check{Name: "log_consistency", Detail: err.Error()}
		}
		if maps.Equal(counts, want) || time.Now().After(deadline) {
			break
		}
	}
	pass := true
	var parts []string
	for _, s := range []int{200, 429, 499, 504} {
		pass = pass && counts[s] == want[s]
		parts = append(parts, fmt.Sprintf("%d: log %d / client %d", s, counts[s], want[s]))
	}
	return check{
		Name:   "log_consistency",
		Pass:   pass,
		Detail: strings.Join(parts, ", "),
		Data:   map[string]any{"log": counts, "client": want},
	}
}

func usageCounts(path string) (map[int]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	counts := map[int]int{200: 0, 429: 0, 499: 0, 504: 0}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var line struct {
			Msg    string `json:"msg"`
			Status int    `json:"status"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Msg == "gateway_usage" {
			counts[line.Status]++
		}
	}
	return counts, sc.Err()
}

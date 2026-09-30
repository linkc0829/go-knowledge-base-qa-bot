//go:build chateval

// Package-internal harness that replays the support eval against the real
// corpus, the real embedder and the real chat model.
//
// It calls Service.ChatWithMetrics directly rather than POSTing to /chat so it
// can name the principal it is testing. That matters: the published 285/287 was
// measured as "support" (all teams, engineering off), and the only other way to
// run without a token is KB_AUTH_DISABLED, whose principal is full access — it
// would let engineering sections into the context and quietly measure something
// else.
//
// Each question gets its own session, matching how the number was produced;
// composeQuery therefore never sees history here.
//
//	go test ./internal/kb/ -tags chateval -run TestChatEval -timeout 90m -v
//
// KB_CHAT_EVAL_RPM     requests per minute (default 12; the chat model 429s above that)
// KB_CHAT_EVAL_OUT     write per-question JSON results here
// KB_CHAT_EVAL_AREAS   comma-separated subdirectories of eval/<team>/eval (default ADMIN,POS)
package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/linkc0829/llm-platform/internal/platform/config"
	"github.com/linkc0829/llm-platform/internal/shared"
)

const (
	chatEvalMaxAttempts    = 6
	chatEvalBackoffSeconds = 5
)

type chatEvalFile struct {
	ModuleCode string `yaml:"module_code"`
	Questions  []struct {
		Question          string   `yaml:"question"`
		ExpectSourceID    string   `yaml:"expect_source_id"`
		ExpectIdentifiers []string `yaml:"expect_identifiers"`
		MustNotInfer      bool     `yaml:"must_not_infer"`
	} `yaml:"questions"`
}

type chatEvalResult struct {
	Area         string   `json:"area"`
	Module       string   `json:"module"`
	Question     string   `json:"q"`
	MustNotInfer bool     `json:"mni"`
	Grounded     bool     `json:"grounded"`
	ExpectID     string   `json:"expect_source_id"`
	GotIDs       []string `json:"got_source_ids"`
	Pass         bool     `json:"pass"`
	Hit1         bool     `json:"hit1"`
	Reason       string   `json:"reason,omitempty"`
}

func TestChatEval(t *testing.T) {
	t.Chdir("../..")
	cfg, err := config.LoadKB()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if strings.EqualFold(cfg.OpenAI.LLMMode, "fake") {
		t.Fatal("KB_LLM_MODE=fake cannot evaluate answers")
	}
	oai := NewOpenAIClient(cfg.OpenAI.APIKey, cfg.OpenAI.BaseURL, cfg.OpenAI.ChatModel, cfg.OpenAI.EmbedModel,
		ChatOptions{Temperature: cfg.OpenAI.ChatTemperature, MaxTokens: cfg.OpenAI.ChatMaxTokens,
			ReasoningEffort: cfg.OpenAI.ChatReasoningEffort})
	svc := NewService(NewMarkdownRepo(cfg.KB.DocsDir, cfg.KB.IndexDir), oai, oai, NewVectorRepo(cfg.KB.IndexDir), NewInProcStore(), cfg.OpenAI.EmbedModel)

	ctx := context.Background()
	if err := svc.LoadOnStartup(ctx); err != nil {
		t.Fatalf("LoadOnStartup() error = %v — run make import and POST /index first", err)
	}
	indexed, _, vectors, ready := svc.indexSnapshot()
	if !ready {
		t.Fatal("index not ready")
	}
	t.Logf("sections: %d, vectors: %d", len(indexed), len(vectors))

	// expect_source_id names a document; answers cite sections. Resolve through
	// the section metadata rather than re-reading kb_index.json, so the mapping
	// is the one retrieval actually served.
	idOf := map[string]string{}
	for _, section := range indexed {
		idOf[section.Citation()] = strings.TrimSpace(section.Meta()["id"])
	}

	// Matches auth.json's "support": every team, engineering off.
	support := shared.Principal{ID: "p_chat_eval_support", Name: "eval-support", AllTeams: true}

	// The eval tree is not part of runtime config -- it is a kbimport flag --
	// so it is named here rather than guessed from KBConfig.
	evalDir := strings.TrimSpace(os.Getenv("KB_CHAT_EVAL_DIR"))
	if evalDir == "" {
		evalDir = filepath.Join("eval", "Store.POS")
	}
	cases := loadChatEvalCases(t, evalDir)
	if len(cases) == 0 {
		t.Fatal("no eval questions found")
	}
	// A full pass is ~25 minutes of paced model calls; the limit exists to prove
	// the harness works before spending that.
	if raw := os.Getenv("KB_CHAT_EVAL_LIMIT"); raw != "" {
		if limit, err := strconv.Atoi(raw); err == nil && limit > 0 && limit < len(cases) {
			t.Logf("KB_CHAT_EVAL_LIMIT=%d — PARTIAL RUN, not comparable to the published total", limit)
			cases = cases[:limit]
		}
	}
	t.Logf("questions: %d", len(cases))

	rpm := 12
	if raw := os.Getenv("KB_CHAT_EVAL_RPM"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			rpm = parsed
		}
	}
	interval := time.Minute / time.Duration(rpm)
	t.Logf("pacing: %d req/min (%v apart)", rpm, interval)

	results := make([]chatEvalResult, 0, len(cases))
	for i, tc := range cases {
		if i > 0 {
			time.Sleep(interval)
		}
		answer := chatEvalAsk(t, ctx, svc, support, tc.Question)
		got := make([]string, 0, len(answer.Sources()))
		for _, citation := range answer.Sources() {
			got = append(got, idOf[citation.String()])
		}

		result := chatEvalResult{
			Area: tc.Area, Module: tc.Module, Question: tc.Question,
			MustNotInfer: tc.MustNotInfer, Grounded: answer.Grounded(),
			ExpectID: tc.ExpectSourceID, GotIDs: got,
		}
		switch {
		case tc.MustNotInfer:
			result.Pass = !answer.Grounded()
			if !result.Pass {
				result.Reason = "answered a question the corpus should not support"
			}
		case !answer.Grounded():
			result.Reason = "declined"
		case tc.ExpectSourceID == "":
			result.Pass = true
		default:
			for rank, id := range got {
				if id == tc.ExpectSourceID {
					result.Pass = true
					result.Hit1 = rank == 0
					break
				}
			}
			if !result.Pass {
				result.Reason = "expected source absent from citations"
			}
		}
		results = append(results, result)

		if (i+1)%25 == 0 || i+1 == len(cases) {
			passed := 0
			for _, r := range results {
				if r.Pass {
					passed++
				}
			}
			// fmt, not t.Logf: go test buffers a running test's log output until
			// it finishes, which leaves a 45-minute run with no visible progress.
			fmt.Printf("progress %d/%d — passing %d\n", i+1, len(cases), passed)
		}
	}

	reportChatEval(t, results)
}

// chatEvalAsk retries upstream rate limiting rather than scoring it as a miss.
func chatEvalAsk(t *testing.T, ctx context.Context, svc *Service, principal shared.Principal, question string) Answer {
	t.Helper()
	var lastErr error
	for attempt := 1; attempt <= chatEvalMaxAttempts; attempt++ {
		answer, _, _, err := svc.ChatWithMetrics(ctx, principal, question, "")
		if err == nil {
			return answer
		}
		lastErr = err
		wait := time.Duration(chatEvalBackoffSeconds*attempt) * time.Second
		t.Logf("retry %d/%d after %v: %v", attempt, chatEvalMaxAttempts, wait, err)
		time.Sleep(wait)
	}
	t.Fatalf("Chat(%q) failed after %d attempts: %v", question, chatEvalMaxAttempts, lastErr)
	return Answer{}
}

type chatEvalCase struct {
	Area, Module, Question, ExpectSourceID string
	MustNotInfer                           bool
}

func loadChatEvalCases(t *testing.T, evalDir string) []chatEvalCase {
	t.Helper()
	areas := []string{"ADMIN", "POS"}
	if raw := strings.TrimSpace(os.Getenv("KB_CHAT_EVAL_AREAS")); raw != "" {
		areas = strings.Split(raw, ",")
	}
	var cases []chatEvalCase
	for _, area := range areas {
		area = strings.TrimSpace(area)
		dir := filepath.Join(evalDir, "eval", area)
		entries, err := filepath.Glob(filepath.Join(dir, "*-eval.yaml"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s has no *-eval.yaml — is KB_EVAL_DIR pointing at the team directory?", dir)
		}
		sort.Strings(entries)
		for _, path := range entries {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			var parsed chatEvalFile
			if err := yaml.Unmarshal(b, &parsed); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, q := range parsed.Questions {
				cases = append(cases, chatEvalCase{
					Area: area, Module: parsed.ModuleCode, Question: q.Question,
					ExpectSourceID: q.ExpectSourceID, MustNotInfer: q.MustNotInfer,
				})
			}
		}
	}
	return cases
}

func reportChatEval(t *testing.T, results []chatEvalResult) {
	t.Helper()
	if path := strings.TrimSpace(os.Getenv("KB_CHAT_EVAL_OUT")); path != "" {
		b, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			t.Fatalf("marshal results: %v", err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s", path)
	}

	passed, hit1, sourced, mni, mniPass := 0, 0, 0, 0, 0
	byArea := map[string][2]int{}
	for _, r := range results {
		stat := byArea[r.Area]
		stat[1]++
		if r.Pass {
			passed++
			stat[0]++
		}
		byArea[r.Area] = stat
		if r.MustNotInfer {
			mni++
			if r.Pass {
				mniPass++
			}
			continue
		}
		if r.ExpectID != "" {
			sourced++
			if r.Hit1 {
				hit1++
			}
		}
	}

	fmt.Printf("\n==== CHAT EVAL (support principal) ====\n")
	fmt.Printf("questions        : %d\n", len(results))
	fmt.Printf("passing          : %d / %d\n", passed, len(results))
	if sourced > 0 {
		fmt.Printf("hit@1            : %d / %d (%.1f%%)\n", hit1, sourced, 100*float64(hit1)/float64(sourced))
	}
	fmt.Printf("must_not_infer   : %d / %d\n", mniPass, mni)
	areas := make([]string, 0, len(byArea))
	for area := range byArea {
		areas = append(areas, area)
	}
	sort.Strings(areas)
	for _, area := range areas {
		fmt.Printf("%-16s : %d / %d\n", area, byArea[area][0], byArea[area][1])
	}
	fmt.Printf("---- failures ----\n")
	for _, r := range results {
		if !r.Pass {
			fmt.Printf("[%s/%s] %s\n    want %s  got %v  (%s)\n",
				r.Area, r.Module, r.Question, r.ExpectID, r.GotIDs, r.Reason)
		}
	}
	fmt.Printf("=======================================\n")
}

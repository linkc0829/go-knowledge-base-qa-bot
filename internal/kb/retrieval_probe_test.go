//go:build retrievalprobe

// Package-internal probe that measures retrieval alone, with no LLM in the loop.
//
// Why this exists: /chat and search_kb both report sources from the Answer, and
// an ungrounded answer now carries none — so the questions we most want to
// diagnose ("how do I check out?", which the model refuses) return an empty
// source list and tell us nothing about what was retrieved. This replays the
// exact ranking pipeline from Service.chat and prints the top-k anchors, which
// is deterministic and free of weak-model noise.
//
// Not part of `make verify`: it needs a built .kb index and a live embedder.
//
//	go test ./internal/kb/ -tags retrievalprobe -run TestRetrievalProbe -v
package kb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/linkc0829/llm-platform/internal/platform/config"
)

// probeQueries are the "how do I X" questions that motivated the change: every
// one of them should retrieve its module's step sections, not the doc boilerplate.
//
// wantArea is matched against Section.File(), which under the modular export is
// "<team>/procedures/<page>/<module>-procedure.md". The pre-modular names
// ("01_Login", "02_Main_Menu", ...) no longer occur in any path, so leaving them
// here silently reported areaHits=0 for every query — retrieval looked dead when
// only the expectation was stale. Keep these in step with the exporter's layout.
//
// The list below is the POS bundle's. Any other bundle needs its own, so set
// KB_RETRIEVAL_PROBE_QUERIES to a JSON file of [{"query":…,"wantArea":…}] —
// without that, probing a different corpus means editing this file, and the
// stale-fixture guard below turns the whole probe into a hard failure.
type probeQuery struct {
	Query    string `json:"query"`
	WantArea string `json:"wantArea"`
	// WantAnchor names the section that should answer the query. Set it to get
	// per-channel ranks: the fused list alone cannot tell you whether BM25 or the
	// vector side is the one failing, and "the file ranked" is not the same as
	// "the answering section ranked".
	WantAnchor string `json:"wantAnchor"`
	// Prior is the questions asked earlier in the same session, oldest first.
	//
	// Service.chat does not rank the query the caller sent: composeQuery
	// concatenates up to five previous questions in front of it and the result
	// feeds both the BM25 tokens and the embedding. Leaving that out meant every
	// number this probe (and the evals, which ask each question in its own
	// session) has ever produced described single-turn retrieval only, while real
	// sessions rank a query the probe never saw. Empty keeps the old behaviour.
	Prior []string `json:"prior"`
	// Shape groups cases in the summary ("follow-up", "topic-switch",
	// "anaphora", ...). Dilution and topic-stickiness fail differently, so one
	// pooled hit rate would average away the only thing worth seeing.
	Shape string `json:"shape"`
}

// priorTurns adapts the probe's plain strings to what composeQuery consumes.
// Only Query is read there, so the answers can stay empty.
func priorTurns(prior []string) []Turn {
	turns := make([]Turn, 0, len(prior))
	for _, q := range prior {
		turns = append(turns, Turn{Query: q})
	}
	return turns
}

// rankOf reports the 1-based position of the first section whose anchor contains
// want, or 0 when absent from the list.
func rankOf(list []ScoredSection, indexed []Section, want string) int {
	if want == "" {
		return 0
	}
	for i, s := range list {
		if s.Index >= 0 && s.Index < len(indexed) &&
			strings.Contains(indexed[s.Index].Anchor(), want) {
			return i + 1
		}
	}
	return 0
}

func rankOfSection(list []Section, want string) int {
	if want == "" {
		return 0
	}
	for i, s := range list {
		if strings.Contains(s.Anchor(), want) {
			return i + 1
		}
	}
	return 0
}

func loadProbeQueries(t *testing.T) []probeQuery {
	path := os.Getenv("KB_RETRIEVAL_PROBE_QUERIES")
	if path == "" {
		return defaultProbeQueries
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read KB_RETRIEVAL_PROBE_QUERIES=%s: %v", path, err)
	}
	var out []probeQuery
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s has no queries", path)
	}
	t.Logf("probe queries from %s: %d", path, len(out))
	return out
}

var defaultProbeQueries = []probeQuery{
	{Query: "如何執行登入?", WantArea: "POS/login-procedure.md"},
	{Query: "如何執行主選單?", WantArea: "POS/main_menu-procedure.md"},
	{Query: "如何執行點餐?", WantArea: "POS/ordering-procedure.md"},
	{Query: "如何執行套餐點餐?", WantArea: "POS/set_meal_ordering-procedure.md"},
	{Query: "如何執行訂單管理?", WantArea: "POS/order_management-procedure.md"},
	{Query: "如何執行單據重印?", WantArea: "POS/receipt_reprint-procedure.md"},
	{Query: "如何執行作廢?", WantArea: "POS/void-procedure.md"},
	{Query: "如何執行營業報表?", WantArea: "POS/business_reports-procedure.md"},
	{Query: "如何執行周邊管理?", WantArea: "POS/peripheral_management-procedure.md"},
	{Query: "如何結帳?", WantArea: ""},
	{Query: "如何用現金付款?", WantArea: ""},
	{Query: "如何作廢訂單?", WantArea: ""},
	{Query: "如何重印發票?", WantArea: ""},
	{Query: "怎麼看營業報表?", WantArea: ""},
	{Query: "如何暫存訂單?", WantArea: ""},
	{Query: "套餐訂單怎麼點?", WantArea: ""},
}

func TestRetrievalProbe(t *testing.T) {
	// go test runs in the package dir; config.LoadKB and KB_*_DIR are relative to the repo root.
	t.Chdir("../..")
	cfg, err := config.LoadKB()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if strings.EqualFold(cfg.OpenAI.LLMMode, "fake") {
		t.Fatal("KB_LLM_MODE=fake cannot probe retrieval embeddings")
	}
	oai := NewOpenAIClient(cfg.OpenAI.APIKey, cfg.OpenAI.BaseURL, cfg.OpenAI.ChatModel, cfg.OpenAI.EmbedModel,
		ChatOptions{Temperature: cfg.OpenAI.ChatTemperature, MaxTokens: cfg.OpenAI.ChatMaxTokens})
	svc := NewService(NewMarkdownRepo(cfg.KB.DocsDir, cfg.KB.IndexDir), nil, oai, NewVectorRepo(cfg.KB.IndexDir), NewInProcStore(), cfg.OpenAI.EmbedModel)

	ctx := context.Background()
	if err := svc.LoadOnStartup(ctx); err != nil {
		if errors.Is(err, ErrNotIndexed) || errors.Is(err, ErrIndexStale) {
			t.Log("index missing or stale — running svc.Index...")
			if _, _, err := svc.Index(ctx); err != nil {
				t.Fatalf("svc.Index() error = %v", err)
			}
		} else {
			t.Fatalf("LoadOnStartup() error = %v — build the index first (make import, then POST /index)", err)
		}
	}
	indexed, corpus, vecMap, ready := svc.indexSnapshot()
	if !ready {
		t.Fatal("index not ready — run POST /index first")
	}
	t.Logf("indexed sections: %d, vectors: %d", len(indexed), len(vecMap))
	if evalPath := os.Getenv("KB_RETRIEVAL_PROBE_EVAL_OUT"); evalPath != "" {
		runEvalRetrievalProbe(t, ctx, oai, indexed, corpus, vecMap, evalPath)
		return
	}

	// Fail loudly on a stale wantArea. Without this a renamed layout just drives
	// areaHits to 0, which reads as "retrieval is broken" instead of "the
	// expectation no longer names a real file".
	probeQueries := loadProbeQueries(t)
	for _, tc := range probeQueries {
		if tc.WantArea == "" {
			continue
		}
		found := false
		for _, sec := range indexed {
			if strings.Contains(sec.File(), tc.WantArea) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("probeQueries wantArea %q matches no indexed file — update it to the current export layout", tc.WantArea)
		}
	}

	type shapeStat struct{ hits, asked int }
	shapes := map[string]*shapeStat{}
	shapeOrder := []string{}

	var stepHits, areaHits, boilerplate, total int
	for _, tc := range probeQueries {
		// Mirror Service.chat exactly: same composed query, same candidate
		// trimming, same fusion, same k.
		retrievalQuery := composeQuery(priorTurns(tc.Prior), tc.Query)
		allow := func(section Section) bool {
			return CanSee(FullAccessPrincipal(AnonymousOwner), section)
		}
		bm25List := corpus.RankBM25(tokenize(retrievalQuery))
		bm25List = filterRankedSections(indexed, bm25List, allow)
		for len(bm25List) > 0 && bm25List[len(bm25List)-1].Score <= 0 {
			bm25List = bm25List[:len(bm25List)-1]
		}
		var bmL1, bmL0 []ScoredSection
		for _, item := range bm25List {
			if indexed[item.Index].Meta()["doc_type"] == "distilled" {
				bmL1 = append(bmL1, item)
			} else {
				bmL0 = append(bmL0, item)
			}
		}
		var vecList []ScoredSection
		if vectors, err := oai.Embed(ctx, []string{retrievalQuery}); err == nil && len(vectors) > 0 {
			vecList = RankVector(indexed, vecMap, vectors[0], len(indexed), allow)
		} else if err != nil {
			t.Fatalf("Embed(%q) error = %v — is the embedder reachable?", retrievalQuery, err)
		}

		poolRes := PartitionAndRankPools(indexed, bm25List, vecList, topK, candidateK, rrfK, minThreshold)

		expanded, err := ExpandDistilled(
			poolRes.TopL1,
			poolRes.TopL0,
			indexed,
			poolRes.RankedL0,
			allow,
			l1K,
			candidateK,
			budget,
		)
		if err != nil {
			t.Fatalf("ExpandDistilled error: %v", err)
		}

		var anchors []string
		hitStep, hitArea := false, false
		for _, sec := range expanded {
			total++
			anchors = append(anchors, sec.Anchor())
			if strings.Contains(sec.Anchor(), "步驟") {
				hitStep = true
			}
			if tc.WantArea != "" && strings.Contains(sec.File(), tc.WantArea) {
				hitArea = true
			}
			// The chunks that currently crowd out real steps.
			if a := sec.Anchor(); strings.Contains(a, "適用範圍") || strings.Contains(a, "操作程序") ||
				strings.Contains(a, "證據說明") || strings.Contains(a, "例外情境") || strings.Contains(a, "前置條件") {
				boilerplate++
			}
		}
		if hitStep {
			stepHits++
		}
		if hitArea {
			areaHits++
		}
		if tc.WantArea != "" {
			shape := tc.Shape
			if shape == "" {
				shape = "single-turn"
			}
			if _, ok := shapes[shape]; !ok {
				shapes[shape] = &shapeStat{}
				shapeOrder = append(shapeOrder, shape)
			}
			shapes[shape].asked++
			if hitArea {
				shapes[shape].hits++
			}
		}
		if tc.WantAnchor != "" {
			t.Logf("[bm25=%-3d vec=%-3d l0fused=%-3d expanded=%-3d] %s  (want %q, l1 hits=%d)",
				rankOf(poolRes.BML0, indexed, tc.WantAnchor),
				rankOf(poolRes.VecL0, indexed, tc.WantAnchor),
				rankOf(poolRes.RankedL0, indexed, tc.WantAnchor),
				rankOfSection(expanded, tc.WantAnchor),
				tc.Query, tc.WantAnchor, len(poolRes.TopL1))
		}
		// The two numbers Service.chat denies on. They decide whether a query
		// retrieves anything on its own, which is what any "should this turn
		// carry context?" rule has to key off — and they were invisible here.
		t.Logf("[gate bm25=%6.2f cos=%.3f deny=%-5v] %-14s %s",
			poolRes.GateBM25, poolRes.GateCosine,
			poolRes.GateBM25 < minThreshold && poolRes.GateCosine < cosineMin, tc.Shape, tc.Query)
		if len(tc.Prior) > 0 {
			// The string that was actually ranked, not the one that was asked.
			t.Logf("[%s] ranked %q", tc.Shape, retrievalQuery)
		}
		t.Logf("[step=%-5v area=%-5v] %-22s -> %v", hitStep, hitArea, tc.Query, anchors)
	}

	areaAsked := 0
	for _, tc := range probeQueries {
		if tc.WantArea != "" {
			areaAsked++
		}
	}
	fmt.Printf("\n==== RETRIEVAL PROBE ====\n")
	fmt.Printf("queries                      : %d\n", len(probeQueries))
	fmt.Printf("with a 步驟 section in top-%d : %d / %d\n", topK, stepHits, len(probeQueries))
	fmt.Printf("reaching the expected area   : %d / %d\n", areaHits, areaAsked)
	fmt.Printf("boilerplate slots of %d      : %d\n", total, boilerplate)
	if len(shapeOrder) > 1 {
		fmt.Printf("-- by shape --\n")
		for _, shape := range shapeOrder {
			s := shapes[shape]
			fmt.Printf("%-28s : %d / %d\n", shape, s.hits, s.asked)
		}
	}
	fmt.Printf("=========================\n")
}

func TestDistilledChatLive(t *testing.T) {
	t.Chdir("../..")
	cfg, err := config.LoadKB()
	if err != nil {
		t.Fatalf("config.LoadKB() error = %v", err)
	}

	oai := NewOpenAIClient(cfg.OpenAI.APIKey, cfg.OpenAI.BaseURL, cfg.OpenAI.ChatModel, cfg.OpenAI.EmbedModel,
		ChatOptions{Temperature: cfg.OpenAI.ChatTemperature, MaxTokens: cfg.OpenAI.ChatMaxTokens,
			ReasoningEffort: cfg.OpenAI.ChatReasoningEffort})
	svc := NewService(NewMarkdownRepo(cfg.KB.DocsDir, cfg.KB.IndexDir), oai, oai, NewVectorRepo(cfg.KB.IndexDir), NewInProcStore(), cfg.OpenAI.EmbedModel)

	ctx := context.Background()
	if err := svc.LoadOnStartup(ctx); err != nil {
		if errors.Is(err, ErrNotIndexed) || errors.Is(err, ErrIndexStale) {
			t.Log("re-indexing corpus...")
			if _, _, err := svc.Index(ctx); err != nil {
				t.Fatalf("svc.Index() error = %v", err)
			}
		} else {
			t.Fatalf("LoadOnStartup() error = %v", err)
		}
	}

	principal := FullAccessPrincipal(AnonymousOwner)
	queries := []string{
		"對套餐頭點選數量增加會發生甚麼事",
		// Notion「自然語句回歸集」那張表的全部六題，一次量完再改文件。
		"如何暫存訂單?",
		"如何作廢訂單?",
		"如何重印發票?",
		"怎麼看營業報表?",
		"如何結帳?",
		"今天台北天氣如何?",
	}

	for _, query := range queries {
		answer, _, metrics, err := svc.ChatWithMetrics(ctx, principal, query, "")
		if err != nil {
			t.Fatalf("ChatWithMetrics(%q) error = %v", query, err)
		}
		t.Logf("=== QUERY: %s ===", query)
		t.Logf("  Grounded: %v", answer.Grounded())
		t.Logf("  Answer: %s", answer.Text())
		t.Logf("  Sources (%d):", len(answer.Sources()))
		for _, src := range answer.Sources() {
			t.Logf("    - %s", src)
		}
		t.Logf("  Metrics: bm25Max=%.2f bestCosine=%.3f", metrics.BM25Max, metrics.BestCosine)
	}
}

type evalProbeRow struct {
	Question            string   `json:"q"`
	EngineeringQuestion string   `json:"question"`
	Kind                string   `json:"kind"`
	MustNotInfer        bool     `json:"mni"`
	Grounded            bool     `json:"grounded"`
	ExpectedSourceID    string   `json:"expected_source_id"`
	ExpectedSource      string   `json:"expect_source"`
	ExpectAny           []string `json:"expect_any"`
}

func (r evalProbeRow) normalize() evalProbeRow {
	if r.Question == "" {
		r.Question = r.EngineeringQuestion
	}
	if r.Kind == "must_not_infer" {
		r.MustNotInfer = true
	}
	return r
}

func TestEvalProbeRowNormalizesChatAndEngineeringSchemas(t *testing.T) {
	input := `[
		{"q":"如何結帳?","kind":"D","mni":false,"expected_source_id":"Store.POS--ui-checkout"},
		{"question":"點餐-列印狀態的呼叫鏈經過哪些方法?","kind":"callchain","expect_source":"Printing_Config-procedure","expect_any":["OnDisableCheckout"]},
		{"question":"會員點數會打哪支 API?","kind":"must_not_infer"}
	]`
	var rows []evalProbeRow
	if err := json.Unmarshal([]byte(input), &rows); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	for i := range rows {
		rows[i] = rows[i].normalize()
	}
	if got := rows[0].Question; got != "如何結帳?" {
		t.Errorf("chat question = %q", got)
	}
	if got := rows[1].Question; got != "點餐-列印狀態的呼叫鏈經過哪些方法?" {
		t.Errorf("engineering question = %q", got)
	}
	if rows[1].ExpectedSource != "Printing_Config-procedure" || len(rows[1].ExpectAny) != 1 {
		t.Errorf("engineering truth = source %q, symbols %v", rows[1].ExpectedSource, rows[1].ExpectAny)
	}
	if !rows[2].MustNotInfer {
		t.Error("engineering must_not_infer kind was not normalized")
	}
}

type evalProbeStat struct {
	total, top3, top20, miss int
	// needRank[i] counts questions whose answer-bearing section first appears at
	// rank <= probeRanks[i]; answerMiss counts those absent from all candidates.
	needRank   []int
	answerable int
	answerMiss int
}

// probeRanks are the top-k values reported for the section-level measurement.
var probeRanks = []int{3, 5, 8, 10, 20}

const probeFileTopK = 3

// quotedTarget pulls the 「...」 term out of a generated question. The A2 sheet asks
// "<module> module 的「<button>」按鈕位於哪個畫面?", so the button is what the
// answering section must actually contain.
var quotedTarget = regexp.MustCompile(`「(.+?)」`)

// answerRank reports the 1-based rank of the first section whose body contains
// target, or 0 when no candidate does.
//
// File-level matching alone misled a diagnosis once: a module overview holds one
// section per screen, so a question about a button on screen 6 counted as
// "expected in top-3" whenever any section of that file ranked, even though the
// section carrying the button never reached the model. The model refusing was
// correct. Measure the section that holds the answer, not the file.
func answerRank(sections []Section, target string) int {
	if target == "" {
		return 0
	}
	for i, sec := range sections {
		if strings.Contains(sec.Body(), target) {
			return i + 1
		}
	}
	return 0
}

func answerRankAny(sections []Section, targets []string) int {
	for i, sec := range sections {
		for _, target := range targets {
			if target != "" && strings.Contains(sec.Body(), target) {
				return i + 1
			}
		}
	}
	return 0
}

func runEvalRetrievalProbe(t *testing.T, ctx context.Context, oai *OpenAIClient,
	indexed []Section, corpus Corpus, vecMap map[string][]float32, evalPath string) {
	b, err := os.ReadFile(evalPath)
	if err != nil {
		t.Fatalf("read eval output: %v", err)
	}
	var rows []evalProbeRow
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatalf("decode eval output: %v", err)
	}
	failed := make([]evalProbeRow, 0)
	for _, raw := range rows {
		row := raw.normalize()
		if !row.MustNotInfer && !row.Grounded && row.Question != "" {
			failed = append(failed, row)
		}
	}
	if len(failed) == 0 {
		t.Fatal("eval output has no non-must_not_infer grounded=false rows")
	}

	queries := make([]string, len(failed))
	for i, row := range failed {
		queries[i] = row.Question
	}
	vectors, err := oai.Embed(ctx, queries)
	if err != nil {
		t.Fatalf("Embed(eval questions) error: %v", err)
	}
	if len(vectors) != len(failed) {
		t.Fatalf("Embed(eval questions) returned %d vectors, want %d", len(vectors), len(failed))
	}

	stats := map[string]*evalProbeStat{}
	for i, row := range failed {
		stat := stats[row.Kind]
		if stat == nil {
			stat = &evalProbeStat{needRank: make([]int, len(probeRanks))}
			stats[row.Kind] = stat
		}
		stat.total++

		bm25List := corpus.RankBM25(tokenize(row.Question))
		for len(bm25List) > 0 && bm25List[len(bm25List)-1].Score <= 0 {
			bm25List = bm25List[:len(bm25List)-1]
		}
		if len(bm25List) > candidateK {
			bm25List = bm25List[:candidateK]
		}
		vecList := RankVector(indexed, vecMap, vectors[i], candidateK, func(section Section) bool {
			return CanSee(FullAccessPrincipal(AnonymousOwner), section)
		})
		ranked := bm25List
		if len(vecList) > 0 && len(bm25List) > 0 {
			ranked = FuseRRF([][]ScoredSection{bm25List, vecList}, rrfK)
		} else if len(vecList) > 0 {
			ranked = vecList
		}

		expectedPath := evalSourcePath(row.ExpectedSourceID)
		if containsExpectedSource(topSections(indexed, ranked, probeFileTopK), expectedPath, row.ExpectedSource) {
			stat.top3++
		} else if containsExpectedSource(topSections(indexed, ranked, candidateK), expectedPath, row.ExpectedSource) {
			stat.top20++
		} else {
			stat.miss++
		}

		target := ""
		if m := quotedTarget.FindStringSubmatch(row.Question); m != nil {
			target = m[1]
		}
		targets := row.ExpectAny
		if target != "" {
			targets = []string{target}
		}
		if len(targets) == 0 {
			continue
		}
		stat.answerable++
		rank := answerRankAny(topSections(indexed, ranked, candidateK), targets)
		t.Logf("[kind=%s expected-file=%q file-top-%d=%v symbol-rank=%d] %s",
			row.Kind, firstNonEmpty(expectedPath, row.ExpectedSource), probeFileTopK,
			containsExpectedSource(topSections(indexed, ranked, probeFileTopK), expectedPath, row.ExpectedSource),
			rank, row.Question)
		if rank == 0 {
			stat.answerMiss++
			continue
		}
		for i, k := range probeRanks {
			if rank <= k {
				stat.needRank[i]++
			}
		}
	}

	kinds := make([]string, 0, len(stats))
	for kind := range stats {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	fmt.Printf("\n==== EVAL RETRIEVAL PROBE ====\nquestions with grounded=false: %d\n", len(failed))
	for _, kind := range kinds {
		stat := stats[kind]
		fmt.Printf("%-16s total=%d  expected in top-3=%d  only top-20=%d  miss top-20=%d\n",
			kind, stat.total, stat.top3, stat.top20, stat.miss)
	}
	fmt.Printf("top-%d = the expected FILE ranked; it does not mean the answering section did\n", probeFileTopK)
	fmt.Println("\n-- section-level: does a retrieved section actually contain the 「term」? --")
	for _, kind := range kinds {
		stat := stats[kind]
		parts := make([]string, 0, len(probeRanks))
		for i, k := range probeRanks {
			parts = append(parts, fmt.Sprintf("top-%d=%d", k, stat.needRank[i]))
		}
		fmt.Printf("%-16s total=%d  %s  not applicable=%d  absent from all %d candidates=%d\n",
			kind, stat.total, strings.Join(parts, "  "), stat.total-stat.answerable, candidateK, stat.answerMiss)
	}
	fmt.Printf("topK is currently %d — raising it only helps for questions already covered above.\n", topK)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func evalSourcePath(id string) string {
	team, rest, ok := strings.Cut(id, "--")
	if !ok {
		return ""
	}
	if suffix, ok := strings.CutPrefix(rest, "ui-"); ok {
		if !strings.HasPrefix(suffix, "module-") {
			return team + "/ui_inventory/" + suffix + "-ui_inventory.md"
		}
		suffix = strings.TrimPrefix(suffix, "module-")
		return team + "/ui_inventory/_modules/" + suffix + "-overview.md"
	}
	return ""
}

func containsExpectedSource(sections []Section, expectedPath, expectedFragment string) bool {
	if expectedPath == "" && expectedFragment == "" {
		return false
	}
	for _, section := range sections {
		file := strings.ReplaceAll(section.File(), "\\", "/")
		if expectedPath != "" && (file == expectedPath || strings.HasSuffix(file, "/"+expectedPath)) {
			return true
		}
		if expectedFragment != "" && strings.Contains(file, expectedFragment) {
			return true
		}
	}
	return false
}

package kb

type HealthResponse struct {
	Status  string `json:"status"`
	Vectors string `json:"vectors"`
	// Chat names the decoding settings the service answers with. An eval run
	// records it so a later comparison can tell a corpus regression apart from
	// a model or parameter change. Absent when no real model is wired.
	Chat *ChatConfig `json:"chat,omitempty"`
}

type ChatConfig struct {
	Model string `json:"model"`
	// Prompt fingerprints the grounding instructions, so a run can prove which
	// prompt answered it. It changes whenever the prompt does.
	Prompt      string  `json:"prompt"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int64   `json:"max_tokens,omitempty"`
	// Thinking on or off changes answers as much as a model swap does.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type IndexResponse struct {
	FilesIndexed    int `json:"files_indexed"`
	SectionsIndexed int `json:"sections_indexed"`
}

type ChatRequest struct {
	Query     string `json:"query"`
	SessionID string `json:"session_id"`
}

type ChatResponse struct {
	SessionID  string   `json:"session_id"`
	Answer     string   `json:"answer"`
	Grounded   bool     `json:"grounded"`
	Sources    []string `json:"sources"`
	Images     []string `json:"images"`
	Strategy   string   `json:"strategy"`
	BM25Max    float64  `json:"bm25_max"`
	BestCosine float64  `json:"best_cosine"`
	// LLM is omitted when the retrieval gate refused before any model call.
	LLM *LLMStats `json:"llm,omitempty"`
}

// LLMStats lets the eval harness compare backends per question. TPS is output
// tokens over the time after the first token, so it excludes queueing and prefill.
type LLMStats struct {
	Model        string  `json:"model"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TTFTMs       float64 `json:"ttft_ms"`
	TPS          float64 `json:"tps"`
	LatencyMs    float64 `json:"latency_ms"`
}

func toLLMStats(c *Completion) *LLMStats {
	if c == nil {
		return nil
	}
	stats := &LLMStats{
		Model:        c.Model,
		InputTokens:  c.PromptTokens,
		OutputTokens: c.CompletionTokens,
		TTFTMs:       float64(c.TTFT.Microseconds()) / 1000,
		LatencyMs:    float64(c.Duration.Microseconds()) / 1000,
	}
	if decode := c.Duration - c.TTFT; c.TTFT > 0 && decode > 0 {
		stats.TPS = float64(c.CompletionTokens) / decode.Seconds()
	}
	return stats
}

func toChatResponse(a Answer, sessionID string, metrics RetrievalMetrics) ChatResponse {
	sources := make([]string, 0, len(a.Sources()))
	for _, citation := range a.Sources() {
		sources = append(sources, citation.String())
	}
	images := a.Images()
	if images == nil {
		images = []string{}
	}
	return ChatResponse{SessionID: sessionID, Answer: a.Text(), Grounded: a.Grounded(), Sources: sources, Images: images, Strategy: a.Strategy(), BM25Max: metrics.BM25Max, BestCosine: metrics.BestCosine, LLM: toLLMStats(metrics.LLM)}
}

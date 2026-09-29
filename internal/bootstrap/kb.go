// Package bootstrap wires the KB service's concrete adapters.
package bootstrap

import (
	"strings"
	"time"

	"github.com/linkc0829/llm-platform/internal/kb"
	"github.com/linkc0829/llm-platform/internal/platform/config"
	"github.com/linkc0829/llm-platform/internal/platform/httpserver"
)

// NewKBService constructs the shared KB service for HTTP and MCP entrypoints.
func NewKBService(cfg *config.Config) *kb.Service {
	repo := kb.NewMarkdownRepo(cfg.KB.DocsDir, cfg.KB.IndexDir)
	vecRepo := kb.NewVectorRepo(cfg.KB.IndexDir)
	sessions := kb.NewInProcStore()
	var llm kb.LLM
	var embedder kb.Embedder
	if strings.EqualFold(cfg.OpenAI.LLMMode, "fake") {
		fake := kb.NewFakeLLM()
		llm = fake
		embedder = fake
	} else {
		oai := kb.NewOpenAIClient(cfg.OpenAI.APIKey, cfg.OpenAI.BaseURL, cfg.OpenAI.ChatModel, cfg.OpenAI.EmbedModel,
			kb.ChatOptions{Temperature: cfg.OpenAI.ChatTemperature, MaxTokens: cfg.OpenAI.ChatMaxTokens,
				ReasoningEffort: cfg.OpenAI.ChatReasoningEffort, ForwardUser: cfg.OpenAI.ForwardUser})
		llm = oai
		embedder = oai
	}
	return kb.NewService(repo, llm, embedder, vecRepo, sessions, cfg.OpenAI.EmbedModel)
}

// ChatRuntime describes the model /health should report, or nil in fake mode
// where no real model answers.
func ChatRuntime(cfg *config.Config) *kb.ChatConfig {
	if strings.EqualFold(cfg.OpenAI.LLMMode, "fake") {
		return nil
	}
	return &kb.ChatConfig{
		Model:           cfg.OpenAI.ChatModel,
		Prompt:          kb.GroundingFingerprint(),
		Temperature:     cfg.OpenAI.ChatTemperature,
		MaxTokens:       cfg.OpenAI.ChatMaxTokens,
		ReasoningEffort: cfg.OpenAI.ChatReasoningEffort,
	}
}

// KBServerConfig computes the HTTP server configuration for the KB service,
// scaling WriteTimeout to accommodate full corpus reindexing while maintaining
// a safety buffer above KB_INDEX_TIMEOUT.
func KBServerConfig(cfg *config.Config, bindAddress string) httpserver.Config {
	indexTimeout := cfg.KB.IndexTimeout
	if indexTimeout <= 0 {
		indexTimeout = 60 * time.Second
	}
	writeTimeout := max(30*time.Second, indexTimeout+10*time.Second)
	return httpserver.Config{
		Port:         cfg.HTTP.Port,
		BindAddress:  bindAddress,
		WriteTimeout: &writeTimeout,
	}
}

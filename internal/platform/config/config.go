package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	App     AppConfig
	HTTP    HTTPConfig
	Logger  LoggerConfig
	OpenAI  OpenAIConfig
	KB      KBConfig
	Auth    AuthConfig
	Gateway GatewayConfig
}

type AppConfig struct {
	Env             string        `mapstructure:"env"`
	Name            string        `mapstructure:"name"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

type HTTPConfig struct {
	Port        int    `mapstructure:"port"`
	BindAddress string `mapstructure:"bind_address"`
}

type AuthConfig struct {
	File     string `mapstructure:"file"`
	Disabled bool   `mapstructure:"disabled"`
}

type LoggerConfig struct {
	Level    string `mapstructure:"level"`
	Encoding string `mapstructure:"encoding"`
	Output   string `mapstructure:"output"`
}

type OpenAIConfig struct {
	APIKey     string `mapstructure:"api_key"`
	LLMMode    string `mapstructure:"llm_mode"`
	BaseURL    string `mapstructure:"base_url"`
	ChatModel  string `mapstructure:"chat_model"`
	EmbedModel string `mapstructure:"embed_model"`
	// Decoding parameters for every chat completion. Left unset, the upstream
	// falls back to the served model's own generation_config — Gemma ships
	// temperature 1.0, which made a 36-question eval disagree with itself
	// between two rounds. Grounded QA wants greedy decoding, so pin it here.
	ChatTemperature float64 `mapstructure:"chat_temperature"`
	// 0 leaves max_tokens off the request. Answers cite their sources in the
	// closing lines, so a truncating limit costs the citation, not just prose.
	ChatMaxTokens int64 `mapstructure:"chat_max_tokens"`
	// Sent as reasoning_effort; empty leaves it off and the served model
	// decides. K2 Horizon thinks by default, which multiplies answer latency.
	ChatReasoningEffort string `mapstructure:"chat_reasoning_effort"`
	// ForwardUser attaches X-On-Behalf-Of: <user_id> on outbound chat requests.
	// Only enable when targeting the internal Gateway, never public cloud providers.
	ForwardUser bool `mapstructure:"forward_user"`
}

type KBConfig struct {
	DocsDir      string        `mapstructure:"docs_dir"`
	IndexDir     string        `mapstructure:"index_dir"`
	IndexTimeout time.Duration `mapstructure:"index_timeout"`
	ChatTimeout  time.Duration `mapstructure:"chat_timeout"`
}

type GatewayConfig struct {
	UpstreamBaseURL       string        `mapstructure:"upstream_base_url"`
	UpstreamAPIKey        string        `mapstructure:"upstream_api_key"`
	UpstreamHeaderTimeout time.Duration `mapstructure:"upstream_header_timeout"`
	EmbedUpstreamBaseURL  string        `mapstructure:"embed_upstream_base_url"`
	EmbedUpstreamAPIKey   string        `mapstructure:"embed_upstream_api_key"`
	EmbedModel            string        `mapstructure:"embed_model"`
	MaxInflightGlobal     int           `mapstructure:"max_inflight_global"`
	MaxInflightPerUser    int           `mapstructure:"max_inflight_per_user"`
	MaxOutputTokens       int           `mapstructure:"max_output_tokens"`
	Port                  int           `mapstructure:"port"`
	LogOutput             string        `mapstructure:"log_output"`
}

func LoadKB() (*Config, error) {
	cfg, err := LoadAuth()
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(cfg.OpenAI.LLMMode, "fake") && cfg.OpenAI.BaseURL == "" && cfg.OpenAI.APIKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is required")
	}
	return cfg, nil
}

// LoadGateway loads the configuration for the standalone gateway binary.
// UpstreamBaseURL must not include /v1.
func LoadGateway() (*Config, error) {
	cfg, err := LoadAuth()
	if err != nil {
		return nil, err
	}
	cfg.Gateway.UpstreamBaseURL = strings.TrimSpace(cfg.Gateway.UpstreamBaseURL)
	if cfg.Gateway.UpstreamBaseURL == "" {
		return nil, fmt.Errorf("GATEWAY_UPSTREAM_BASE_URL is required (must not include /v1)")
	}
	cfg.Gateway.UpstreamBaseURL = strings.TrimRight(cfg.Gateway.UpstreamBaseURL, "/")
	cfg.Gateway.UpstreamBaseURL = strings.TrimSuffix(cfg.Gateway.UpstreamBaseURL, "/v1")

	cfg.Gateway.EmbedUpstreamBaseURL = strings.TrimSpace(cfg.Gateway.EmbedUpstreamBaseURL)
	cfg.Gateway.EmbedUpstreamBaseURL = strings.TrimRight(cfg.Gateway.EmbedUpstreamBaseURL, "/")
	cfg.Gateway.EmbedModel = strings.TrimSpace(cfg.Gateway.EmbedModel)

	if cfg.Gateway.EmbedUpstreamBaseURL != "" && cfg.Gateway.EmbedModel == "" {
		return nil, fmt.Errorf("GATEWAY_EMBED_UPSTREAM_BASE_URL requires GATEWAY_EMBED_MODEL")
	}
	return cfg, nil
}

// LoadAuth loads the settings needed by kbtoken without requiring the KB
// service's LLM configuration.
func LoadAuth() (*Config, error) {
	v := newViper()

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return &cfg, nil
}

func newViper() *viper.Viper {
	v := viper.New()

	v.SetDefault("app.env", "development")
	v.SetDefault("app.name", "knowledge-base-qa-bot")
	v.SetDefault("app.shutdown_timeout", "10s")
	v.SetDefault("http.port", 12598)
	v.SetDefault("auth.disabled", false)
	v.SetDefault("logger.level", "info")
	v.SetDefault("logger.encoding", "json")
	// Writes to a file by default. The kb_query / llm_usage lines are the only
	// record of what the KB was asked, and a day not captured cannot be
	// recovered — opt-out (LOG_OUTPUT=stdout) is cheaper than a silent gap.
	v.SetDefault("logger.output", "stdout,log/kb.log")
	v.SetDefault("openai.llm_mode", "openai")
	v.SetDefault("openai.chat_model", "gpt-4o-mini")
	v.SetDefault("openai.embed_model", "text-embedding-3-small")
	v.SetDefault("openai.chat_temperature", 0)
	v.SetDefault("openai.chat_max_tokens", 1024)
	v.SetDefault("openai.chat_reasoning_effort", "")
	v.SetDefault("openai.forward_user", false)
	v.SetDefault("kb.docs_dir", "docs")
	v.SetDefault("kb.index_dir", ".kb")
	v.SetDefault("kb.index_timeout", "60s")
	v.SetDefault("kb.chat_timeout", "30s")
	v.SetDefault("gateway.max_inflight_global", 64)
	v.SetDefault("gateway.max_inflight_per_user", 4)
	v.SetDefault("gateway.max_output_tokens", 16384)
	v.SetDefault("gateway.port", 12599)
	v.SetDefault("gateway.log_output", "stdout,log/gateway.log")
	v.SetDefault("gateway.upstream_header_timeout", "300s")

	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	binds := map[string]string{
		"app.env":                         "APP_ENV",
		"app.name":                        "APP_NAME",
		"app.shutdown_timeout":            "APP_SHUTDOWN_TIMEOUT",
		"http.port":                       "APP_PORT",
		"http.bind_address":               "APP_BIND_ADDRESS",
		"logger.level":                    "LOG_LEVEL",
		"logger.encoding":                 "LOG_ENCODING",
		"logger.output":                   "LOG_OUTPUT",
		"openai.api_key":                  "OPENAI_API_KEY",
		"openai.llm_mode":                 "KB_LLM_MODE",
		"openai.base_url":                 "OPENAI_BASE_URL",
		"openai.chat_model":               "KB_CHAT_MODEL",
		"openai.chat_temperature":         "KB_CHAT_TEMPERATURE",
		"openai.chat_max_tokens":          "KB_CHAT_MAX_TOKENS",
		"openai.chat_reasoning_effort":    "KB_CHAT_REASONING_EFFORT",
		"openai.embed_model":              "KB_EMBED_MODEL",
		"openai.forward_user":             "KB_LLM_FORWARD_USER",
		"kb.docs_dir":                     "KB_DOCS_DIR",
		"kb.index_dir":                    "KB_INDEX_DIR",
		"kb.index_timeout":                "KB_INDEX_TIMEOUT",
		"kb.chat_timeout":                 "KB_CHAT_TIMEOUT",
		"auth.file":                       "KB_AUTH_FILE",
		"auth.disabled":                   "KB_AUTH_DISABLED",
		"gateway.upstream_base_url":       "GATEWAY_UPSTREAM_BASE_URL",
		"gateway.upstream_api_key":        "GATEWAY_UPSTREAM_API_KEY",
		"gateway.upstream_header_timeout": "GATEWAY_UPSTREAM_HEADER_TIMEOUT",
		"gateway.embed_upstream_base_url": "GATEWAY_EMBED_UPSTREAM_BASE_URL",
		"gateway.embed_upstream_api_key":  "GATEWAY_EMBED_UPSTREAM_API_KEY",
		"gateway.embed_model":             "GATEWAY_EMBED_MODEL",
		"gateway.max_inflight_global":     "GATEWAY_MAX_INFLIGHT",
		"gateway.max_inflight_per_user":   "GATEWAY_MAX_INFLIGHT_PER_USER",
		"gateway.max_output_tokens":       "GATEWAY_MAX_OUTPUT_TOKENS",
		"gateway.port":                    "GATEWAY_PORT",
		"gateway.log_output":              "GATEWAY_LOG_OUTPUT",
	}
	for k, env := range binds {
		_ = v.BindEnv(k, env)
	}

	v.SetConfigName(".env")
	v.SetConfigType("env")
	v.AddConfigPath(".")
	_ = v.ReadInConfig()
	applyEnvFileAliases(v, binds)

	return v
}

func applyEnvFileAliases(v *viper.Viper, binds map[string]string) {
	for key, env := range binds {
		if _, ok := os.LookupEnv(env); ok {
			continue
		}
		if v.InConfig(env) {
			v.Set(key, v.Get(env))
		}
	}
}

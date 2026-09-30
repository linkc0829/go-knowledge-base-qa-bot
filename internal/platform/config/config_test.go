package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadKBReadsEnvFileAliases(t *testing.T) {
	unsetEnv(t, "OPENAI_API_KEY")
	unsetEnv(t, "APP_PORT")
	unsetEnv(t, "KB_CHAT_MODEL")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	defer func() {
		_ = os.Chdir(cwd)
	}()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_API_KEY='test-key'\nAPP_PORT=9090\nKB_CHAT_MODEL=test-model\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cfg, err := LoadKB()
	if err != nil {
		t.Fatalf("LoadKB: %v", err)
	}
	if cfg.OpenAI.APIKey != "test-key" {
		t.Fatalf("OpenAI API key = %q, want test-key", cfg.OpenAI.APIKey)
	}
	if cfg.HTTP.Port != 9090 {
		t.Fatalf("HTTP port = %d, want 9090", cfg.HTTP.Port)
	}
	if cfg.OpenAI.ChatModel != "test-model" {
		t.Fatalf("OpenAI chat model = %q, want test-model", cfg.OpenAI.ChatModel)
	}
}

func TestLoadKBAllowsBaseURLWithoutAPIKey(t *testing.T) {
	unsetEnv(t, "OPENAI_API_KEY")
	unsetEnv(t, "KB_LLM_MODE")
	unsetEnv(t, "OPENAI_BASE_URL")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_BASE_URL=http://localhost:11434/v1\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cfg, err := LoadKB()
	if err != nil {
		t.Fatalf("LoadKB() error = %v, want nil", err)
	}
	if cfg.OpenAI.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("OpenAI base URL = %q, want Ollama URL", cfg.OpenAI.BaseURL)
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	old, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			if err := os.Setenv(key, old); err != nil {
				t.Fatalf("restore %s: %v", key, err)
			}
			return
		}
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("restore unset %s: %v", key, err)
		}
	})
}

func TestLoadKBAllowsFakeLLMWithoutAPIKey(t *testing.T) {
	unsetEnv(t, "OPENAI_API_KEY")
	unsetEnv(t, "KB_LLM_MODE")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	defer func() {
		_ = os.Chdir(cwd)
	}()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KB_LLM_MODE=fake\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cfg, err := LoadKB()
	if err != nil {
		t.Fatalf("LoadKB: %v", err)
	}
	if cfg.OpenAI.LLMMode != "fake" {
		t.Fatalf("OpenAI LLM mode = %q, want fake", cfg.OpenAI.LLMMode)
	}
}

// TestLoadKBDefaultsLoggerOutputToFile pins the default that makes the metric
// log exist at all. A typo in the viper key would leave Output empty, the logger
// would fall back to stdout only, and the absence would look exactly like a
// service nobody queried — so assert the default rather than trust the string.
func TestLoadKBDefaultsLoggerOutputToFile(t *testing.T) {
	unsetEnv(t, "LOG_OUTPUT")
	unsetEnv(t, "OPENAI_API_KEY")
	unsetEnv(t, "KB_LLM_MODE")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	defer func() {
		_ = os.Chdir(cwd)
	}()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KB_LLM_MODE=fake\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cfg, err := LoadKB()
	if err != nil {
		t.Fatalf("LoadKB: %v", err)
	}
	if cfg.Logger.Output != "stdout,log/kb.log" {
		t.Errorf("Logger.Output = %q, want stdout,log/kb.log", cfg.Logger.Output)
	}
}

func TestLoadKBChatDecodingDefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name          string
		env           string
		wantTemp      float64
		wantMaxTokens int64
	}{
		{name: "defaults_to_greedy", env: "", wantTemp: 0, wantMaxTokens: 1024},
		{name: "env_overrides", env: "KB_CHAT_TEMPERATURE=0.3\nKB_CHAT_MAX_TOKENS=0\n", wantTemp: 0.3, wantMaxTokens: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetEnv(t, "KB_CHAT_TEMPERATURE")
			unsetEnv(t, "KB_CHAT_MAX_TOKENS")

			cwd, err := os.Getwd()
			if err != nil {
				t.Fatalf("get cwd: %v", err)
			}
			defer func() {
				_ = os.Chdir(cwd)
			}()

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_API_KEY=key\n"+tt.env), 0o600); err != nil {
				t.Fatalf("write .env: %v", err)
			}
			if err := os.Chdir(dir); err != nil {
				t.Fatalf("chdir: %v", err)
			}

			cfg, err := LoadKB()
			if err != nil {
				t.Fatalf("LoadKB: %v", err)
			}
			if cfg.OpenAI.ChatTemperature != tt.wantTemp {
				t.Errorf("chat temperature = %v, want %v", cfg.OpenAI.ChatTemperature, tt.wantTemp)
			}
			if cfg.OpenAI.ChatMaxTokens != tt.wantMaxTokens {
				t.Errorf("chat max tokens = %d, want %d", cfg.OpenAI.ChatMaxTokens, tt.wantMaxTokens)
			}
		})
	}
}

func TestLoadGatewayConfig(t *testing.T) {
	unsetEnv(t, "GATEWAY_UPSTREAM_BASE_URL")
	unsetEnv(t, "GATEWAY_UPSTREAM_HEADER_TIMEOUT")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}

	runInDir := func(t *testing.T, envContent string, fn func(t *testing.T)) {
		dir := t.TempDir()
		if envContent != "" {
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(envContent), 0o600); err != nil {
				t.Fatalf("write .env: %v", err)
			}
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatalf("chdir: %v", err)
		}
		defer func() { _ = os.Chdir(cwd) }()
		fn(t)
	}

	t.Run("missing_base_url_fails", func(t *testing.T) {
		runInDir(t, "", func(t *testing.T) {
			_, err := LoadGateway()
			if err == nil {
				t.Fatal("expected error when GATEWAY_UPSTREAM_BASE_URL is missing")
			}
		})
	})

	t.Run("defaults_and_url_stripping", func(t *testing.T) {
		runInDir(t, "GATEWAY_UPSTREAM_BASE_URL=http://localhost:8000/v1/\n", func(t *testing.T) {
			cfg, err := LoadGateway()
			if err != nil {
				t.Fatalf("LoadGateway: %v", err)
			}
			if cfg.Gateway.UpstreamBaseURL != "http://localhost:8000" {
				t.Errorf("UpstreamBaseURL = %q, want http://localhost:8000", cfg.Gateway.UpstreamBaseURL)
			}
			if cfg.Gateway.UpstreamHeaderTimeout != 300*time.Second {
				t.Errorf("UpstreamHeaderTimeout = %v, want 300s", cfg.Gateway.UpstreamHeaderTimeout)
			}
		})
	})

	t.Run("custom_header_timeout", func(t *testing.T) {
		runInDir(t, "GATEWAY_UPSTREAM_BASE_URL=http://localhost:8000\nGATEWAY_UPSTREAM_HEADER_TIMEOUT=45s\n", func(t *testing.T) {
			cfg, err := LoadGateway()
			if err != nil {
				t.Fatalf("LoadGateway: %v", err)
			}
			if cfg.Gateway.UpstreamHeaderTimeout != 45*time.Second {
				t.Errorf("UpstreamHeaderTimeout = %v, want 45s", cfg.Gateway.UpstreamHeaderTimeout)
			}
		})
	})

	// Missing embed model when embed upstream URL is set must fail startup:
	// otherwise the model binding check is silently disabled and incompatible vectors can pollute index.
	t.Run("embed_upstream_without_model_fails", func(t *testing.T) {
		runInDir(t, "GATEWAY_UPSTREAM_BASE_URL=http://localhost:8000\nGATEWAY_EMBED_UPSTREAM_BASE_URL=https://generativelanguage.googleapis.com/v1beta/openai\n", func(t *testing.T) {
			_, err := LoadGateway()
			if err == nil {
				t.Fatal("expected error when GATEWAY_EMBED_UPSTREAM_BASE_URL is set without GATEWAY_EMBED_MODEL")
			}
		})
	})

	t.Run("embed_upstream_with_model_preserves_version_path", func(t *testing.T) {
		runInDir(t, "GATEWAY_UPSTREAM_BASE_URL=http://localhost:8000\nGATEWAY_EMBED_UPSTREAM_BASE_URL=https://generativelanguage.googleapis.com/v1beta/openai/\nGATEWAY_EMBED_MODEL=gemini-embedding-2\nGATEWAY_EMBED_UPSTREAM_API_KEY=test-embed-key\n", func(t *testing.T) {
			cfg, err := LoadGateway()
			if err != nil {
				t.Fatalf("LoadGateway: %v", err)
			}
			if cfg.Gateway.EmbedUpstreamBaseURL != "https://generativelanguage.googleapis.com/v1beta/openai" {
				t.Errorf("EmbedUpstreamBaseURL = %q, want https://generativelanguage.googleapis.com/v1beta/openai", cfg.Gateway.EmbedUpstreamBaseURL)
			}
			if cfg.Gateway.EmbedModel != "gemini-embedding-2" {
				t.Errorf("EmbedModel = %q, want gemini-embedding-2", cfg.Gateway.EmbedModel)
			}
			if cfg.Gateway.EmbedUpstreamAPIKey != "test-embed-key" {
				t.Errorf("EmbedUpstreamAPIKey = %q, want test-embed-key", cfg.Gateway.EmbedUpstreamAPIKey)
			}
		})
	})

	t.Run("embed_model_only_succeeds", func(t *testing.T) {
		runInDir(t, "GATEWAY_UPSTREAM_BASE_URL=http://localhost:8000\nGATEWAY_EMBED_MODEL=text-embedding-3-small\n", func(t *testing.T) {
			cfg, err := LoadGateway()
			if err != nil {
				t.Fatalf("LoadGateway: %v", err)
			}
			if cfg.Gateway.EmbedModel != "text-embedding-3-small" {
				t.Errorf("EmbedModel = %q, want text-embedding-3-small", cfg.Gateway.EmbedModel)
			}
			if cfg.Gateway.EmbedUpstreamBaseURL != "" {
				t.Errorf("EmbedUpstreamBaseURL = %q, want empty", cfg.Gateway.EmbedUpstreamBaseURL)
			}
		})
	})
}

// KB_CHAT_TIMEOUT must reach the handler from the environment (how compose
// passes it) and keep the old 30s behaviour when unset.
func TestLoadKBChatTimeout(t *testing.T) {
	unsetEnv(t, "OPENAI_API_KEY")
	unsetEnv(t, "KB_CHAT_TIMEOUT")
	t.Setenv("KB_LLM_MODE", "fake")
	t.Chdir(t.TempDir())

	cfg, err := LoadKB()
	if err != nil {
		t.Fatalf("LoadKB: %v", err)
	}
	if cfg.KB.ChatTimeout != 30*time.Second {
		t.Errorf("default ChatTimeout = %v, want 30s", cfg.KB.ChatTimeout)
	}

	t.Setenv("KB_CHAT_TIMEOUT", "90s")
	cfg, err = LoadKB()
	if err != nil {
		t.Fatalf("LoadKB: %v", err)
	}
	if cfg.KB.ChatTimeout != 90*time.Second {
		t.Errorf("ChatTimeout with KB_CHAT_TIMEOUT=90s = %v, want 90s", cfg.KB.ChatTimeout)
	}
}

func TestLoadKBIndexTimeout(t *testing.T) {
	unsetEnv(t, "OPENAI_API_KEY")
	unsetEnv(t, "KB_LLM_MODE")
	unsetEnv(t, "KB_INDEX_TIMEOUT")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}

	t.Run("default_index_timeout", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KB_LLM_MODE=fake\n"), 0o600); err != nil {
			t.Fatalf("write .env: %v", err)
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatalf("chdir: %v", err)
		}
		defer func() { _ = os.Chdir(cwd) }()

		cfg, err := LoadKB()
		if err != nil {
			t.Fatalf("LoadKB: %v", err)
		}
		if cfg.KB.IndexTimeout != 60*time.Second {
			t.Errorf("IndexTimeout = %v, want 60s", cfg.KB.IndexTimeout)
		}
	})

	t.Run("custom_index_timeout", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KB_LLM_MODE=fake\nKB_INDEX_TIMEOUT=10m\n"), 0o600); err != nil {
			t.Fatalf("write .env: %v", err)
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatalf("chdir: %v", err)
		}
		defer func() { _ = os.Chdir(cwd) }()

		cfg, err := LoadKB()
		if err != nil {
			t.Fatalf("LoadKB: %v", err)
		}
		if cfg.KB.IndexTimeout != 10*time.Minute {
			t.Errorf("IndexTimeout = %v, want 10m", cfg.KB.IndexTimeout)
		}
	})
}

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/linkc0829/llm-platform/internal/platform/config"
	"github.com/linkc0829/llm-platform/internal/platform/httpserver"
)

// 接線測試：透過正式的組裝路徑，驗證 WriteTimeout 的計算與預設規則。
func TestKBServerConfig_WriteTimeoutCalculation(t *testing.T) {
	tests := []struct {
		name         string
		indexTimeout time.Duration
		chatTimeout  time.Duration
		wantTimeout  time.Duration
	}{
		{
			// Otherwise the server would cut off a slow answer the handler still allows.
			name:        "long_chat_timeout_lifts_write_timeout",
			chatTimeout: 3 * time.Minute,
			wantTimeout: 3*time.Minute + 10*time.Second,
		},
		{
			name:         "custom_10m_timeout_adds_10s_buffer",
			indexTimeout: 10 * time.Minute,
			wantTimeout:  10*time.Minute + 10*time.Second,
		},
		{
			name:         "default_zero_falls_back_to_60s_plus_10s",
			indexTimeout: 0,
			wantTimeout:  70 * time.Second,
		},
		{
			name:         "short_timeout_respects_30s_minimum",
			indexTimeout: 5 * time.Second,
			wantTimeout:  30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				HTTP: config.HTTPConfig{Port: 12598},
				KB:   config.KBConfig{IndexTimeout: tt.indexTimeout, ChatTimeout: tt.chatTimeout},
			}
			serverCfg := KBServerConfig(cfg, "127.0.0.1")
			if serverCfg.WriteTimeout == nil {
				t.Fatal("expected WriteTimeout to be set")
			}
			if *serverCfg.WriteTimeout != tt.wantTimeout {
				t.Errorf("WriteTimeout = %v, want %v", *serverCfg.WriteTimeout, tt.wantTimeout)
			}
			if serverCfg.Port != 12598 {
				t.Errorf("Port = %d, want 12598", serverCfg.Port)
			}
			if serverCfg.BindAddress != "127.0.0.1" {
				t.Errorf("BindAddress = %q, want 127.0.0.1", serverCfg.BindAddress)
			}
		})
	}
}

// WriteTimeout（必要驗收）：用 httpserver.Wrap 起一個真的伺服器，把時間等比例縮小。
// 證明當 WriteTimeout 足夠時 client 收到 200 與完整 body；
// 對照組：WriteTimeout 小於 handler 執行時間時，client 收不到成功回應。
func TestKBServer_WriteTimeout_Enforcement(t *testing.T) {
	gin.SetMode(gin.TestMode)

	getFreePort := func(t *testing.T) int {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}

	t.Run("handler_finishes_within_write_timeout_succeeds", func(t *testing.T) {
		port := getFreePort(t)
		engine := gin.New()
		engine.POST("/slow", func(c *gin.Context) {
			time.Sleep(100 * time.Millisecond)
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		})

		writeTimeout := 500 * time.Millisecond // 大於 100ms
		srv := httpserver.Wrap(engine, httpserver.Config{
			Port:         port,
			BindAddress:  "127.0.0.1",
			WriteTimeout: &writeTimeout,
		}, zap.NewNop())

		errCh := make(chan error, 1)
		go func() {
			if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
		time.Sleep(50 * time.Millisecond) // 等待 listener 就緒

		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()

		client := &http.Client{Timeout: 2 * time.Second}
		targetURL := fmt.Sprintf("http://127.0.0.1:%d/slow", port)
		resp, err := client.Post(targetURL, "application/json", nil)
		if err != nil {
			t.Fatalf("client.Post error = %v, want nil", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != `{"status":"ok"}` {
			t.Errorf("body = %s, want %s", string(body), `{"status":"ok"}`)
		}
	})

	t.Run("handler_exceeds_write_timeout_fails", func(t *testing.T) {
		port := getFreePort(t)
		engine := gin.New()
		engine.POST("/slow", func(c *gin.Context) {
			time.Sleep(200 * time.Millisecond)
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		})

		writeTimeout := 50 * time.Millisecond // 小於 200ms
		srv := httpserver.Wrap(engine, httpserver.Config{
			Port:         port,
			BindAddress:  "127.0.0.1",
			WriteTimeout: &writeTimeout,
		}, zap.NewNop())

		errCh := make(chan error, 1)
		go func() {
			if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
		time.Sleep(50 * time.Millisecond)

		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()

		client := &http.Client{Timeout: 2 * time.Second}
		targetURL := fmt.Sprintf("http://127.0.0.1:%d/slow", port)
		resp, err := client.Post(targetURL, "application/json", nil)
		if err == nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			// 若 WriteTimeout 截斷連線，client 通常拿到 EOF / connection reset，或者未收到完整 200 body
			if resp.StatusCode == http.StatusOK && string(body) == `{"status":"ok"}` {
				t.Errorf("expected WriteTimeout failure, but got 200 with complete body")
			}
		}
		// 若 err != nil 則為預期中的連線被 server 中斷
	})
}

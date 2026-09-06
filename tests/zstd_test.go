package zstd_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/roadrunner-server/config/v6"
	"github.com/roadrunner-server/endure/v2"
	httpPlugin "github.com/roadrunner-server/http/v6"
	"github.com/roadrunner-server/http/v6/api"
	"github.com/roadrunner-server/logger/v6"
	"github.com/roadrunner-server/server/v6"
	zstdPlugin "github.com/roadrunner-server/zstd/v6"
	"github.com/stretchr/testify/require"
)

var _ api.Middleware = (*zstdPlugin.Plugin)(nil)

func TestHTTPMiddleware(t *testing.T) {
	want := strings.Repeat("RoadRunner zstd response.\n", 128)
	for _, tc := range []struct {
		name       string
		middleware string
		encodings  []string
	}{
		{"configured", "zstd", []string{"zstd", "identity", "", "gzip", "zstd;q=0"}},
		{"unconfigured", "", []string{"zstd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, url := startHTTP(t, tc.middleware)
			for _, encoding := range tc.encodings {
				name := encoding
				if name == "" {
					name = "missing"
				}
				t.Run(name, func(t *testing.T) {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
					require.NoError(t, err)
					if encoding != "" {
						req.Header.Set("Accept-Encoding", encoding)
					}

					resp, err := client.Do(req)
					require.NoError(t, err)
					defer func() { _ = resp.Body.Close() }()
					require.Equal(t, http.StatusOK, resp.StatusCode)
					body, err := io.ReadAll(resp.Body)
					require.NoError(t, err)

					if tc.middleware == "zstd" && encoding == "zstd" {
						require.Equal(t, "zstd", resp.Header.Get("Content-Encoding"))
						vary := strings.Split(strings.Join(resp.Header.Values("Vary"), ","), ",")
						require.True(t, slices.ContainsFunc(vary, func(token string) bool {
							return strings.EqualFold(strings.TrimSpace(token), "Accept-Encoding")
						}), "Vary must include Accept-Encoding")
						require.Less(t, len(body), len(want))

						decoder, err := zstd.NewReader(nil)
						require.NoError(t, err)
						defer decoder.Close()
						body, err = decoder.DecodeAll(body, nil)
						require.NoError(t, err)
					} else {
						require.Empty(t, resp.Header.Get("Content-Encoding"))
					}
					require.Equal(t, want, string(body))
				})
			}
		})
	}
}

func startHTTP(t *testing.T, middleware string) (*http.Client, string) {
	t.Helper()

	socket := filepath.Join(t.TempDir(), "http")
	cfg := &config.Plugin{
		Version: "2024.2.0",
		Type:    "yaml",
		ReadInCfg: fmt.Appendf(nil, `version: "3"
server:
  command: "php php_test_files/worker.php"
  relay: pipes
http:
  address: %q
  middleware: [%s]
  pool:
    num_workers: 1
    allocate_timeout: 10s
    destroy_timeout: 5s
logs:
  mode: development
  level: error
`, "unix://"+socket, middleware),
	}
	container := endure.New(slog.LevelError, endure.GracefulShutdownTimeout(10*time.Second))
	require.NoError(t, container.RegisterAll(cfg, &logger.Plugin{}, &server.Plugin{}, &httpPlugin.Plugin{}, &zstdPlugin.Plugin{}))
	require.NoError(t, container.Init())

	done := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() {
		if err := container.Stop(); err != nil {
			t.Errorf("stop container: %v", err)
		}
		close(done)
		wg.Wait()
	})
	results, err := container.Serve()
	require.NoError(t, err)
	wg.Go(func() {
		for {
			select {
			case result := <-results:
				if result == nil {
					return
				}
				t.Errorf("plugin %s: %v", result.VertexID, result.Error)
			case <-done:
				return
			}
		}
	})

	dialer := net.Dialer{Timeout: time.Second}
	require.Eventually(t, func() bool {
		conn, err := dialer.DialContext(t.Context(), "unix", socket)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 15*time.Second, 20*time.Millisecond, "HTTP server did not become ready")

	transport := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}, "http://localhost"
}

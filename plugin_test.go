package zstd

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/gzhttp"
	zstdcodec "github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func newPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := &Plugin{}
	require.NoError(t, p.Init())
	return p
}

func decodeZstd(t *testing.T, wire []byte) []byte {
	t.Helper()
	decoder, err := zstdcodec.NewReader(nil, zstdcodec.WithDecoderConcurrency(1))
	require.NoError(t, err)
	defer decoder.Close()
	body, err := decoder.DecodeAll(wire, nil)
	require.NoError(t, err)
	return body
}

func TestPlugin(t *testing.T) {
	p := newPlugin(t)
	require.Equal(t, "zstd", PluginName)
	require.Equal(t, "zstd", p.Name())
	require.NotNil(t, p.wrapper)
	require.NotNil(t, p.prop)
}

func TestMiddlewareEncoding(t *testing.T) {
	h := newPlugin(t).Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.WriteString(w, strings.Repeat(r.URL.Path, 1024))
		if err != nil {
			t.Error(err)
		}
	}))

	for _, tc := range []struct {
		name     string
		method   string
		accept   string
		encoding string
	}{
		{"zstd", http.MethodGet, "zstd", "zstd"},
		{"mixed", http.MethodGet, "gzip, deflate, zstd", "zstd"},
		{"quality", http.MethodGet, "gzip;q=1, zstd;q=0.5", "zstd"},
		{"case", http.MethodGet, " ZSTD ; q=1.0 ", "zstd"},
		{"missing", http.MethodGet, "", ""},
		{"identity", http.MethodGet, "identity", ""},
		{"gzip", http.MethodGet, "gzip", ""},
		{"disabled", http.MethodGet, "zstd;q=0", ""},
		{"no_gzip_fallback", http.MethodGet, "gzip, zstd;q=0", ""},
		{"head", http.MethodHead, "zstd", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), tc.method, "/"+tc.name, nil)
			if tc.accept != "" {
				req.Header.Set("Accept-Encoding", tc.accept)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			resp := rec.Result()
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, tc.encoding, resp.Header.Get("Content-Encoding"))
			require.Contains(t, resp.Header.Values("Vary"), "Accept-Encoding")
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			want := strings.Repeat(req.URL.Path, 1024)
			if tc.encoding == "zstd" {
				require.Less(t, len(body), len(want))
				body = decodeZstd(t, body)
			}
			require.Equal(t, want, string(body))
		})
	}
}

func TestMiddlewareResponse(t *testing.T) {
	body := strings.Repeat("compress me ", 512)
	for _, tc := range []struct {
		name     string
		body     string
		headers  http.Header
		status   int
		encoding string
	}{
		{"status", body, nil, http.StatusCreated, "zstd"},
		{"content_type", body, http.Header{"Content-Type": {"text/plain"}}, http.StatusOK, "zstd"},
		{"content_length", body, http.Header{"Content-Length": {strconv.Itoa(len(body))}, "Accept-Ranges": {"bytes"}}, http.StatusOK, "zstd"},
		{"below_threshold", strings.Repeat("x", 1023), nil, http.StatusOK, ""},
		{"at_threshold", strings.Repeat("x", 1024), nil, http.StatusOK, "zstd"},
		{"empty", "", nil, http.StatusOK, ""},
		{"no_content", "", nil, http.StatusNoContent, ""},
		{"not_modified", "", nil, http.StatusNotModified, ""},
		{"encoded", body, http.Header{"Content-Encoding": {"gzip"}}, http.StatusOK, "gzip"},
		{"partial", strings.Repeat("x", 1024), http.Header{"Content-Range": {"bytes 0-1023/2048"}}, http.StatusPartialContent, ""},
		{"excluded_type", body, http.Header{"Content-Type": {"image/jpeg"}}, http.StatusOK, ""},
		{"opt_out", body, http.Header{gzhttp.HeaderNoCompression: {"1"}}, http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlugin(t).Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Add("Vary", "Origin")
				for key, values := range tc.headers {
					for _, value := range values {
						w.Header().Add(key, value)
					}
				}
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, err := io.WriteString(w, tc.body)
					require.NoError(t, err)
				}
			}))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "zstd")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			resp := rec.Result()
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, tc.status, resp.StatusCode)
			require.Equal(t, tc.encoding, resp.Header.Get("Content-Encoding"))
			require.ElementsMatch(t, []string{"Accept-Encoding", "Origin"}, resp.Header.Values("Vary"))
			require.Empty(t, resp.Header.Get(gzhttp.HeaderNoCompression))
			for key, values := range tc.headers {
				if key == gzhttp.HeaderNoCompression || tc.encoding == "zstd" && (key == "Content-Length" || key == "Accept-Ranges") {
					require.Empty(t, resp.Header.Values(key))
					continue
				}
				require.Equal(t, values, resp.Header.Values(key))
			}
			if tc.encoding == "zstd" && tc.headers.Get("Content-Type") == "" {
				require.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))
			}
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			if tc.encoding == "zstd" {
				got = decodeZstd(t, got)
			}
			require.Equal(t, tc.body, string(got))
		})
	}
}

func TestMiddlewareFlush(t *testing.T) {
	first := "first chunk\n"
	next := strings.Repeat("next chunk\n", 32)
	rec := httptest.NewRecorder()
	h := newPlugin(t).Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(w, first)
		require.NoError(t, err)
		require.NoError(t, http.NewResponseController(w).Flush())
		require.True(t, rec.Flushed)
		require.Equal(t, "zstd", rec.Header().Get("Content-Encoding"))

		// Decode the flushed bytes before the handler completes the response.
		decoder, err := zstdcodec.NewReader(bytes.NewReader(rec.Body.Bytes()), zstdcodec.WithDecoderConcurrency(1))
		require.NoError(t, err)
		defer decoder.Close()
		got := make([]byte, len(first))
		_, err = io.ReadFull(decoder, got)
		require.NoError(t, err)
		require.Equal(t, first, string(got))

		for range 4 {
			_, err = io.WriteString(w, next)
			require.NoError(t, err)
		}
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	h.ServeHTTP(rec, req)
	require.Equal(t, first+strings.Repeat(next, 4), string(decodeZstd(t, rec.Body.Bytes())))
}

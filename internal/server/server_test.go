package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/snoby/dcache/internal/storage"
	"github.com/stretchr/testify/require"
)

const (
	testToken  = "engine-token-0123456789"
	testSecret = "0123456789abcdef0123456789abcdef"
)

// stubStore satisfies BlobStore without touching any network.
type stubStore struct{}

func (stubStore) PresignPut(context.Context, string, int64) (string, error) {
	return "", nil
}

func (stubStore) PresignGet(context.Context, string) (string, time.Time, error) {
	return "", time.Time{}, nil
}

func (stubStore) Exists(context.Context, string, int64) (bool, error) { return false, nil }

func (stubStore) Verify(context.Context, string, int64) error { return nil }

func (stubStore) List(context.Context) ([]storage.Object, error) { return nil, nil }

func (stubStore) Delete(context.Context, string) error { return nil }

func baseConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Token:      testToken,
		Pool:       "pool-a",
		HMACSecret: testSecret,
		DBPath:     filepath.Join(t.TempDir(), "dcache.sqlite3"),
	}
}

func newServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	s, err := New(context.Background(), cfg, &stubStore{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func serve(t *testing.T, s *Server, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"short token", func(c *Config) { c.Token = "short" }},
		{"empty token", func(c *Config) { c.Token = "" }},
		{"empty pool", func(c *Config) { c.Pool = "" }},
		{"pool dot", func(c *Config) { c.Pool = "." }},
		{"pool dotdot", func(c *Config) { c.Pool = ".." }},
		{"pool traversal", func(c *Config) { c.Pool = "../evil" }},
		{"pool slash", func(c *Config) { c.Pool = "a/b" }},
		{"short secret", func(c *Config) { c.HMACSecret = "too-short" }},
		{"empty db path", func(c *Config) { c.DBPath = "" }},
		{"negative max exports", func(c *Config) { c.MaxExports = -1 }},
		{"negative max verifiers", func(c *Config) { c.MaxVerifiers = -2 }},
		{"negative warm roots", func(c *Config) { c.WarmRoots = -3 }},
		{"negative request timeout", func(c *Config) { c.RequestTimeout = -time.Second }},
		{"negative renew timeout", func(c *Config) { c.RenewTimeout = -time.Second }},
		{"negative upload timeout", func(c *Config) { c.UploadTimeout = -time.Second }},
		{"negative checkpoint interval", func(c *Config) { c.CheckpointInterval = -time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t)
			tc.mutate(&cfg)
			s, err := New(context.Background(), cfg, &stubStore{})
			require.Error(t, err)
			require.Nil(t, s)
		})
	}
	t.Run("nil store", func(t *testing.T) {
		s, err := New(context.Background(), baseConfig(t), nil)
		require.Error(t, err)
		require.Nil(t, s)
	})
}

func TestNewAppliesDefaults(t *testing.T) {
	s := newServer(t, baseConfig(t))
	require.Equal(t, "v1.0.0-beta.25", s.cfg.EngineVersion)
	require.Equal(t, 30*time.Second, s.cfg.RequestTimeout)
	require.Equal(t, 1500*time.Millisecond, s.cfg.RenewTimeout)
	require.Equal(t, 15*time.Minute, s.cfg.UploadTimeout)
	require.Equal(t, 10*time.Minute, s.cfg.CheckpointInterval)
	require.Equal(t, 8, s.cfg.MaxExports)
	require.Equal(t, 16, s.cfg.MaxVerifiers)
	require.Equal(t, 64, s.cfg.WarmRoots)
}

func TestHealthzPublic(t *testing.T) {
	s := newServer(t, baseConfig(t))
	rec := serve(t, s, http.MethodGet, "/healthz", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestPrometheusMetricsPublicWithoutSecrets(t *testing.T) {
	s := newServer(t, baseConfig(t))
	rec := serve(t, s, http.MethodGet, "/metrics", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "dcache_")
	require.NotContains(t, rec.Body.String(), testToken)
	require.NotContains(t, rec.Body.String(), testSecret)
}

func TestDebugStateRequiresAuth(t *testing.T) {
	s := newServer(t, baseConfig(t))
	rec := serve(t, s, http.MethodGet, "/debug/state", nil, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.NotContains(t, rec.Body.String(), testToken)

	rec = serve(t, s, http.MethodGet, "/debug/state", nil, map[string]string{
		"Authorization": basicAuth(testToken, ""),
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), testToken)
}

func TestOTLPWrongMethodRejected(t *testing.T) {
	s := newServer(t, baseConfig(t))
	for _, path := range []string{"/v1/traces", "/v1/logs", "/v1/metrics"} {
		rec := serve(t, s, http.MethodGet, path, nil, map[string]string{
			"Authorization": basicAuth(testToken, ""),
		})
		require.Equal(t, http.StatusMethodNotAllowed, rec.Code, path)
	}
	rec := serve(t, s, http.MethodPost, "/healthz", nil, nil)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestOTLPUnauthorizedRejected(t *testing.T) {
	s := newServer(t, baseConfig(t))
	body := []byte(`{"resourceSpans":[]}`)
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no auth", nil},
		{"wrong basic token", map[string]string{"Authorization": basicAuth("wrong-token-0123456", "")}},
		{"basic with password", map[string]string{"Authorization": basicAuth(testToken, "secret")}},
		{"wrong bearer", map[string]string{"Authorization": "Bearer wrong-token-0123456"}},
	}
	for _, path := range []string{"/v1/traces", "/v1/logs", "/v1/metrics"} {
		for _, tc := range cases {
			rec := serve(t, s, http.MethodPost, path, body, tc.headers)
			require.Equal(t, http.StatusUnauthorized, rec.Code, "%s %s", path, tc.name)
			require.NotContains(t, rec.Body.String(), testToken)
		}
	}
}

func TestHeadTracesConnectivityProbe(t *testing.T) {
	s := newServer(t, baseConfig(t))
	rec := serve(t, s, http.MethodHead, "/v1/traces", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Zero(t, rec.Body.Len(), "HEAD probe must not return data")
}

func TestOTLPJSONAccepted(t *testing.T) {
	s := newServer(t, baseConfig(t))
	cases := []struct {
		path, body string
	}{
		{"/v1/traces", `{"resourceSpans":[]}`},
		{"/v1/logs", `{"resourceLogs":[]}`},
		{"/v1/metrics", `{"resourceMetrics":[]}`},
	}
	for _, tc := range cases {
		rec := serve(t, s, http.MethodPost, tc.path, []byte(tc.body), map[string]string{
			"Authorization": basicAuth(testToken, ""),
			"Content-Type":  "application/json",
		})
		require.Equal(t, http.StatusOK, rec.Code, tc.path)
	}
}

func TestOTLPProtobufGzipAccepted(t *testing.T) {
	s := newServer(t, baseConfig(t))
	// One empty ResourceSpans/ResourceLogs/ResourceMetrics element each:
	// field 1, wire type 2, length 0.
	bodies := map[string][]byte{
		"/v1/traces":  {0x0a, 0x00},
		"/v1/logs":    {0x0a, 0x00},
		"/v1/metrics": {0x0a, 0x00},
	}
	for path, plain := range bodies {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, err := zw.Write(plain)
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		rec := serve(t, s, http.MethodPost, path, buf.Bytes(), map[string]string{
			"Authorization":    basicAuth(testToken, ""),
			"Content-Type":     "application/x-protobuf",
			"Content-Encoding": "gzip",
		})
		require.Equal(t, http.StatusOK, rec.Code, path)
	}
}

func TestOTLPBearerSessionAuthAccepted(t *testing.T) {
	s := newServer(t, baseConfig(t))
	rec := serve(t, s, http.MethodPost, "/v1/traces", []byte(`{"resourceSpans":[]}`), map[string]string{
		"Authorization": "Bearer " + testToken,
		"Content-Type":  "application/json",
	})
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestOTLPMalformedBodyRejected(t *testing.T) {
	s := newServer(t, baseConfig(t))
	auth := map[string]string{
		"Authorization": basicAuth(testToken, ""),
		"Content-Type":  "application/json",
	}
	t.Run("empty body", func(t *testing.T) {
		rec := serve(t, s, http.MethodPost, "/v1/traces", nil, auth)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("bad json", func(t *testing.T) {
		rec := serve(t, s, http.MethodPost, "/v1/traces", []byte("{nope"), auth)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("bad protobuf", func(t *testing.T) {
		rec := serve(t, s, http.MethodPost, "/v1/traces", []byte{0xff, 0xff, 0xff}, map[string]string{
			"Authorization": basicAuth(testToken, ""),
			"Content-Type":  "application/x-protobuf",
		})
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("unsupported content type", func(t *testing.T) {
		rec := serve(t, s, http.MethodPost, "/v1/traces", []byte("x"), map[string]string{
			"Authorization": basicAuth(testToken, ""),
			"Content-Type":  "text/plain",
		})
		require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	})
	t.Run("unsupported content encoding", func(t *testing.T) {
		rec := serve(t, s, http.MethodPost, "/v1/traces", []byte("x"), map[string]string{
			"Authorization":    basicAuth(testToken, ""),
			"Content-Type":     "application/json",
			"Content-Encoding": "br",
		})
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("invalid gzip", func(t *testing.T) {
		rec := serve(t, s, http.MethodPost, "/v1/traces", []byte("definitely not gzip"), map[string]string{
			"Authorization":    basicAuth(testToken, ""),
			"Content-Type":     "application/json",
			"Content-Encoding": "gzip",
		})
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

func TestOTLPOversizeRejected(t *testing.T) {
	s := newServer(t, baseConfig(t))
	t.Run("declared content length", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader([]byte("x")))
		req.ContentLength = maxRequestBody + 1
		req.Header.Set("Authorization", basicAuth(testToken, ""))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	})
	t.Run("gzip bomb exceeds decompressed limit", func(t *testing.T) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, err := zw.Write(bytes.Repeat([]byte{0}, maxRequestBody+1))
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		require.Less(t, buf.Len(), 1<<20, "bomb should be small on the wire")
		rec := serve(t, s, http.MethodPost, "/v1/traces", buf.Bytes(), map[string]string{
			"Authorization":    basicAuth(testToken, ""),
			"Content-Type":     "application/x-protobuf",
			"Content-Encoding": "gzip",
		})
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	})
}

func TestMetricsIngestAcceptedWithoutProcessing(t *testing.T) {
	s := newServer(t, baseConfig(t))
	rec := serve(t, s, http.MethodPost, "/v1/metrics", []byte(`{"resourceMetrics":[]}`), map[string]string{
		"Authorization": basicAuth(testToken, ""),
		"Content-Type":  "application/json",
	})
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestCloseIsContextAwareAndCheckpoints(t *testing.T) {
	cfg := baseConfig(t)
	s, err := New(context.Background(), cfg, &stubStore{})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.Close(ctx))
}

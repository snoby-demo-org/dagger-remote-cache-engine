// Command dcache runs the standalone Dagger/S3 cache coordinator.
//
// It is a single Go process that owns a shared trusted dagql pool, serves the
// engine peer handshake and OTLP capture routes, checkpoints graph state into
// local SQLite, and stores verified snapshot bytes in an S3-compatible bucket. It is not a custom Dagger engine: clients use
// the pinned upstream engine version reported by DCACHE_ENGINE_VERSION.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/snoby/dcache/internal/server"
	"github.com/snoby/dcache/internal/storage"
)

const (
	defaultListen         = ":8080"
	defaultDBPath         = "./state/cache.db"
	defaultPool           = "shared"
	defaultRegion         = "us-east-1"
	defaultURLTTL         = 15 * time.Minute
	defaultEngineVersion  = "v1.0.0-beta.25"
	defaultCheckpoint     = 10 * time.Minute
	defaultMaxExports     = 8
	defaultMaxVerifiers   = 16
	defaultWarmRoots      = 64
	defaultRenewTimeout   = 1500 * time.Millisecond
	defaultRequestTimeout = 30 * time.Second
	defaultUploadTimeout  = 15 * time.Minute
	maxURLTTL             = 7 * 24 * time.Hour
	ensureBucketTimeout   = 15 * time.Second
	serverCloseTimeout    = 30 * time.Second
	httpShutdownTimeout   = 30 * time.Second
	httpReadHeaderTimeout = 10 * time.Second
	httpMaxHeaderBytes    = 1 << 20
	minTokenLength        = 16
	minHMACSecretLength   = 32
)

// loadConfig reads all environment configuration once and validates it. The
// returned listen address is separate because server.Config does not own the
// HTTP listener. Errors name the offending variable but never include secret
// values.
func loadConfig(getenv func(string) string) (server.Config, storage.Options, string, error) {
	var cfg server.Config
	var opts storage.Options

	listen := strings.TrimSpace(getenv("DCACHE_LISTEN"))
	if listen == "" {
		listen = defaultListen
	}
	if err := validateListen(listen); err != nil {
		return cfg, opts, "", fmt.Errorf("DCACHE_LISTEN: %w", err)
	}

	cfg.DBPath = strings.TrimSpace(getenv("DCACHE_DB"))
	if cfg.DBPath == "" {
		cfg.DBPath = defaultDBPath
	}

	cfg.Pool = strings.TrimSpace(getenv("DCACHE_POOL"))
	if cfg.Pool == "" {
		cfg.Pool = defaultPool
	}
	if !safePoolSegment(cfg.Pool) {
		return cfg, opts, "", errors.New("DCACHE_POOL: must be a single safe path segment")
	}

	cfg.Token = getenv("DCACHE_TOKEN")
	if strings.TrimSpace(cfg.Token) == "" {
		return cfg, opts, "", errors.New("DCACHE_TOKEN: required")
	}
	if len(cfg.Token) < minTokenLength {
		return cfg, opts, "", fmt.Errorf("DCACHE_TOKEN: must be at least %d characters", minTokenLength)
	}

	cfg.HMACSecret = getenv("DCACHE_HMAC_SECRET")
	if strings.TrimSpace(cfg.HMACSecret) == "" {
		return cfg, opts, "", errors.New("DCACHE_HMAC_SECRET: required")
	}
	if len(cfg.HMACSecret) < minHMACSecretLength {
		return cfg, opts, "", fmt.Errorf("DCACHE_HMAC_SECRET: must be at least %d bytes", minHMACSecretLength)
	}

	opts.Endpoint = strings.TrimSpace(getenv("S3_ENDPOINT"))
	if opts.Endpoint == "" {
		return cfg, opts, "", errors.New("S3_ENDPOINT: required")
	}
	if err := validateEndpoint(opts.Endpoint); err != nil {
		return cfg, opts, "", fmt.Errorf("S3_ENDPOINT: %w", err)
	}

	opts.PublicEndpoint = strings.TrimSpace(getenv("S3_PUBLIC_ENDPOINT"))
	if opts.PublicEndpoint != "" {
		if err := validateEndpoint(opts.PublicEndpoint); err != nil {
			return cfg, opts, "", fmt.Errorf("S3_PUBLIC_ENDPOINT: %w", err)
		}
	}

	opts.Bucket = strings.TrimSpace(getenv("S3_BUCKET"))
	if opts.Bucket == "" {
		return cfg, opts, "", errors.New("S3_BUCKET: required")
	}

	opts.Region = strings.TrimSpace(getenv("S3_REGION"))
	if opts.Region == "" {
		opts.Region = defaultRegion
	}

	opts.AccessKey = getenv("S3_ACCESS_KEY")
	if strings.TrimSpace(opts.AccessKey) == "" {
		return cfg, opts, "", errors.New("S3_ACCESS_KEY: required")
	}

	opts.SecretKey = getenv("S3_SECRET_KEY")
	if strings.TrimSpace(opts.SecretKey) == "" {
		return cfg, opts, "", errors.New("S3_SECRET_KEY: required")
	}

	ttl, err := parsePositiveDuration(getenv("DCACHE_URL_TTL"), defaultURLTTL)
	if err != nil {
		return cfg, opts, "", fmt.Errorf("DCACHE_URL_TTL: %w", err)
	}
	if ttl > maxURLTTL {
		return cfg, opts, "", fmt.Errorf("DCACHE_URL_TTL: must not exceed %s", maxURLTTL)
	}
	opts.URLTTL = ttl

	cfg.EngineVersion = strings.TrimSpace(getenv("DCACHE_ENGINE_VERSION"))
	if cfg.EngineVersion == "" {
		cfg.EngineVersion = defaultEngineVersion
	}

	cfg.CheckpointInterval, err = parsePositiveDuration(getenv("DCACHE_CHECKPOINT_INTERVAL"), defaultCheckpoint)
	if err != nil {
		return cfg, opts, "", fmt.Errorf("DCACHE_CHECKPOINT_INTERVAL: %w", err)
	}

	cfg.RenewTimeout, err = parsePositiveDuration(getenv("DCACHE_RENEW_TIMEOUT"), defaultRenewTimeout)
	if err != nil {
		return cfg, opts, "", fmt.Errorf("DCACHE_RENEW_TIMEOUT: %w", err)
	}

	cfg.MaxExports, err = parsePositiveInt(getenv("DCACHE_MAX_EXPORTS"), defaultMaxExports)
	if err != nil {
		return cfg, opts, "", fmt.Errorf("DCACHE_MAX_EXPORTS: %w", err)
	}
	cfg.MaxVerifiers, err = parsePositiveInt(getenv("DCACHE_MAX_VERIFIERS"), defaultMaxVerifiers)
	if err != nil {
		return cfg, opts, "", fmt.Errorf("DCACHE_MAX_VERIFIERS: %w", err)
	}
	cfg.WarmRoots, err = parsePositiveInt(getenv("DCACHE_WARM_ROOTS"), defaultWarmRoots)
	if err != nil {
		return cfg, opts, "", fmt.Errorf("DCACHE_WARM_ROOTS: %w", err)
	}

	cfg.RequestTimeout, err = parsePositiveDuration(getenv("DCACHE_REQUEST_TIMEOUT"), defaultRequestTimeout)
	if err != nil {
		return cfg, opts, "", fmt.Errorf("DCACHE_REQUEST_TIMEOUT: %w", err)
	}
	cfg.UploadTimeout, err = parsePositiveDuration(getenv("DCACHE_UPLOAD_TIMEOUT"), defaultUploadTimeout)
	if err != nil {
		return cfg, opts, "", fmt.Errorf("DCACHE_UPLOAD_TIMEOUT: %w", err)
	}
	opts.Pool = cfg.Pool

	return cfg, opts, listen, nil
}

func parsePositiveDuration(raw string, def time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New("invalid duration")
	}
	if d <= 0 {
		return 0, errors.New("must be positive")
	}
	return d, nil
}

func parsePositiveInt(raw string, def int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("invalid integer")
	}
	if n <= 0 {
		return 0, errors.New("must be positive")
	}
	return n, nil
}

func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("URL must use http or https")
	}
	if u.Host == "" {
		return errors.New("URL must include a host")
	}
	return nil
}

func validateListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("invalid listen address")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return errors.New("invalid listen port")
	}
	return nil
}

func safePoolSegment(p string) bool {
	if p == "" || p == "." || p == ".." {
		return false
	}
	if strings.ContainsAny(p, `/\`) {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// run loads configuration, connects to S3, starts the coordinator and serves
// HTTP until ctx is cancelled or the listener fails. All returned errors are
// sanitized so credentials never reach logs.
func run(ctx context.Context) error {
	cfg, opts, listen, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	secrets := []string{cfg.Token, cfg.HMACSecret, opts.AccessKey, opts.SecretKey}
	fail := func(err error) error {
		if err == nil {
			return nil
		}
		return errors.New(sanitize(err.Error(), secrets...))
	}

	store, err := storage.New(opts)
	if err != nil {
		return fail(err)
	}

	ensureCtx, cancelEnsure := context.WithTimeout(ctx, ensureBucketTimeout)
	err = store.EnsureBucket(ensureCtx)
	cancelEnsure()
	if err != nil {
		return fail(fmt.Errorf("ensure bucket: %w", err))
	}

	srv, err := server.New(ctx, cfg, store)
	if err != nil {
		return fail(err)
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		closeErr := closeServer(srv)
		return fail(errors.Join(fmt.Errorf("listen: %w", err), closeErr))
	}

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		MaxHeaderBytes:    httpMaxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	log.Printf("dcache listening on %s pool=%s engine=%s", listen, cfg.Pool, cfg.EngineVersion)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpSrv.Serve(ln)
	}()

	select {
	case err := <-serveErr:
		closeErr := closeServer(srv)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fail(errors.Join(fmt.Errorf("serve: %w", err), closeErr))
		}
		return fail(closeErr)
	case <-ctx.Done():
	}

	// Close the websocket/peer service first so long-lived engine connections
	// stop before the HTTP listener drains. server.Close also performs the
	// final checkpoint under its own bounded context.
	closeErr := closeServer(srv)

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), httpShutdownTimeout)
	httpErr := httpSrv.Shutdown(shutdownCtx)
	cancelShutdown()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr = errors.Join(httpErr, err)
		}
	default:
	}

	return fail(errors.Join(closeErr, httpErr))
}

func closeServer(srv *server.Server) error {
	closeCtx, cancel := context.WithTimeout(context.Background(), serverCloseTimeout)
	defer cancel()
	return srv.Close(closeCtx)
}

func sanitize(msg string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[REDACTED]")
		}
	}
	return msg
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		log.Printf("dcache: %v", err)
		os.Exit(1)
	}
}

// Package server assembles the dcache coordinator: a single Go process owning
// a shared trusted dagql pool, the HTTP routes for engine peers and OTLP
// telemetry capture, and background checkpointing over a blob store.
package server

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dagger/dagger/dagql"
	clogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	cmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	ctracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/snoby/dcache/internal/graph"
	"github.com/snoby/dcache/internal/peer"
	"github.com/snoby/dcache/internal/renewal"
	"github.com/snoby/dcache/internal/storage"
	"github.com/snoby/dcache/internal/telemetry"
)

// BlobStore is the object storage the coordinator presigns uploads and
// downloads against, verifies publications from, and checkpoints into.
type BlobStore interface {
	PresignPut(context.Context, string, int64) (string, error)
	PresignGet(context.Context, string) (string, time.Time, error)
	Exists(context.Context, string, int64) (bool, error)
	Verify(context.Context, string, int64) error
	List(context.Context) ([]storage.Object, error)
	Delete(context.Context, string) error
}

// Config is the coordinator configuration.
type Config struct {
	Token, HMACSecret  string
	Pool               string
	DBPath             string
	EngineVersion      string
	RequestTimeout     time.Duration
	RenewTimeout       time.Duration
	UploadTimeout      time.Duration
	CheckpointInterval time.Duration
	MaxExports         int
	MaxVerifiers       int
	WarmRoots          int
}

// Defaults applied when the corresponding Config field is zero. Negative
// values are never defaulted; they are rejected.
const (
	DefaultEngineVersion      = "v1.0.0-beta.25"
	defaultRequestTimeout     = 30 * time.Second
	defaultRenewTimeout       = 1500 * time.Millisecond
	defaultUploadTimeout      = 15 * time.Minute
	defaultCheckpointInterval = 10 * time.Minute
	defaultMaxExports         = 8
	defaultMaxVerifiers       = 16
	defaultWarmRoots          = 64
)

// signerTTL is the lifetime of presigned URLs issued by the renewal signer.
// It is independent of CheckpointInterval so a short-lived URL (e.g. 15m) can
// still be renewed after its expiry without waiting for the next checkpoint.
const signerTTL = 7 * 24 * time.Hour

// maxRequestBody is the strict limit on decompressed OTLP input: 64 MiB.
const maxRequestBody = 64 << 20

// Stats aggregates coordinator activity counters.
type Stats struct {
	Exports   atomic.Int64
	Published atomic.Int64
	Uploaded  atomic.Int64
	Offers    atomic.Int64
	Merges    atomic.Int64
	Renewals  atomic.Int64
	Rejected  atomic.Int64
	Dropped   atomic.Int64
}

// Server is the coordinator. handleRequest, warmPeer and onObservation are
// provided by the pipeline layer in this same package.
type Server struct {
	cfg Config

	graph  *graph.Graph
	store  BlobStore
	signer *renewal.Signer
	ingest *telemetry.Ingestor

	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	peers       map[dagql.CacheID]*peer.Peer
	exportSlots map[*peer.Peer]chan struct{}
	exporting   map[dagql.HolderKey]bool
	dirty       map[dagql.HolderKey]telemetry.Event
	inflight    map[string]int

	exportSem chan struct{}
	verifySem chan struct{}

	wg    sync.WaitGroup
	stats Stats
}

// New validates cfg, opens the real dagql cache at cfg.DBPath, and starts the
// checkpoint loop. No network I/O happens here.
func New(ctx context.Context, cfg Config, store BlobStore) (*Server, error) {
	if store == nil {
		return nil, errors.New("server: blob store must not be nil")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.applyDefaults()

	g, err := graph.New(ctx, cfg.DBPath)
	if err != nil {
		return nil, err
	}

	signer, err := renewal.New([]byte(cfg.HMACSecret), cfg.Pool, signerTTL)
	if err != nil {
		_ = g.Close(context.Background())
		return nil, fmt.Errorf("server: create renewal signer: %w", err)
	}

	sctx, cancel := context.WithCancel(ctx)
	s := &Server{
		cfg:         cfg,
		graph:       g,
		store:       store,
		signer:      signer,
		ctx:         sctx,
		cancel:      cancel,
		peers:       make(map[dagql.CacheID]*peer.Peer),
		exportSlots: make(map[*peer.Peer]chan struct{}),
		exporting:   make(map[dagql.HolderKey]bool),
		dirty:       make(map[dagql.HolderKey]telemetry.Event),
		inflight:    make(map[string]int),
		exportSem:   make(chan struct{}, cfg.MaxExports),
		verifySem:   make(chan struct{}, cfg.MaxVerifiers),
	}
	s.ingest = telemetry.New(g.Cache, s.onObservation)

	s.wg.Add(1)
	go s.checkpointLoop()
	return s, nil
}

func (c *Config) validate() error {
	if len(c.Token) < 16 {
		return errors.New("server: token must be at least 16 characters")
	}
	if !safePoolSegment(c.Pool) {
		return fmt.Errorf("server: pool %q must be a single safe path segment", c.Pool)
	}
	if len(c.HMACSecret) < 32 {
		return errors.New("server: HMAC secret must be at least 32 bytes")
	}
	if c.DBPath == "" {
		return errors.New("server: database path is required")
	}
	if c.RequestTimeout < 0 || c.RenewTimeout < 0 || c.UploadTimeout < 0 || c.CheckpointInterval < 0 {
		return errors.New("server: timeouts must not be negative")
	}
	if c.MaxExports < 0 || c.MaxVerifiers < 0 || c.WarmRoots < 0 {
		return errors.New("server: limits must not be negative")
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.EngineVersion == "" {
		c.EngineVersion = DefaultEngineVersion
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = defaultRequestTimeout
	}
	if c.RenewTimeout == 0 {
		c.RenewTimeout = defaultRenewTimeout
	}
	if c.UploadTimeout == 0 {
		c.UploadTimeout = defaultUploadTimeout
	}
	if c.CheckpointInterval == 0 {
		c.CheckpointInterval = defaultCheckpointInterval
	}
	if c.MaxExports == 0 {
		c.MaxExports = defaultMaxExports
	}
	if c.MaxVerifiers == 0 {
		c.MaxVerifiers = defaultMaxVerifiers
	}
	if c.WarmRoots == 0 {
		c.WarmRoots = defaultWarmRoots
	}
}

// safePoolSegment reports whether p is exactly one path segment that cannot
// escape a directory: no separators, no "." or "..", no control characters.
func safePoolSegment(p string) bool {
	if p == "" || p == "." || p == ".." {
		return false
	}
	if p != path.Base(p) || strings.ContainsAny(p, `/\`) {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Handler returns the public HTTP routes:
//
//	/v1/engine    engine peer handshake (Basic token, handled by peer.Accept)
//	/v1/traces    OTLP trace capture (Basic engine token or Bearer session)
//	/v1/logs      OTLP log capture (same auth)
//	/v1/metrics   OTLP metric capture (same auth, accept/drop)
//	/healthz      unauthenticated liveness probe
//	/metrics      unauthenticated coordinator counters (no secrets)
//	/debug/state  coordinator state snapshot (authenticated)
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/engine", s.handleEngine)
	mux.HandleFunc("/v1/traces", s.handleTraces)
	mux.HandleFunc("/v1/logs", s.handleLogs)
	mux.HandleFunc("/v1/metrics", s.handleMetrics)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/metrics", s.handlePromMetrics)
	mux.HandleFunc("/debug/state", s.handleDebugState)
	return mux
}

func (s *Server) handleEngine(w http.ResponseWriter, r *http.Request) {
	p, err := peer.Accept(s.ctx, w, r, s.cfg.Token, s.handleRequest)
	if err != nil {
		return
	}
	if v := p.Hello.EngineVersion; v != s.cfg.EngineVersion && !strings.HasPrefix(v, s.cfg.EngineVersion+"+") {
		p.Close()
		return
	}
	s.registerPeer(dagql.CacheID(p.Hello.CacheID), p)
}

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	s.handleOTLP(w, r,
		func() proto.Message { return &ctracepb.ExportTraceServiceRequest{} },
		func(ctx context.Context, msg proto.Message) error {
			return s.ingest.Traces(ctx, msg.(*ctracepb.ExportTraceServiceRequest))
		})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.handleOTLP(w, r,
		func() proto.Message { return &clogspb.ExportLogsServiceRequest{} },
		func(ctx context.Context, msg proto.Message) error {
			return s.ingest.Logs(ctx, msg.(*clogspb.ExportLogsServiceRequest))
		})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.handleOTLP(w, r,
		func() proto.Message { return &cmetricspb.ExportMetricsServiceRequest{} },
		func(ctx context.Context, msg proto.Message) error {
			return nil // Metrics are decoded and accepted; this cache ingests traces and logs.
		})
}

// handleOTLP enforces method, auth, and body limits, then hands the decoded
// request to ingest. HEAD is an unauthenticated connectivity probe returning
// no data. /v1/metrics accepts a valid POST even when processing drops it.
func (s *Server) handleOTLP(w http.ResponseWriter, r *http.Request, newMsg func() proto.Message, ingest func(context.Context, proto.Message) error) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorize(r) {
		s.stats.Rejected.Add(1)
		w.Header().Set("WWW-Authenticate", `Basic realm="dcache"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	msg := newMsg()
	if !s.decodeBody(w, r, msg) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	if err := ingest(ctx, msg); err != nil {
		if r.URL.Path == "/v1/metrics" {
			s.stats.Dropped.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// authorize accepts the engine Basic credential (username = token, empty
// password, exactly what the pinned engine's cloud exporters send) or a
// session exporter Bearer token. Comparison is constant time and the token is
// never echoed back.
func (s *Server) authorize(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(h, "Basic "):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
		if err != nil {
			return false
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.Token)) == 1 && pass == ""
	case strings.HasPrefix(h, "Bearer "):
		tok := strings.TrimPrefix(h, "Bearer ")
		return subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.Token)) == 1
	default:
		return false
	}
}

// decodeBody reads the request body honoring Content-Encoding gzip, enforcing
// the strict 64 MiB decompressed limit, and decoding application/x-protobuf or
// application/json into msg. It writes the error response itself.
func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, msg proto.Message) bool {
	if r.ContentLength > maxRequestBody {
		s.stats.Rejected.Add(1)
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	var reader io.Reader = r.Body
	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		if !strings.EqualFold(enc, "gzip") {
			s.stats.Rejected.Add(1)
			http.Error(w, "unsupported content encoding", http.StatusBadRequest)
			return false
		}
		zr, err := gzip.NewReader(reader)
		if err != nil {
			s.stats.Rejected.Add(1)
			http.Error(w, "invalid gzip body", http.StatusBadRequest)
			return false
		}
		defer zr.Close()
		reader = zr
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxRequestBody+1))
	if err != nil {
		s.stats.Rejected.Add(1)
		http.Error(w, "read body", http.StatusBadRequest)
		return false
	}
	if len(data) > maxRequestBody {
		s.stats.Rejected.Add(1)
		http.Error(w, "decompressed body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	if len(data) == 0 {
		s.stats.Rejected.Add(1)
		http.Error(w, "empty body", http.StatusBadRequest)
		return false
	}
	switch ct := r.Header.Get("Content-Type"); {
	case strings.HasPrefix(ct, "application/x-protobuf"):
		if err := proto.Unmarshal(data, msg); err != nil {
			s.stats.Rejected.Add(1)
			http.Error(w, "invalid protobuf body", http.StatusBadRequest)
			return false
		}
	case strings.HasPrefix(ct, "application/json"):
		if err := protojson.Unmarshal(data, msg); err != nil {
			s.stats.Rejected.Add(1)
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return false
		}
	default:
		s.stats.Rejected.Add(1)
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// handlePromMetrics exposes coordinator counters only: no tokens, no secrets.
func (s *Server) handlePromMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	peerCount, exporting := len(s.peers), len(s.exporting)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "dcache_peers %d\n", peerCount)
	fmt.Fprintf(w, "dcache_exporting %d\n", exporting)
	fmt.Fprintf(w, "dcache_exports_total %d\n", s.stats.Exports.Load())
	fmt.Fprintf(w, "dcache_published_total %d\n", s.stats.Published.Load())
	fmt.Fprintf(w, "dcache_uploaded_total %d\n", s.stats.Uploaded.Load())
	fmt.Fprintf(w, "dcache_offers_total %d\n", s.stats.Offers.Load())
	fmt.Fprintf(w, "dcache_merges_total %d\n", s.stats.Merges.Load())
	fmt.Fprintf(w, "dcache_renewals_total %d\n", s.stats.Renewals.Load())
	fmt.Fprintf(w, "dcache_rejected_total %d\n", s.stats.Rejected.Load())
	fmt.Fprintf(w, "dcache_dropped_total %d\n", s.stats.Dropped.Load())
}

// handleDebugState serves an authenticated JSON snapshot for functional
// evidence. It never includes the token.
func (s *Server) handleDebugState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorize(r) {
		s.stats.Rejected.Add(1)
		w.Header().Set("WWW-Authenticate", `Basic realm="dcache"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	ids := make([]string, 0, len(s.peers))
	for id := range s.peers {
		ids = append(ids, fmt.Sprint(id))
	}
	exporting := len(s.exporting)
	s.mu.Unlock()
	sort.Strings(ids)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pool":           s.cfg.Pool,
		"engine_version": s.cfg.EngineVersion,
		"peers":          ids,
		"exporting":      exporting,
		"stats": map[string]int64{
			"exports":   s.stats.Exports.Load(),
			"published": s.stats.Published.Load(),
			"uploaded":  s.stats.Uploaded.Load(),
			"offers":    s.stats.Offers.Load(),
			"merges":    s.stats.Merges.Load(),
			"renewals":  s.stats.Renewals.Load(),
			"rejected":  s.stats.Rejected.Load(),
			"dropped":   s.stats.Dropped.Load(),
		},
	})
}

// registerPeer records a peer that completed hello with a matching engine
// version, replacing any earlier peer with the same cache ID (the old
// connection is closed, never the new one), warms it in a tracked goroutine,
// and watches for its Done to remove it only if it is still the registered
// pointer.
func (s *Server) registerPeer(id dagql.CacheID, p *peer.Peer) {
	s.mu.Lock()
	old := s.peers[id]
	s.peers[id] = p
	s.mu.Unlock()
	if old != nil && old != p {
		old.Close()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.warmPeer(p)
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-s.ctx.Done():
		case <-p.Done():
			s.unregisterPeer(id, p)
		}
	}()
}

func (s *Server) unregisterPeer(id dagql.CacheID, p *peer.Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.peers[id]; ok && cur == p {
		delete(s.peers, id)
	}
	delete(s.exportSlots, p)
}

// exportSlotsFor returns the stable per-peer export slot gate (capacity 4),
// creating it lazily under s.mu.
func (s *Server) exportSlotsFor(p *peer.Peer) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exportSlots == nil {
		s.exportSlots = make(map[*peer.Peer]chan struct{})
	}
	ch, ok := s.exportSlots[p]
	if !ok {
		ch = make(chan struct{}, 4)
		s.exportSlots[p] = ch
	}
	return ch
}

// checkpointLoop checkpoints the graph every CheckpointInterval until the
// server context is cancelled.
func (s *Server) checkpointLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.CheckpointInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			_ = s.graph.Checkpoint(s.ctx)
		}
	}
}

// Close cancels the server context, closes all peers, waits for tracked
// workers within ctx, then checkpoints and closes the graph.
func (s *Server) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.cancel()

	s.mu.Lock()
	peers := make([]*peer.Peer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	s.peers = make(map[dagql.CacheID]*peer.Peer)
	s.mu.Unlock()
	for _, p := range peers {
		p.Close()
	}

	waitDone := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-ctx.Done():
		return fmt.Errorf("server: shutdown: %w", ctx.Err())
	}

	cpCtx, cpCancel := context.WithTimeout(context.Background(), s.cfg.RequestTimeout)
	defer cpCancel()
	var err error
	if cpErr := s.graph.Checkpoint(cpCtx); cpErr != nil {
		err = errors.Join(err, fmt.Errorf("server: final checkpoint: %w", cpErr))
	}
	if gErr := s.graph.Close(cpCtx); gErr != nil {
		err = errors.Join(err, fmt.Errorf("server: close graph: %w", gErr))
	}
	return err
}

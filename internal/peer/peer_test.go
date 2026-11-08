package peer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dagger/dagger/engine/remotecache/protocol"
	"github.com/stretchr/testify/require"
)

const testToken = "s3cret-token"

func validHello() protocol.Hello {
	return protocol.Hello{
		CacheID:       "cache-1",
		Generation:    3,
		EngineVersion: "v0.20.0",
		EngineName:    "dagger-engine",
	}
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// testServer runs Accept behind an httptest server and keeps the accepted
// peer until the socket ends.
type testServer struct {
	url  string
	peer chan *Peer
	errs chan error

	mu sync.Mutex
	p  *Peer
}

func newTestServer(t *testing.T, token string, handler RequestHandler) *testServer {
	t.Helper()
	ts := &testServer{
		peer: make(chan *Peer, 1),
		errs: make(chan error, 1),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := Accept(r.Context(), w, r, token, handler)
		if err != nil {
			ts.errs <- err
			return
		}
		ts.mu.Lock()
		ts.p = p
		ts.mu.Unlock()
		ts.peer <- p
		<-p.Done()
	}))
	t.Cleanup(func() {
		ts.mu.Lock()
		p := ts.p
		ts.mu.Unlock()
		if p != nil {
			_ = p.Close()
		}
		srv.Close()
	})
	ts.url = "ws" + strings.TrimPrefix(srv.URL, "http") + protocol.EnginePath
	return ts
}

func (ts *testServer) accepted(t *testing.T) *Peer {
	t.Helper()
	select {
	case p := <-ts.peer:
		return p
	case err := <-ts.errs:
		t.Fatalf("accept: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("accept timed out")
	}
	return nil
}

func (ts *testServer) acceptErr(t *testing.T) error {
	t.Helper()
	select {
	case err := <-ts.errs:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("accept error timed out")
	}
	return nil
}

// testClient is the engine side of the wire: it dials, reads envelopes and
// writes them, without using Peer.
type testClient struct {
	t    *testing.T
	conn *websocket.Conn
	in   chan protocol.Envelope
}

func dial(t *testing.T, url, token string) *testClient {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", basicAuth(token, ""))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	require.NoError(t, err)
	conn.SetReadLimit(protocol.MaxMessageBytes)
	c := &testClient{t: t, conn: conn, in: make(chan protocol.Envelope, 128)}
	go c.read()
	t.Cleanup(func() { _ = conn.CloseNow() })
	return c
}

func (c *testClient) read() {
	for {
		typ, data, err := c.conn.Read(context.Background())
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		c.in <- env
	}
}

func (c *testClient) next(t *testing.T) protocol.Envelope {
	t.Helper()
	select {
	case env := <-c.in:
		return env
	case <-time.After(5 * time.Second):
		t.Fatal("no message from peer")
	}
	return protocol.Envelope{}
}

func (c *testClient) send(t *testing.T, env protocol.Envelope) {
	t.Helper()
	data, err := json.Marshal(env)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.conn.Write(ctx, websocket.MessageText, data))
}

func (c *testClient) hello(t *testing.T, id uint64, h protocol.Hello) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEnvelope(id, 0, protocol.TypeHello, h)
	require.NoError(t, err)
	c.send(t, env)
	return c.next(t)
}

func (c *testClient) close() {
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

func TestAcceptRejectsUnauthorized(t *testing.T) {
	var accepted atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := Accept(r.Context(), w, r, testToken, nil)
		if err == nil {
			accepted.Store(true)
			_ = p.Close()
		}
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		auth string
	}{
		{"missing", ""},
		{"wrong token", basicAuth("wrong", "")},
		{"password set", basicAuth(testToken, "x")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL+protocol.EnginePath, nil)
			require.NoError(t, err)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}
	require.False(t, accepted.Load())
}

func TestAcceptHelloWelcome(t *testing.T) {
	ts := newTestServer(t, testToken, nil)
	c := dial(t, ts.url, testToken)
	defer c.close()

	welcome := c.hello(t, 7, validHello())
	require.Equal(t, protocol.TypeWelcome, welcome.Type)
	require.Equal(t, uint64(7), welcome.Re)
	require.NotZero(t, welcome.ID)

	p := ts.accepted(t)
	require.Equal(t, validHello(), p.Hello)
}

func TestAcceptRejectsBadHello(t *testing.T) {
	long := strings.Repeat("v", 4096)
	for _, tc := range []struct {
		name string
		env  protocol.Envelope
	}{
		{"not hello", mustEnvelope(t, 1, 0, protocol.TypeExport, protocol.Export{})},
		{"reply", mustEnvelope(t, 1, 5, protocol.TypeHello, validHello())},
		{"zero id", mustEnvelope(t, 0, 0, protocol.TypeHello, validHello())},
		{"empty cache", mustEnvelope(t, 1, 0, protocol.TypeHello, protocol.Hello{Generation: 1})},
		{"cloud cache", mustEnvelope(t, 1, 0, protocol.TypeHello, protocol.Hello{CacheID: "cloud", Generation: 1})},
		{"zero generation", mustEnvelope(t, 1, 0, protocol.TypeHello, protocol.Hello{CacheID: "cache-1"})},
		{"long version", mustEnvelope(t, 1, 0, protocol.TypeHello, protocol.Hello{CacheID: "cache-1", Generation: 1, EngineVersion: long})},
		{"long name", mustEnvelope(t, 1, 0, protocol.TypeHello, protocol.Hello{CacheID: "cache-1", Generation: 1, EngineName: long})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, testToken, nil)
			c := dial(t, ts.url, testToken)
			defer c.close()
			c.send(t, tc.env)
			require.Error(t, ts.acceptErr(t))
		})
	}
}

func mustEnvelope(t *testing.T, id, re uint64, typ protocol.Type, body any) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEnvelope(id, re, typ, body)
	require.NoError(t, err)
	return env
}

func TestRequestOutOfOrderReplies(t *testing.T) {
	ts := newTestServer(t, testToken, nil)
	c := dial(t, ts.url, testToken)
	defer c.close()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	type result struct {
		id  string
		err error
	}
	results := make(chan result, 3)
	for i := 0; i < 3; i++ {
		go func(i int) {
			var out protocol.Exported
			err := p.Request(context.Background(), protocol.TypeExport, protocol.Export{Roots: []uint64{uint64(i)}}, &out)
			results <- result{out.ExportID, err}
		}(i)
	}

	reqs := make([]protocol.Envelope, 3)
	for i := range reqs {
		reqs[i] = c.next(t)
		require.Equal(t, protocol.TypeExport, reqs[i].Type)
		require.NotZero(t, reqs[i].ID)
	}

	// Reply in reverse order, with IDs above 2^53 to prove they are not floats.
	for i := len(reqs) - 1; i >= 0; i-- {
		id := uint64(1)<<53 + uint64(i) + 1
		c.send(t, mustEnvelope(t, id, reqs[i].ID, protocol.TypeExported, protocol.Exported{ExportID: fmt.Sprintf("export-%d", i)}))
	}

	got := map[string]bool{}
	for i := 0; i < 3; i++ {
		r := <-results
		require.NoError(t, r.err)
		got[r.id] = true
	}
	require.Len(t, got, 3)
}

func TestRequestTimeoutRemovesPending(t *testing.T) {
	ts := newTestServer(t, testToken, nil)
	c := dial(t, ts.url, testToken)
	defer c.close()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var out protocol.Exported
	err := p.Request(ctx, protocol.TypeExport, protocol.Export{}, &out)
	require.Error(t, err)

	req := c.next(t)
	// A late reply is ignored, and the peer keeps serving.
	c.send(t, mustEnvelope(t, 1, req.ID, protocol.TypeExported, protocol.Exported{ExportID: "late"}))

	p.mu.Lock()
	pending := len(p.pending)
	p.mu.Unlock()
	require.Zero(t, pending)

	done := make(chan error, 1)
	go func() {
		var out protocol.Exported
		done <- p.Request(context.Background(), protocol.TypeExport, protocol.Export{}, &out)
	}()
	next := c.next(t)
	c.send(t, mustEnvelope(t, 2, next.ID, protocol.TypeExported, protocol.Exported{ExportID: "ok"}))
	require.NoError(t, <-done)
}

func TestRenewHandlerDoesNotBlockReplies(t *testing.T) {
	release := make(chan struct{})
	handler := func(ctx context.Context, _ *Peer, req protocol.Envelope) (any, error) {
		if req.Type != protocol.TypeRenew {
			return nil, fmt.Errorf("unexpected %s", req.Type)
		}
		select {
		case <-release:
			return protocol.Renewed{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ts := newTestServer(t, testToken, handler)
	c := dial(t, ts.url, testToken)
	defer c.close()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	done := make(chan error, 1)
	go func() {
		var out protocol.Exported
		done <- p.Request(context.Background(), protocol.TypeExport, protocol.Export{}, &out)
	}()
	req := c.next(t)

	c.send(t, mustEnvelope(t, 100, 0, protocol.TypeRenew, protocol.Renew{}))
	c.send(t, mustEnvelope(t, 101, req.ID, protocol.TypeExported, protocol.Exported{ExportID: "x"}))
	require.NoError(t, <-done)

	close(release)
	renewed := c.next(t)
	require.Equal(t, protocol.TypeRenewed, renewed.Type)
	require.Equal(t, uint64(100), renewed.Re)
	require.NotZero(t, renewed.ID)
}

func TestDisconnectCancelsWaiter(t *testing.T) {
	ts := newTestServer(t, testToken, nil)
	c := dial(t, ts.url, testToken)
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	done := make(chan error, 1)
	go func() {
		var out protocol.Exported
		done <- p.Request(context.Background(), protocol.TypeExport, protocol.Export{}, &out)
	}()
	_ = c.next(t)
	c.close()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("request was not canceled")
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not stop")
	}
}

func TestRequestWrongReplyType(t *testing.T) {
	ts := newTestServer(t, testToken, nil)
	c := dial(t, ts.url, testToken)
	defer c.close()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	done := make(chan error, 1)
	go func() {
		var out protocol.Exported
		done <- p.Request(context.Background(), protocol.TypeExport, protocol.Export{}, &out)
	}()
	req := c.next(t)
	c.send(t, mustEnvelope(t, 1, req.ID, protocol.TypeMerged, protocol.Merged{}))
	err := <-done
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected reply")
}

func TestUnknownRequestRepliesError(t *testing.T) {
	var called atomic.Bool
	handler := func(context.Context, *Peer, protocol.Envelope) (any, error) {
		called.Store(true)
		return nil, errors.New("should not be called")
	}
	ts := newTestServer(t, testToken, handler)
	c := dial(t, ts.url, testToken)
	defer c.close()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	c.send(t, mustEnvelope(t, 42, 0, protocol.Type("bogus"), nil))
	reply := c.next(t)
	require.Equal(t, protocol.TypeError, reply.Type)
	require.Equal(t, uint64(42), reply.Re)
	require.NotZero(t, reply.ID)
	require.False(t, called.Load())
}

func TestSecondHelloRepliesError(t *testing.T) {
	ts := newTestServer(t, testToken, nil)
	c := dial(t, ts.url, testToken)
	defer c.close()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	c.send(t, mustEnvelope(t, 9, 0, protocol.TypeHello, validHello()))
	reply := c.next(t)
	require.Equal(t, protocol.TypeError, reply.Type)
	require.Equal(t, uint64(9), reply.Re)
}

func TestHandlerErrorIsGeneric(t *testing.T) {
	handler := func(context.Context, *Peer, protocol.Envelope) (any, error) {
		return nil, fmt.Errorf("handler saw %s", testToken)
	}
	ts := newTestServer(t, testToken, handler)
	c := dial(t, ts.url, testToken)
	defer c.close()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	c.send(t, mustEnvelope(t, 5, 0, protocol.TypeRenew, protocol.Renew{}))
	reply := c.next(t)
	require.Equal(t, protocol.TypeError, reply.Type)
	require.Equal(t, uint64(5), reply.Re)
	var e protocol.Error
	require.NoError(t, reply.Decode(&e))
	require.NotEmpty(t, e.Message)
	require.NotContains(t, e.Message, testToken)
}

func TestPendingBound(t *testing.T) {
	ts := newTestServer(t, testToken, nil)
	c := dial(t, ts.url, testToken)
	defer c.close()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	p.mu.Lock()
	for i := uint64(1); i <= maxPending; i++ {
		p.pending[i] = make(chan protocol.Envelope, 1)
	}
	p.mu.Unlock()

	err := p.Request(context.Background(), protocol.TypeExport, protocol.Export{}, nil)
	require.Error(t, err)

	p.mu.Lock()
	p.pending = map[uint64]chan protocol.Envelope{}
	p.mu.Unlock()
}

func TestInboundBound(t *testing.T) {
	handler := func(context.Context, *Peer, protocol.Envelope) (any, error) {
		return protocol.Renewed{}, nil
	}
	ts := newTestServer(t, testToken, handler)
	c := dial(t, ts.url, testToken)
	defer c.close()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	for i := 0; i < maxInbound; i++ {
		p.inbound <- struct{}{}
	}
	c.send(t, mustEnvelope(t, 77, 0, protocol.TypeRenew, protocol.Renew{}))
	reply := c.next(t)
	require.Equal(t, protocol.TypeError, reply.Type)
	require.Equal(t, uint64(77), reply.Re)
	for i := 0; i < maxInbound; i++ {
		<-p.inbound
	}
}

// TestRequestCanceledWhileWriteBlocked proves a Request whose ctx expires
// returns promptly even while another write is blocked on the connection.
// The custom write mutex used to make the canceled request wait behind the
// blocked writer, so priority renewals could miss their deadline.
func TestRequestCanceledWhileWriteBlocked(t *testing.T) {
	big := strings.Repeat("x", 16<<20)
	handler := func(_ context.Context, _ *Peer, req protocol.Envelope) (any, error) {
		if req.Type == protocol.TypeRenew {
			return big, nil
		}
		return protocol.Exported{}, nil
	}
	ts := newTestServer(t, testToken, handler)

	// Dial without a read loop: after the welcome, the engine stops reading,
	// so the peer's large reply write blocks on the socket buffer.
	header := http.Header{}
	header.Set("Authorization", basicAuth(testToken, ""))
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, ts.url, &websocket.DialOptions{HTTPHeader: header})
	require.NoError(t, err)
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxMessageBytes)

	helloEnv, err := protocol.NewEnvelope(1, 0, protocol.TypeHello, validHello())
	require.NoError(t, err)
	helloData, err := json.Marshal(helloEnv)
	require.NoError(t, err)
	require.NoError(t, conn.Write(dialCtx, websocket.MessageText, helloData))
	_, _, err = conn.Read(dialCtx)
	require.NoError(t, err)

	p := ts.accepted(t)
	defer p.Close()

	// Ask for a huge reply; the peer's write blocks because nobody reads.
	renewEnv, err := protocol.NewEnvelope(2, 0, protocol.TypeRenew, protocol.Renew{})
	require.NoError(t, err)
	renewData, err := json.Marshal(renewEnv)
	require.NoError(t, err)
	require.NoError(t, conn.Write(dialCtx, websocket.MessageText, renewData))

	// Give the peer time to start the blocked write.
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- p.Request(ctx, protocol.TypeExport, protocol.Export{}, nil)
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("canceled request waited behind blocked write")
	}
}

// TestDispatchDropsZeroIDRequest proves a request with ID 0 is dropped: a
// reply with Re 0 would look like a fresh request to the engine.
func TestDispatchDropsZeroIDRequest(t *testing.T) {
	var called atomic.Bool
	handler := func(context.Context, *Peer, protocol.Envelope) (any, error) {
		called.Store(true)
		return protocol.Renewed{}, nil
	}
	ts := newTestServer(t, testToken, handler)
	c := dial(t, ts.url, testToken)
	defer c.close()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	c.send(t, mustEnvelope(t, 0, 0, protocol.TypeRenew, protocol.Renew{}))

	// A valid request after it must still be served, and no reply for the
	// zero-ID request may arrive first.
	c.send(t, mustEnvelope(t, 55, 0, protocol.TypeRenew, protocol.Renew{}))
	reply := c.next(t)
	require.Equal(t, protocol.TypeRenewed, reply.Type)
	require.Equal(t, uint64(55), reply.Re)
	require.True(t, called.Load())
}

// TestReplyEncodingFailureRepliesError proves a handler body that cannot be
// encoded still yields a TypeError reply instead of silence.
func TestReplyEncodingFailureRepliesError(t *testing.T) {
	handler := func(context.Context, *Peer, protocol.Envelope) (any, error) {
		return make(chan int), nil
	}
	ts := newTestServer(t, testToken, handler)
	c := dial(t, ts.url, testToken)
	defer c.close()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()

	c.send(t, mustEnvelope(t, 66, 0, protocol.TypeRenew, protocol.Renew{}))
	reply := c.next(t)
	require.Equal(t, protocol.TypeError, reply.Type)
	require.Equal(t, uint64(66), reply.Re)
}

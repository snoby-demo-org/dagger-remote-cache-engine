// Package peer is the service side of one engine WebSocket: it accepts the
// engine's hello, serves the engine's requests through a handler, and sends
// its own requests over the same connection.
package peer

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/dagger/dagger/engine/remotecache/protocol"
)

const (
	// helloTimeout bounds the wait for the engine's hello after upgrade.
	helloTimeout = 10 * time.Second

	// maxPending bounds the service's requests waiting for replies.
	maxPending = 128

	// maxInbound bounds the engine's requests being handled at once.
	maxInbound = 32

	// maxHelloFieldBytes bounds the hello's identity strings.
	maxHelloFieldBytes = 256

	// maxErrorBytes bounds an error message copied from the wire.
	maxErrorBytes = 256

	// cloudCacheID is the cache identity reserved for the Cloud, which an
	// engine may not claim.
	cloudCacheID = "cloud"
)

// RemoteError is a typed error reported by the engine on the wire. It is
// returned by Request when the engine replies with a TypeError.
type RemoteError struct {
	Op      protocol.Type
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("%s: %s", e.Op, e.Message)
}

var (
	errUnauthorized     = errors.New("peer: unauthorized")
	errTooManyPending   = errors.New("peer: too many pending requests")
	errConnectionClosed = errors.New("peer: connection closed")
)

// RequestHandler serves one engine request. It runs off the read loop, so it
// may block without delaying replies. Its body is sent as the request's reply
// type; an error is sent as a TypeError.
type RequestHandler func(ctx context.Context, p *Peer, request protocol.Envelope) (any, error)

// Peer is one accepted engine connection.
type Peer struct {
	// Hello is the engine's validated hello.
	Hello protocol.Hello

	conn    *websocket.Conn
	handler RequestHandler

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	lastID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan protocol.Envelope

	inbound chan struct{}

	stopOnce  sync.Once
	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

// Accept authenticates the request, upgrades it, reads and validates the
// engine's hello, welcomes it, and starts serving. The peer lives until the
// connection ends or Close is called, independently of the HTTP handler.
func Accept(ctx context.Context, w http.ResponseWriter, r *http.Request, token string, handler RequestHandler) (*Peer, error) {
	if !authorized(r, token) {
		w.Header().Set("WWW-Authenticate", `Basic realm="dcache"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, errUnauthorized
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionNoContextTakeover,
	})
	if err != nil {
		return nil, fmt.Errorf("peer: upgrade: %w", err)
	}
	conn.SetReadLimit(protocol.MaxMessageBytes)

	helloCtx, cancel := context.WithTimeout(ctx, helloTimeout)
	env, err := readEnvelope(helloCtx, conn)
	cancel()
	if err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("peer: hello: %w", err)
	}
	if err := validateHello(env); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	var hello protocol.Hello
	if err := env.Decode(&hello); err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("peer: hello: %w", err)
	}

	p := &Peer{
		Hello:   hello,
		conn:    conn,
		handler: handler,
		ctx:     context.WithoutCancel(ctx),
		done:    make(chan struct{}),
		pending: map[uint64]chan protocol.Envelope{},
		inbound: make(chan struct{}, maxInbound),
	}
	p.ctx, p.cancel = context.WithCancel(p.ctx)

	if err := p.reply(env, protocol.TypeWelcome, protocol.Welcome{}); err != nil {
		p.stop()
		_ = conn.CloseNow()
		return nil, fmt.Errorf("peer: welcome: %w", err)
	}

	p.wg.Add(2)
	go p.readLoop()
	go p.pingLoop()
	return p, nil
}

// authorized checks the Basic credentials: the username is the token, in
// constant time, and the password is empty.
func authorized(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(user), []byte(token)) != 1 {
		return false
	}
	return pass == ""
}

// readEnvelope reads one text message and decodes it as an envelope.
func readEnvelope(ctx context.Context, conn *websocket.Conn) (protocol.Envelope, error) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return protocol.Envelope{}, err
		}
		if typ != websocket.MessageText {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return protocol.Envelope{}, err
		}
		return env, nil
	}
}

// validateHello checks the first message is a well-formed hello.
func validateHello(env protocol.Envelope) error {
	if env.Type != protocol.TypeHello {
		return fmt.Errorf("peer: first message is %q, not hello", bounded(string(env.Type), 64))
	}
	if env.Re != 0 {
		return errors.New("peer: hello is a reply")
	}
	if env.ID == 0 {
		return errors.New("peer: hello has no id")
	}
	var hello protocol.Hello
	if err := env.Decode(&hello); err != nil {
		return fmt.Errorf("peer: hello: %w", err)
	}
	if hello.CacheID == "" {
		return errors.New("peer: hello has no cache id")
	}
	if hello.CacheID == cloudCacheID {
		return errors.New("peer: hello claims the cloud cache id")
	}
	if len(hello.CacheID) > maxHelloFieldBytes {
		return errors.New("peer: hello cache id too long")
	}
	if hello.Generation == 0 {
		return errors.New("peer: hello has no generation")
	}
	if len(hello.EngineVersion) > maxHelloFieldBytes {
		return errors.New("peer: hello engine version too long")
	}
	if len(hello.EngineName) > maxHelloFieldBytes {
		return errors.New("peer: hello engine name too long")
	}
	return nil
}

// Request sends a request and waits for its reply, decoding the reply body
// into out when out is not nil. It fails on a TypeError reply, a reply of the
// wrong type, ctx ending, or the connection ending.
func (p *Peer) Request(ctx context.Context, typ protocol.Type, body any, out any) error {
	want, ok := protocol.ReplyType(typ)
	if !ok {
		return fmt.Errorf("peer: %q is not a request", bounded(string(typ), 64))
	}
	id := p.lastID.Add(1)
	env, err := protocol.NewEnvelope(id, 0, typ, body)
	if err != nil {
		return err
	}

	waiter := make(chan protocol.Envelope, 1)
	p.mu.Lock()
	if len(p.pending) >= maxPending {
		p.mu.Unlock()
		return errTooManyPending
	}
	p.pending[id] = waiter
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()

	if err := p.write(ctx, env); err != nil {
		return err
	}

	select {
	case reply := <-waiter:
		if reply.Type == protocol.TypeError {
			var e protocol.Error
			if err := reply.Decode(&e); err != nil {
				return fmt.Errorf("%s: decode error reply: %w", typ, err)
			}
			return &RemoteError{Op: typ, Message: bounded(e.Message, maxErrorBytes)}
		}
		if reply.Type != want {
			return fmt.Errorf("%s: unexpected reply %q", typ, bounded(string(reply.Type), 64))
		}
		if out != nil {
			if err := reply.Decode(out); err != nil {
				return err
			}
		}
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-p.ctx.Done():
		return errConnectionClosed
	}
}

// Done is closed when the connection ends.
func (p *Peer) Done() <-chan struct{} {
	return p.done
}

// Close ends the connection and waits for the peer's goroutines. It must not
// be called from within a RequestHandler: Close waits for the handler's
// goroutine, so calling it there deadlocks. Handlers should return instead.
func (p *Peer) Close() error {
	p.stop()
	p.closeOnce.Do(func() {
		p.closeErr = p.conn.CloseNow()
	})
	p.wg.Wait()
	return p.closeErr
}

// stop cancels the peer's context and closes Done, once.
func (p *Peer) stop() {
	p.stopOnce.Do(func() {
		p.cancel()
		close(p.done)
	})
}

// readLoop reads messages until the connection ends, routing replies to their
// waiters and requests to handlers.
func (p *Peer) readLoop() {
	defer p.wg.Done()
	for {
		typ, data, err := p.conn.Read(p.ctx)
		if err != nil {
			p.abortWrite()
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		p.dispatch(env)
	}
}

// dispatch routes one message. Replies go to their waiter; requests run in a
// handler goroutine, bounded by inbound slots, so blocked work never delays
// reply routing.
func (p *Peer) dispatch(env protocol.Envelope) {
	if env.Re != 0 {
		p.mu.Lock()
		waiter := p.pending[env.Re]
		delete(p.pending, env.Re)
		p.mu.Unlock()
		if waiter != nil {
			select {
			case waiter <- env:
			default:
			}
		}
		return
	}
	if env.ID == 0 {
		// A reply would carry Re 0, which the engine reads as a new request.
		return
	}

	select {
	case p.inbound <- struct{}{}:
	case <-p.ctx.Done():
		return
	default:
		p.replyError(env, "too many requests")
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.inbound }()
		p.handle(env)
	}()
}

// handle serves one request through the handler and replies.
func (p *Peer) handle(env protocol.Envelope) {
	if env.Type == protocol.TypeHello {
		p.replyError(env, "unexpected hello")
		return
	}
	replyType, ok := protocol.ReplyType(env.Type)
	if !ok {
		p.replyError(env, fmt.Sprintf("unexpected request %q", bounded(string(env.Type), 64)))
		return
	}
	if p.handler == nil {
		p.replyError(env, "no handler")
		return
	}
	body, err := p.handler(p.ctx, p, env)
	if err != nil {
		p.replyError(env, "request failed")
		return
	}
	if err := p.reply(env, replyType, body); err != nil {
		p.replyError(env, "reply failed")
	}
}

// reply answers a request with its reply type and a fresh ID.
func (p *Peer) reply(to protocol.Envelope, typ protocol.Type, body any) error {
	env, err := protocol.NewEnvelope(p.lastID.Add(1), to.ID, typ, body)
	if err != nil {
		return err
	}
	return p.write(p.ctx, env)
}

// replyError answers a request with a bounded TypeError.
func (p *Peer) replyError(to protocol.Envelope, message string) {
	env, err := protocol.NewEnvelope(p.lastID.Add(1), to.ID, protocol.TypeError, protocol.Error{
		Message: bounded(message, maxErrorBytes),
	})
	if err != nil {
		return
	}
	_ = p.write(p.ctx, env)
}

// write sends one message onto the connection. coder/websocket serializes
// concurrent writes internally and honors ctx while waiting, so no custom
// mutex is needed; one would let a blocked write delay a priority renewal
// past its deadline.
func (p *Peer) write(ctx context.Context, env protocol.Envelope) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	writer, err := p.conn.Writer(ctx, websocket.MessageText)
	if err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		p.abortWrite()
		return err
	}
	if err := writer.Close(); err != nil {
		p.abortWrite()
		return err
	}
	return nil
}

// abortWrite tears down the connection after a failed message write so that
// waiters are released instead of wedging on a stuck message writer.
func (p *Peer) abortWrite() {
	p.stop()
	p.closeOnce.Do(func() {
		p.closeErr = p.conn.CloseNow()
	})
}

// pingLoop pings the engine periodically, and stops the peer on a ping
// without a pong within the timeout.
func (p *Peer) pingLoop() {
	defer p.wg.Done()
	t := time.NewTicker(protocol.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(p.ctx, protocol.PingTimeout)
		err := p.conn.Ping(ctx)
		cancel()
		if err != nil {
			p.stop()
			return
		}
	}
}

// bounded cuts s to at most n bytes.
func bounded(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

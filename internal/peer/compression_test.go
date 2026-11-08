package peer

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dagger/dagger/engine/remotecache/protocol"
)

func TestCompressedPreCanceledRequestDoesNotWedgeOpenConnection(t *testing.T) {
	ts := newTestServer(t, testToken, nil)
	header := http.Header{}
	header.Set("Authorization", basicAuth(testToken, ""))
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()
	conn, _, err := websocket.Dial(dialCtx, ts.url, &websocket.DialOptions{HTTPHeader: header, CompressionMode: websocket.CompressionNoContextTakeover})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxMessageBytes)
	c := &testClient{t: t, conn: conn, in: make(chan protocol.Envelope, 128)}
	go c.read()
	c.hello(t, 1, validHello())
	p := ts.accepted(t)
	defer p.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case env := <-c.in:
				if env.Re != 0 || env.Type != protocol.TypeExport {
					continue
				}
				reply, err := protocol.NewEnvelope(env.ID+1000, env.ID, protocol.TypeExported, protocol.Exported{})
				if err != nil {
					return
				}
				raw, err := json.Marshal(reply)
				if err != nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				err = conn.Write(ctx, websocket.MessageText, raw)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}()
	for i := 0; i < 64; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := p.Request(ctx, protocol.TypeExport, protocol.Export{}, nil); err == nil {
			t.Fatal("pre-canceled request succeeded")
		}
		liveCtx, cancelLive := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := p.Request(liveCtx, protocol.TypeExport, protocol.Export{}, nil)
		cancelLive()
		if err != nil {
			select {
			case <-p.Done():
				return
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("compressed write wedged an open connection: %v", err)
			}
		}
	}
}

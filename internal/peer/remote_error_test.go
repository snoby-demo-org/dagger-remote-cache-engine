package peer

import (
	"context"
	"github.com/dagger/dagger/engine/remotecache/protocol"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestRemoteErrorReplyIsTypedWithoutDisconnect(t *testing.T) {
	ts := newTestServer(t, testToken, nil)
	c := dial(t, ts.url, testToken)
	require.Equal(t, protocol.TypeWelcome, c.hello(t, 1, validHello()).Type)
	p := ts.accepted(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		var out protocol.Exported
		result <- p.Request(ctx, protocol.TypeExport, protocol.Export{Roots: []uint64{7}}, &out)
	}()
	req := c.next(t)
	reply, err := protocol.NewEnvelope(2, req.ID, protocol.TypeError, protocol.Error{Message: "not exportable"})
	require.NoError(t, err)
	c.send(t, reply)
	var remote *RemoteError
	require.ErrorAs(t, <-result, &remote)
	require.Contains(t, remote.Error(), "not exportable")
	select {
	case <-p.Done():
		t.Fatal("ordinary remote error disconnected peer")
	default:
	}
}

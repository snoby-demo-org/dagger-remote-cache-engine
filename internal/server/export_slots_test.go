package server

import (
	"github.com/snoby/dcache/internal/peer"
	"testing"
)

func TestExportCaptureSlotsAreBoundedPerPeer(t *testing.T) {
	s := &Server{}
	a, b := &peer.Peer{}, &peer.Peer{}
	slots := s.exportSlotsFor(a)
	if cap(slots) != 4 {
		t.Fatalf("capture slots=%d want4", cap(slots))
	}
	if s.exportSlotsFor(a) != slots {
		t.Fatal("same peer lost its gate")
	}
	if s.exportSlotsFor(b) == slots {
		t.Fatal("independent peers share capture gate")
	}
	for i := 0; i < 4; i++ {
		slots <- struct{}{}
	}
	select {
	case slots <- struct{}{}:
		t.Fatal("fifth capture admitted")
	default:
	}
	for i := 0; i < 4; i++ {
		<-slots
	}
}

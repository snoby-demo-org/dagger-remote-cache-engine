package server

import (
	"testing"
	"time"
)

func TestDefaultUploadTimeoutAllowsLargeTransfers(t *testing.T) {
	s := newServer(t, baseConfig(t))
	if s.cfg.UploadTimeout != 15*time.Minute {
		t.Errorf("upload default = %s; want 15m", s.cfg.UploadTimeout)
	}
}

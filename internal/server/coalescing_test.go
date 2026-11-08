package server

import (
	"context"
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/snoby/dcache/internal/telemetry"
)

func TestActiveExportCoalescesLatestObservationBeforeSemaphore(t *testing.T) {
	key := dagql.HolderKey{Cache: "engine", Number: 7}
	s := &Server{ctx: context.Background(), exporting: map[dagql.HolderKey]bool{key: true}, dirty: make(map[dagql.HolderKey]telemetry.Event), exportSem: make(chan struct{}, 1)}
	s.exportSem <- struct{}{}
	s.onObservation(telemetry.Event{Key: key, Recipe: "old"})
	s.onObservation(telemetry.Event{Key: key, Recipe: "latest", Parts: []dagql.PersistedPartAddress{{Part: "fs"}}})
	if s.stats.Dropped.Load() != 0 {
		t.Fatal("latest state dropped while its export is active")
	}
	queued, ok := s.dirty[key]
	if !ok || queued.Recipe != "latest" || len(queued.Parts) != 1 {
		t.Fatalf("latest state not coalesced: %#v", queued)
	}
	if len(s.dirty) != 1 || len(s.exportSem) != 1 {
		t.Fatal("coalescing grew beyond one event or consumed a worker slot")
	}
}

func TestUnrelatedExportStillDropsWhenBoundedPoolFull(t *testing.T) {
	key := dagql.HolderKey{Cache: "engine", Number: 8}
	s := &Server{ctx: context.Background(), exporting: map[dagql.HolderKey]bool{}, dirty: make(map[dagql.HolderKey]telemetry.Event), exportSem: make(chan struct{}, 1)}
	s.exportSem <- struct{}{}
	s.onObservation(telemetry.Event{Key: key, Recipe: "unrelated"})
	if s.stats.Dropped.Load() != 1 {
		t.Fatal("a full worker pool must remain bounded")
	}
	if len(s.dirty) != 0 {
		t.Fatal("inactive entries must not accumulate in dirty state")
	}
}

func TestDuplicateWorkerReleasesItsSemaphoreSlot(t *testing.T) {
	key := dagql.HolderKey{Cache: "engine", Number: 7}
	s := &Server{ctx: context.Background(), exporting: map[dagql.HolderKey]bool{key: true}, dirty: make(map[dagql.HolderKey]telemetry.Event), exportSem: make(chan struct{}, 1)}
	s.exportSem <- struct{}{}
	s.runExport(telemetry.Event{Key: key, Recipe: "latest"}, 0)
	if len(s.exportSem) != 0 {
		t.Fatal("duplicate worker stranded an export slot")
	}
	if !s.exporting[key] || s.dirty[key].Recipe != "latest" {
		t.Fatal("duplicate worker corrupted the owning worker")
	}
}

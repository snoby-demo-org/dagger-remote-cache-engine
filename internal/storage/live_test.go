//go:build integration

package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRealS3RoundTrip is a functional regression test against a real
// S3-compatible server (RustFS). It requires DCACHE_TEST_S3 to point at
// the server base URL (e.g. http://127.0.0.1:9000) and is skipped
// otherwise. Run with: go test -tags integration ./internal/storage/
func TestRealS3RoundTrip(t *testing.T) {
	endpoint := os.Getenv("DCACHE_TEST_S3")
	if endpoint == "" {
		t.Skip("DCACHE_TEST_S3 not set; skipping live S3 test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := New(Options{
		Endpoint:       endpoint,
		PublicEndpoint: endpoint,
		Region:         "us-east-1",
		Bucket:         "dcache-live-test",
		AccessKey:      "dcachetest",
		SecretKey:      "dcachetest-secret",
		Pool:           "shared",
		URLTTL:         1 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// EnsureBucket must succeed against the authenticated server.
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}

	// Deterministic body > 2MiB so the old 1MiB verification limit
	// would truncate and mismatch.
	body := make([]byte, 2<<20+256*1024) // 2MiB + 256KiB
	for i := range body {
		body[i] = byte(i % 251)
	}
	wantSum := sha256.Sum256(body)
	key := "sha256:" + fmt.Sprintf("%x", wantSum)

	// Signed PUT of the original body.
	putURL, err := store.PresignPut(ctx, key, int64(len(body)))
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build PUT request: %v", err)
	}
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT request: %v", err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 200))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.Fatalf("PUT status = %d, want 2xx", resp.StatusCode)
	}

	// Existence check with exact size.
	ok, err := store.Exists(ctx, key, int64(len(body)))
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Fatal("Exists: object not found after PUT")
	}

	// Verify must succeed on the intact object.
	if err := store.Verify(ctx, key, int64(len(body))); err != nil {
		t.Fatalf("Verify (intact): %v", err)
	}

	// Signed GET with a byte range: bytes=5-30 (26 bytes).
	getURL, _, err := store.PresignGet(ctx, key)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, getURL, nil)
	if err != nil {
		t.Fatalf("build GET request: %v", err)
	}
	getReq.Header.Set("Range", "bytes=5-30")
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("GET request: %v", err)
	}
	rangeBody, readErr := io.ReadAll(io.LimitReader(getResp.Body, 1<<20))
	getResp.Body.Close()
	if readErr != nil {
		t.Fatalf("read GET body: %v", readErr)
	}
	if getResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("GET status = %d, want 206", getResp.StatusCode)
	}
	wantCR := fmt.Sprintf("bytes 5-30/%d", len(body))
	if got := getResp.Header.Get("Content-Range"); got != wantCR {
		t.Fatalf("Content-Range = %q, want %q", got, wantCR)
	}
	wantRange := body[5:31]
	if !bytes.Equal(rangeBody, wantRange) {
		t.Fatalf("range body mismatch: got %d bytes, want %d", len(rangeBody), len(wantRange))
	}

	// Corrupt the object in place with a same-size buffer and confirm
	// Verify now fails.
	corrupt := make([]byte, len(body))
	if _, err := rand.Read(corrupt); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if bytes.Equal(corrupt, body) {
		t.Fatal("corrupt buffer unexpectedly identical to original")
	}
	putURL2, err := store.PresignPut(ctx, key, int64(len(corrupt)))
	if err != nil {
		t.Fatalf("PresignPut (corrupt): %v", err)
	}
	req2, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL2, bytes.NewReader(corrupt))
	if err != nil {
		t.Fatalf("build corrupt PUT request: %v", err)
	}
	req2.ContentLength = int64(len(corrupt))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("corrupt PUT request: %v", err)
	}
	io.Copy(io.Discard, io.LimitReader(resp2.Body, 200))
	resp2.Body.Close()
	if resp2.StatusCode < 200 || resp2.StatusCode > 299 {
		t.Fatalf("corrupt PUT status = %d, want 2xx", resp2.StatusCode)
	}

	if err := store.Verify(ctx, key, int64(len(corrupt))); err == nil {
		t.Fatal("Verify (corrupted): expected error, got nil")
	}

	// Cleanup: delete only our own test object.
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, err := store.Exists(ctx, key, int64(len(corrupt))); err != nil {
		t.Fatalf("Exists after Delete: %v", err)
	} else if ok {
		t.Fatal("object still exists after Delete")
	}
}

// boundedError truncates an error/body string to at most 200 bytes so
// signed URLs or large payloads are never printed in test output.
func boundedError(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return strings.TrimSpace(s)
}

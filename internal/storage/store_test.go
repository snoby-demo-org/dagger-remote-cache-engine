package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testRegion = "us-east-1"
	testBucket = "dcache"
	testPool   = "pool1"
	testAccess = "test-access-key"
	testSecret = "test-secret-key-0123456789abcdef"
)

// s3Fixture is a minimal in-memory S3 API. It independently recomputes SigV4
// presigned query signatures so that presigned PUT/GET flows are genuinely
// signature-gated, supports Range GET, bucket creation races and listing.
type s3Fixture struct {
	srv     *httptest.Server
	url     string
	mu      sync.Mutex
	buckets map[string]bool
	objs    map[string][]byte
	secret  string
	region  string
	deny    bool // force AccessDenied on bucket creation
	reqs    int
}

func newFixture(t *testing.T) *s3Fixture {
	t.Helper()
	f := &s3Fixture{
		buckets: map[string]bool{},
		objs:    map[string][]byte{},
		secret:  testSecret,
		region:  testRegion,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	f.url = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

func (f *s3Fixture) store(t *testing.T, mutate func(*Options)) *Store {
	t.Helper()
	opts := Options{
		Endpoint:  f.url,
		Region:    testRegion,
		Bucket:    testBucket,
		AccessKey: testAccess,
		SecretKey: testSecret,
		Pool:      testPool,
		URLTTL:    15 * time.Minute,
	}
	if mutate != nil {
		mutate(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func (f *s3Fixture) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs++
	f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	switch {
	case r.Method == http.MethodPut && key == "":
		f.createBucket(w, bucket)
	case r.Method == http.MethodPut:
		f.putObject(w, r, bucket, key)
	case r.Method == http.MethodHead:
		f.headObject(w, bucket, key)
	case r.Method == http.MethodGet && key == "":
		f.listObjects(w, r, bucket)
	case r.Method == http.MethodGet:
		f.getObject(w, r, bucket, key)
	case r.Method == http.MethodDelete:
		f.deleteObject(w, bucket, key)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, code)
}

func (f *s3Fixture) createBucket(w http.ResponseWriter, bucket string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deny {
		s3Error(w, http.StatusForbidden, "AccessDenied")
		return
	}
	if f.buckets[bucket] {
		s3Error(w, http.StatusConflict, "BucketAlreadyOwnedByYou")
		return
	}
	f.buckets[bucket] = true
	w.WriteHeader(http.StatusOK)
}

func (f *s3Fixture) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	// Object writes are only allowed via presigned (signed) requests.
	if !f.verifySig(r) {
		s3Error(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s3Error(w, http.StatusBadRequest, "IncompleteBody")
		return
	}
	f.mu.Lock()
	f.objs[bucket+"/"+key] = body
	f.mu.Unlock()
	sum := sha256.Sum256(body)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.WriteHeader(http.StatusOK)
}

func (f *s3Fixture) headObject(w http.ResponseWriter, bucket, key string) {
	f.mu.Lock()
	obj, ok := f.objs[bucket+"/"+key]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(obj)))
	sum := sha256.Sum256(obj)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func (f *s3Fixture) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	// Presigned GETs must carry a valid signature; internal (unsigned) GETs
	// from the trusted service are accepted.
	if r.URL.Query().Get("X-Amz-Algorithm") != "" && !f.verifySig(r) {
		s3Error(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	f.mu.Lock()
	obj, ok := f.objs[bucket+"/"+key]
	f.mu.Unlock()
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	status, body := http.StatusOK, obj
	if rng := r.Header.Get("Range"); rng != "" {
		var start, end int
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err == nil && start <= end && start < len(obj) {
			if end >= len(obj) {
				end = len(obj) - 1
			}
			body = obj[start : end+1]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(obj)))
			status = http.StatusPartialContent
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

type xmlContents struct {
	Key          string    `xml:"Key"`
	Size         int64     `xml:"Size"`
	LastModified time.Time `xml:"LastModified"`
}

func (f *s3Fixture) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	prefix := r.URL.Query().Get("prefix")
	f.mu.Lock()
	defer f.mu.Unlock()
	var res struct {
		XMLName     xml.Name      `xml:"ListBucketResult"`
		IsTruncated bool          `xml:"IsTruncated"`
		Contents    []xmlContents `xml:"Contents"`
	}
	keys := make([]string, 0, len(f.objs))
	for k := range f.objs {
		if strings.HasPrefix(k, bucket+"/"+prefix) {
			keys = append(keys, strings.TrimPrefix(k, bucket+"/"))
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		res.Contents = append(res.Contents, xmlContents{
			Key:          k,
			Size:         int64(len(f.objs[bucket+"/"+k])),
			LastModified: time.Now().UTC().Truncate(time.Second),
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml.NewEncoder(w).Encode(res)
}

func (f *s3Fixture) deleteObject(w http.ResponseWriter, bucket, key string) {
	f.mu.Lock()
	_, ok := f.objs[bucket+"/"+key]
	delete(f.objs, bucket+"/"+key)
	f.mu.Unlock()
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *s3Fixture) hmac(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// verifySig independently recomputes the SigV4 presigned query signature.
func (f *s3Fixture) verifySig(r *http.Request) bool {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		return false
	}
	got := q.Get("X-Amz-Signature")
	cred := strings.Split(q.Get("X-Amz-Credential"), "/")
	if got == "" || len(cred) != 5 {
		return false
	}
	type pair struct{ k, v string }
	var pairs []pair
	for k, vs := range q {
		if k == "X-Amz-Signature" {
			continue
		}
		for _, v := range vs {
			pairs = append(pairs, pair{k, v})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var cq strings.Builder
	for i, p := range pairs {
		if i > 0 {
			cq.WriteByte('&')
		}
		cq.WriteString(s3Escape(p.k))
		cq.WriteByte('=')
		cq.WriteString(s3Escape(p.v))
	}
	creq := strings.Join([]string{
		r.Method,
		r.URL.EscapedPath(),
		cq.String(),
		"host:" + r.Host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	sum := sha256.Sum256([]byte(creq))
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		q.Get("X-Amz-Date"),
		strings.Join(cred[1:], "/"),
		hex.EncodeToString(sum[:]),
	}, "\n")
	kDate := f.hmac([]byte("AWS4"+f.secret), cred[1])
	kRegion := f.hmac(kDate, cred[2])
	kService := f.hmac(kRegion, cred[3])
	kSign := f.hmac(kService, cred[4])
	want := hex.EncodeToString(f.hmac(kSign, sts))
	return hmac.Equal([]byte(want), []byte(got))
}

func s3Escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// upload is a helper performing a presigned raw PUT of content.
func upload(t *testing.T, s *Store, content []byte) string {
	t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	u, err := s.PresignPut(ctx, digest, int64(len(content)))
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("raw PUT request: %v", err)
	}
	req.ContentLength = int64(len(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw PUT status = %d, want 200", resp.StatusCode)
	}
	return digest
}

func TestPresignPutRawUploadAndVerify(t *testing.T) {
	f := newFixture(t)
	s := f.store(t, nil)
	ctx := context.Background()
	content := []byte("hello dcache blob")
	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])
	digest := "sha256:" + hexSum

	u, err := s.PresignPut(ctx, digest, int64(len(content)))
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if !strings.Contains(u, "X-Amz-Signature=") {
		t.Fatalf("presigned URL missing signature: %s", u)
	}
	wantPath := "/" + testBucket + "/pools/" + testPool + "/blobs/sha256/" + hexSum
	if !strings.Contains(u, wantPath) {
		t.Fatalf("presigned URL key mismatch: %s", u)
	}

	// Unsigned PUT must be rejected: proves the presigned flow is signature-gated.
	bare := f.url + wantPath
	req, _ := http.NewRequest(http.MethodPut, bare, bytes.NewReader(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unsigned PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned PUT status = %d, want 403", resp.StatusCode)
	}

	// Signed raw PUT with no custom headers.
	req, _ = http.NewRequest(http.MethodPut, u, bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw PUT status = %d, want 200", resp.StatusCode)
	}

	if err := s.Verify(ctx, digest, int64(len(content))); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := s.Verify(ctx, digest, int64(len(content))+1); err == nil {
		t.Fatal("Verify with wrong size: want error, got nil")
	}
	ok, err := s.Exists(ctx, digest, int64(len(content)))
	if err != nil || !ok {
		t.Fatalf("Exists = %v, %v; want true, nil", ok, err)
	}
	// Tampered content must fail verification.
	f.mu.Lock()
	f.objs[testBucket+"/pools/"+testPool+"/blobs/sha256/"+hexSum] = []byte("tampered!")
	f.mu.Unlock()
	if err := s.Verify(ctx, digest, int64(len(content))); err == nil {
		t.Fatal("Verify tampered content: want error, got nil")
	}
}

func TestPresignGetRange(t *testing.T) {
	f := newFixture(t)
	s := f.store(t, nil)
	ctx := context.Background()
	content := []byte("0123456789abcdef")
	digest := upload(t, s, content)

	u, exp, err := s.PresignGet(ctx, digest)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if !strings.Contains(u, "X-Amz-Signature=") {
		t.Fatalf("presigned GET missing signature: %s", u)
	}
	if !exp.After(time.Now().Add(14 * time.Minute)) {
		t.Fatalf("expiry %v not ~15m out", exp)
	}

	// Full GET.
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("raw GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("full GET status=%d body=%q", resp.StatusCode, body)
	}

	// Range GET.
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Range", "bytes=2-5")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range GET: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range GET status = %d, want 206", resp.StatusCode)
	}
	if !bytes.Equal(body, content[2:6]) {
		t.Fatalf("range GET body = %q, want %q", body, content[2:6])
	}
}

func TestExistsAndErrorPropagation(t *testing.T) {
	f := newFixture(t)
	s := f.store(t, nil)
	ctx := context.Background()
	content := []byte("exists-check")
	digest := upload(t, s, content)

	ok, err := s.Exists(ctx, digest, int64(len(content)))
	if err != nil || !ok {
		t.Fatalf("Exists present = %v, %v; want true, nil", ok, err)
	}
	if _, err := s.Exists(ctx, digest, int64(len(content))+1); err == nil {
		t.Fatal("Exists wrong size: want error, got nil")
	}
	missing := "sha256:" + strings.Repeat("ab", 32)
	ok, err = s.Exists(ctx, missing, 5)
	if err != nil {
		t.Fatalf("Exists missing: %v", err)
	}
	if ok {
		t.Fatal("Exists missing = true, want false")
	}

	// Unreachable endpoint must propagate a storage error, not false.
	s2 := f.store(t, func(o *Options) { o.Endpoint = "http://127.0.0.1:1" })
	if _, err := s2.Exists(ctx, missing, 5); err == nil {
		t.Fatal("Exists on unreachable endpoint: want error, got nil")
	}
}

func TestVerifyContextCancellation(t *testing.T) {
	f := newFixture(t)
	s := f.store(t, nil)
	content := []byte("cancel-me")
	digest := upload(t, s, content)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Verify(ctx, digest, int64(len(content))); !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify cancelled ctx err = %v, want context.Canceled", err)
	}
}

func TestEnsureBucket(t *testing.T) {
	f := newFixture(t)
	s := f.store(t, nil)
	ctx := context.Background()
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	f.mu.Lock()
	created := f.buckets[testBucket]
	f.mu.Unlock()
	if !created {
		t.Fatal("bucket not created")
	}
	// Exists race: 409 BucketAlreadyOwnedByYou must be tolerated.
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket race: %v", err)
	}
	// Access denied must not be swallowed.
	s2 := f.store(t, func(o *Options) { o.Bucket = "other-bucket" })
	f.mu.Lock()
	f.deny = true
	f.mu.Unlock()
	if err := s2.EnsureBucket(ctx); err == nil {
		t.Fatal("EnsureBucket access denied: want error, got nil")
	}
}

func TestListOwnPoolValidDigestsOnly(t *testing.T) {
	f := newFixture(t)
	s := f.store(t, nil)
	ctx := context.Background()
	content := []byte("listed blob")
	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])
	digest := "sha256:" + hexSum
	upload(t, s, content)

	f.mu.Lock()
	f.objs[testBucket+"/pools/other/blobs/sha256/"+hexSum] = content
	f.objs[testBucket+"/pools/"+testPool+"/blobs/sha256/notvalid"] = content
	f.objs[testBucket+"/pools/"+testPool+"/misc/stray"] = content
	f.mu.Unlock()

	objs, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("List returned %d objects, want 1: %+v", len(objs), objs)
	}
	if objs[0].Digest != digest || objs[0].Size != int64(len(content)) || objs[0].Modified.IsZero() {
		t.Fatalf("unexpected object %+v", objs[0])
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	s := f.store(t, nil)
	ctx := context.Background()
	content := []byte("delete me")
	digest := upload(t, s, content)
	if err := s.Delete(ctx, digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ok, err := s.Exists(ctx, digest, int64(len(content)))
	if err != nil || ok {
		t.Fatalf("Exists after delete = %v, %v; want false, nil", ok, err)
	}
	if err := s.Delete(ctx, digest); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	base := Options{
		Endpoint: "http://127.0.0.1:9000", Region: testRegion, Bucket: testBucket,
		AccessKey: testAccess, SecretKey: testSecret, Pool: testPool,
	}
	cases := []struct {
		name   string
		mutate func(*Options)
	}{
		{"bad scheme", func(o *Options) { o.Endpoint = "ftp://h:1" }},
		{"no host", func(o *Options) { o.Endpoint = "http://" }},
		{"empty bucket", func(o *Options) { o.Bucket = "" }},
		{"empty access", func(o *Options) { o.AccessKey = "" }},
		{"empty secret", func(o *Options) { o.SecretKey = "" }},
		{"pool with slash", func(o *Options) { o.Pool = "a/b" }},
		{"pool dotdot", func(o *Options) { o.Pool = ".." }},
		{"ttl too large", func(o *Options) { o.URLTTL = 8 * 24 * time.Hour }},
		{"ttl negative", func(o *Options) { o.URLTTL = -time.Minute }},
		{"bad public endpoint", func(o *Options) { o.PublicEndpoint = "gopher://h" }},
	}
	for _, tc := range cases {
		o := base
		tc.mutate(&o)
		if _, err := New(o); err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
	}
}

func TestPresignValidationBeforeIO(t *testing.T) {
	f := newFixture(t)
	s := f.store(t, nil)
	ctx := context.Background()
	bad := []string{"sha256:ABC", "md5:" + strings.Repeat("ab", 16), "sha256:zz", "nope"}
	for _, d := range bad {
		if _, err := s.PresignPut(ctx, d, 1); err == nil {
			t.Errorf("PresignPut %q: want error", d)
		}
		if _, _, err := s.PresignGet(ctx, d); err == nil {
			t.Errorf("PresignGet %q: want error", d)
		}
	}
	if _, err := s.PresignPut(ctx, "sha256:"+strings.Repeat("ab", 32), -1); err == nil {
		t.Error("PresignPut negative size: want error")
	}
	f.mu.Lock()
	n := f.reqs
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("invalid inputs caused %d fixture requests, want 0", n)
	}
}

func TestTTLDefaultAndBound(t *testing.T) {
	f := newFixture(t)
	// TTL zero defaults to 15m.
	s := f.store(t, func(o *Options) { o.URLTTL = 0 })
	u, _, err := s.PresignGet(context.Background(), "sha256:"+strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if !strings.Contains(u, "X-Amz-Expires=900") {
		t.Fatalf("default TTL not 900s: %s", u)
	}
	// TTL bounded to 7d.
	s2 := f.store(t, func(o *Options) { o.URLTTL = 7 * 24 * time.Hour })
	u2, _, err := s2.PresignGet(context.Background(), "sha256:"+strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("PresignGet 7d: %v", err)
	}
	if !strings.Contains(u2, "X-Amz-Expires=604800") {
		t.Fatalf("7d TTL not 604800s: %s", u2)
	}
}

func TestPublicEndpointPresigner(t *testing.T) {
	f := newFixture(t)
	s := f.store(t, func(o *Options) { o.PublicEndpoint = f.url; o.Endpoint = "http://127.0.0.1:1" })
	content := []byte("public endpoint blob")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	u, err := s.PresignPut(context.Background(), digest, int64(len(content)))
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if !strings.HasPrefix(u, f.url+"/") {
		t.Fatalf("presigned URL not on public endpoint: %s", u)
	}
	req, _ := http.NewRequest(http.MethodPut, u, bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw PUT status = %d, want 200", resp.StatusCode)
	}
}

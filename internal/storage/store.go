// Package storage provides an S3-compatible content-addressed blob store with
// presigned upload/download URLs and digest verification.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"

	"github.com/minio/minio-go/v7/pkg/signer"
	"strconv"
	"strings"
	"time"
)

// Options configures a Store.
type Options struct {
	Endpoint       string        // internal http(s) URL including port
	PublicEndpoint string        // optional engine-visible http(s) URL; separate presigner if set
	Region         string        // S3 region (defaults to us-east-1)
	Bucket         string        // bucket name
	AccessKey      string        // S3 access key
	SecretKey      string        // S3 secret key
	Pool           string        // pool name; single safe path segment
	URLTTL         time.Duration // presigned URL lifetime; default 15m, max 7d
}

// Object describes one cached blob.
type Object struct {
	Digest   string
	Size     int64
	Modified time.Time
}

// Store talks to an S3-compatible blob store. Engine-facing uploads and
// downloads use presigned SigV4 query URLs; internal control operations run
// against the trusted internal endpoint.
type Store struct {
	opts      Options
	scheme    string
	host      string
	pubScheme string
	pubHost   string
	client    *http.Client
}

const (
	defaultTTL = 15 * time.Minute
	maxTTL     = 7 * 24 * time.Hour
	maxExpires = int64(604800) // S3 presigned URL maximum in seconds
)

var (
	digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	poolRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func validDigest(d string) bool { return digestRe.MatchString(d) }

func validPool(p string) bool { return poolRe.MatchString(p) }

// New validates the configuration and returns a Store.
func New(opts Options) (*Store, error) {
	scheme, host, err := parseEndpoint(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("storage: endpoint: %w", err)
	}
	pubScheme, pubHost := scheme, host
	if opts.PublicEndpoint != "" {
		pubScheme, pubHost, err = parseEndpoint(opts.PublicEndpoint)
		if err != nil {
			return nil, fmt.Errorf("storage: public endpoint: %w", err)
		}
	}
	if opts.Bucket == "" {
		return nil, errors.New("storage: bucket required")
	}
	if opts.AccessKey == "" || opts.SecretKey == "" {
		return nil, errors.New("storage: access key and secret key required")
	}
	if !validPool(opts.Pool) {
		return nil, errors.New("storage: invalid pool")
	}
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}
	ttl := opts.URLTTL
	if ttl == 0 {
		ttl = defaultTTL
	}
	if ttl < 0 || ttl > maxTTL {
		return nil, errors.New("storage: url ttl out of range")
	}
	opts.URLTTL = ttl
	return &Store{
		opts:      opts,
		scheme:    scheme,
		host:      host,
		pubScheme: pubScheme,
		pubHost:   pubHost,
		client:    &http.Client{},
	}, nil
}

func parseEndpoint(ep string) (scheme, host string, err error) {
	u, err := url.Parse(ep)
	if err != nil {
		return "", "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("scheme %q not supported", u.Scheme)
	}
	if u.Host == "" {
		return "", "", errors.New("host required")
	}
	if u.Path != "" && u.Path != "/" {
		return "", "", errors.New("path not allowed in endpoint")
	}
	return u.Scheme, u.Host, nil
}

func blobKey(pool, digest string) string {
	return "pools/" + pool + "/blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
}

func (s *Store) objectPath(digest string) string {
	return "/" + s.opts.Bucket + "/" + blobKey(s.opts.Pool, digest)
}

// presign builds a SigV4 presigned query URL for method on key using the
// official minio-go signer. The resulting URL requires no extra headers
// beyond Host (payload is unsigned).
func (s *Store) presign(method, key string) (string, time.Time, error) {
	expires := int64(s.opts.URLTTL / time.Second)
	if expires < 1 {
		expires = 1
	}
	if expires > maxExpires {
		expires = maxExpires
	}
	u := s.pubScheme + "://" + s.pubHost + "/" + s.opts.Bucket + "/" + key
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req = signer.PreSignV4(*req, s.opts.AccessKey, s.opts.SecretKey, "", s.opts.Region, expires)
	return req.URL.String(), time.Now().UTC().Add(s.opts.URLTTL), nil
}

// PresignPut returns a presigned URL for uploading the blob with digest.
func (s *Store) PresignPut(ctx context.Context, digest string, size int64) (string, error) {
	if !validDigest(digest) {
		return "", errors.New("storage: invalid digest")
	}
	if size < 0 {
		return "", errors.New("storage: negative size")
	}
	u, _, err := s.presign(http.MethodPut, blobKey(s.opts.Pool, digest))
	if err != nil {
		return "", err
	}
	return u, nil
}

// PresignGet returns a presigned URL for downloading the blob with digest,
// along with its expiry time.
func (s *Store) PresignGet(ctx context.Context, digest string) (string, time.Time, error) {
	if !validDigest(digest) {
		return "", time.Time{}, errors.New("storage: invalid digest")
	}
	u, exp, err := s.presign(http.MethodGet, blobKey(s.opts.Pool, digest))
	if err != nil {
		return "", time.Time{}, err
	}
	return u, exp, nil
}

// do performs an internal (trusted-network) request against the endpoint.
func (s *Store) do(ctx context.Context, method, path string, query url.Values, hdr http.Header) (*http.Response, []byte, error) {
	u := s.scheme + "://" + s.host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("X-Amz-Content-Sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	req = signer.SignV4(*req, s.opts.AccessKey, s.opts.SecretKey, "", s.opts.Region)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("storage: read response: %w", err)
	}
	return resp, body, nil
}

func errorFromBody(status int, body []byte) error {
	var x struct {
		Code string `xml:"Code"`
	}
	code := ""
	if err := xml.Unmarshal(body, &x); err == nil {
		code = x.Code
	}
	if code == "" {
		code = http.StatusText(status)
	}
	return fmt.Errorf("storage: s3 error %s (status %d)", code, status)
}

// EnsureBucket creates the bucket if missing, tolerating exists races without
// swallowing access denied.
func (s *Store) EnsureBucket(ctx context.Context) error {
	resp, body, err := s.do(ctx, http.MethodPut, "/"+s.opts.Bucket, nil, nil)
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		return nil
	case resp.StatusCode == http.StatusConflict:
		var x struct {
			Code string `xml:"Code"`
		}
		if xml.Unmarshal(body, &x) == nil && x.Code == "BucketAlreadyOwnedByYou" {
			return nil
		}
		return errorFromBody(resp.StatusCode, body)
	default:
		return errorFromBody(resp.StatusCode, body)
	}
}

// Exists reports whether the blob is present with exactly the declared size.
// It returns false only for a positive not-found; other storage errors are
// propagated, and a wrong size is an error.
func (s *Store) Exists(ctx context.Context, digest string, size int64) (bool, error) {
	if !validDigest(digest) {
		return false, errors.New("storage: invalid digest")
	}
	if size < 0 {
		return false, errors.New("storage: negative size")
	}
	resp, body, err := s.do(ctx, http.MethodHead, s.objectPath(digest), nil, nil)
	if err != nil {
		return false, err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		if resp.ContentLength >= 0 && resp.ContentLength != size {
			return false, fmt.Errorf("storage: size mismatch for %s: have %d want %d", digest, resp.ContentLength, size)
		}
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, errorFromBody(resp.StatusCode, body)
	}
}

// Verify streams the blob bounded by declared size +1 and checks the exact
// SHA256 digest and size. Context cancellation propagates.
func (s *Store) Verify(ctx context.Context, digest string, size int64) error {
	if !validDigest(digest) {
		return errors.New("storage: invalid digest")
	}
	if size < 0 {
		return errors.New("storage: negative size")
	}
	u := s.scheme + "://" + s.host + s.objectPath(digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-"+strconv.FormatInt(size, 10))
	req.Header.Set("X-Amz-Content-Sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	req = signer.SignV4(*req, s.opts.AccessKey, s.opts.SecretKey, "", s.opts.Region)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("storage: %s not found", digest)
	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return errorFromBody(resp.StatusCode, body)
	}
	return s.VerifyStream(ctx, digest, size, resp.Body)
}

// VerifyStream verifies a live response stream bounded by size+1. It is used
// internally by Verify when a streaming body is available.
func (s *Store) VerifyStream(ctx context.Context, digest string, size int64, r io.Reader) error {
	if !validDigest(digest) {
		return errors.New("storage: invalid digest")
	}
	if size < 0 {
		return errors.New("storage: negative size")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, size+1))
	if err != nil {
		return fmt.Errorf("storage: read %s: %w", digest, err)
	}
	if n != size {
		return fmt.Errorf("storage: size mismatch for %s: have %d want %d", digest, n, size)
	}
	if hex.EncodeToString(h.Sum(nil)) != strings.TrimPrefix(digest, "sha256:") {
		return fmt.Errorf("storage: hash mismatch for %s", digest)
	}
	return nil
}

type listBucketResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

// digestFromKey extracts a valid digest from a blob key under the store's pool.
func (s *Store) digestFromKey(key string) (string, bool) {
	prefix := "pools/" + s.opts.Pool + "/blobs/sha256/"
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok || !validDigest("sha256:"+rest) {
		return "", false
	}
	return "sha256:" + rest, true
}

// List returns objects under the store's pool with valid digests only.
func (s *Store) List(ctx context.Context) ([]Object, error) {
	prefix := "pools/" + s.opts.Pool + "/blobs/sha256/"
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, body, err := s.do(ctx, http.MethodGet, "/"+s.opts.Bucket, q, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, errorFromBody(resp.StatusCode, body)
		}
		var res listBucketResult
		if err := xml.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("storage: parse list: %w", err)
		}
		for _, c := range res.Contents {
			d, ok := s.digestFromKey(c.Key)
			if !ok {
				continue
			}
			out = append(out, Object{Digest: d, Size: c.Size, Modified: c.LastModified})
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return out, nil
		}
		token = res.NextContinuationToken
	}
}

// Delete removes the blob with digest. Deleting a missing object is a no-op.
func (s *Store) Delete(ctx context.Context, digest string) error {
	if !validDigest(digest) {
		return errors.New("storage: invalid digest")
	}
	resp, body, err := s.do(ctx, http.MethodDelete, s.objectPath(digest), nil, nil)
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent ||
		resp.StatusCode == http.StatusNotFound:
		return nil
	default:
		return errorFromBody(resp.StatusCode, body)
	}
}

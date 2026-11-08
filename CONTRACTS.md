# Module contracts

Module: github.com/snoby/dcache. Read actual .upstream/dagger source; documentation is not a replacement for API signatures. Tests run on isolated ethpow dev workspace through scripts/test-remote.sh.

## internal/storage

Implement using github.com/minio/minio-go/v7 (S3 API) and standard crypto; no dagql dependency.

```
type Options struct {
 Endpoint string // internal http(s) URL including port
 PublicEndpoint string // optional engine-visible http(s) URL; separate presigner if set
 Region, Bucket, AccessKey, SecretKey, Pool string
 URLTTL time.Duration
}
type Store struct { ... }
func New(Options) (*Store,error)
func (*Store) EnsureBucket(context.Context) error
func (*Store) PresignPut(ctx context.Context, digest string, size int64) (string,error)
func (*Store) PresignGet(ctx context.Context, digest string) (string,time.Time,error)
func (*Store) Exists(ctx context.Context, digest string, size int64) (bool,error)
func (*Store) Verify(ctx context.Context, digest string, size int64) error
func (*Store) List(ctx context.Context) ([]Object,error)
func (*Store) Delete(ctx context.Context,digest string) error
type Object struct { Digest string; Size int64; Modified time.Time }
```
Only sha256:<64 lowercase hex> valid. Pool is single safe path segment (no slash, dot/dotdot); keys pools/<pool>/blobs/sha256/<hex>. Reject invalid config, digest and negative size before I/O. Presigned requests require no custom engine headers. Verification streams bounded by declared size +1 and checks exact SHA256 and size; context cancellation propagates. Exists returns false only for positive not-found, propagates other storage errors; wrong size is an error. No signed URLs/secrets in errors. EnsureBucket handles exists races without swallowing access denied. List only own pool and only valid digest objects. New validates URL scheme and nonempty creds/bucket, TTL default 15m and bounded <=7d. Functional MinIO server started later; use httptest S3 API for RED/GREEN unit test, real S3 later.

## internal/renewal

Standard library only.

```
type Claims struct { Pool string `json:"pool"`; Donor uint64 `json:"donor"`; Digests []string `json:"digests"`; Expires int64 `json:"expires"` }
type Signer struct {...}
func New(secret []byte,pool string,ttl time.Duration) (*Signer,error)
func (*Signer) Mint(donor uint64,digests []string) (string,error)
func (*Signer) Verify(token string) (Claims,error)
```
HMAC-SHA256 of base64url JSON payload; token payload.signature. At least 32 secret bytes. Positive TTL, pool segment validation, positive donor, ordered sha256 digests (empty valid for scratch chain if donor positive). Deep copy claims inputs. Max token length bounded (64KiB). Verify pool, expiration and exact signature, reject malformed/extra payload segments/unknown fields/trailing JSON and invalid digests. No magic cache lookup/durable store. Unit tests tamper payload/signature, wrong key/pool, expired token, malformed base64, uint64>2^53 donor preserved, layer order retained. No secrets in error.

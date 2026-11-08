package renewal

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// maxTokenLen bounds the accepted token size (64 KiB).
const maxTokenLen = 64 * 1024

var (
	// ErrInvalidToken reports a malformed token or invalid claims.
	ErrInvalidToken = errors.New("renewal: invalid token")
	// ErrExpired reports a token whose expiration has passed.
	ErrExpired = errors.New("renewal: token expired")
	// ErrPoolMismatch reports a token minted for a different pool.
	ErrPoolMismatch = errors.New("renewal: pool mismatch")
	// ErrSignature reports a token whose signature does not match.
	ErrSignature = errors.New("renewal: signature mismatch")
)

// Claims is the signed payload carried by a renewal token.
type Claims struct {
	Pool    string   `json:"pool"`
	Donor   uint64   `json:"donor"`
	Digests []string `json:"digests"`
	Expires int64    `json:"expires"`
}

// Signer mints and verifies HMAC-SHA256 renewal tokens.
type Signer struct {
	secret []byte
	pool   string
	ttl    time.Duration
}

// New validates the configuration and returns a Signer. The secret must be at
// least 32 bytes, the TTL positive, and the pool a single safe path segment.
func New(secret []byte, pool string, ttl time.Duration) (*Signer, error) {
	if len(secret) < 32 {
		return nil, errors.New("renewal: secret must be at least 32 bytes")
	}
	if ttl <= 0 {
		return nil, errors.New("renewal: ttl must be positive")
	}
	if !validPool(pool) {
		return nil, errors.New("renewal: invalid pool")
	}
	return &Signer{
		secret: append([]byte(nil), secret...),
		pool:   pool,
		ttl:    ttl,
	}, nil
}

// Mint returns a signed token for donor and digests. The digest order is
// preserved and the input slice is copied.
func (s *Signer) Mint(donor uint64, digests []string) (string, error) {
	if donor == 0 {
		return "", errors.New("renewal: donor must be positive")
	}
	for _, d := range digests {
		if !validDigest(d) {
			return "", errors.New("renewal: invalid digest")
		}
	}
	claims := Claims{
		Pool:    s.pool,
		Donor:   donor,
		Digests: append([]string(nil), digests...),
		Expires: time.Now().Add(s.ttl).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("renewal: marshal claims: %w", err)
	}
	return s.sign(payload), nil
}

// sign encodes payload and appends the HMAC-SHA256 signature over the encoded
// payload segment.
func (s *Signer) sign(payload []byte) string {
	enc := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(enc))
	return enc + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify checks the signature, pool, expiration and claims of token.
func (s *Signer) Verify(token string) (Claims, error) {
	var claims Claims
	if token == "" || len(token) > maxTokenLen {
		return claims, ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return claims, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return claims, ErrSignature
	}

	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&claims); err != nil {
		return Claims{}, ErrInvalidToken
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Claims{}, ErrInvalidToken
	}

	if claims.Pool != s.pool {
		return Claims{}, ErrPoolMismatch
	}
	if claims.Donor == 0 {
		return Claims{}, ErrInvalidToken
	}
	for _, d := range claims.Digests {
		if !validDigest(d) {
			return Claims{}, ErrInvalidToken
		}
	}
	if claims.Expires <= 0 {
		return Claims{}, ErrInvalidToken
	}
	if time.Now().Unix() >= claims.Expires {
		return Claims{}, ErrExpired
	}
	return claims, nil
}

// validPool reports whether p is a single safe path segment.
func validPool(p string) bool {
	if p == "" || p == "." || p == ".." {
		return false
	}
	for _, r := range p {
		if r == '/' || r == '\\' || r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validDigest reports whether d is sha256:<64 lowercase hex>.
func validDigest(d string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(d, prefix) {
		return false
	}
	hex := d[len(prefix):]
	if len(hex) != 64 {
		return false
	}
	for _, c := range hex {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

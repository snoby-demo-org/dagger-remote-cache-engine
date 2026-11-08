package renewal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

const (
	testSecret = "0123456789abcdef0123456789abcdef"
	testPool   = "pool-a"
	testTTL    = time.Minute
)

func testSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := New([]byte(testSecret), testPool, testTTL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// makeToken builds a token independently of the implementation so fixtures can
// carry arbitrary (including expired or malformed) claims.
func makeToken(t *testing.T, secret []byte, claims Claims) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return makeRawToken(t, secret, payload)
}

func makeRawToken(t *testing.T, secret, payload []byte) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(enc))
	return enc + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func digest(seed byte) string {
	return fmt.Sprintf("sha256:%064x", seed)
}

func TestNewValidation(t *testing.T) {
	valid := []byte(testSecret)
	if _, err := New(valid, testPool, testTTL); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	short := make([]byte, 31)
	if _, err := New(short, testPool, testTTL); err == nil {
		t.Fatal("short secret accepted")
	}
	if _, err := New(nil, testPool, testTTL); err == nil {
		t.Fatal("nil secret accepted")
	}
	if _, err := New(valid, testPool, 0); err == nil {
		t.Fatal("zero ttl accepted")
	}
	if _, err := New(valid, testPool, -time.Second); err == nil {
		t.Fatal("negative ttl accepted")
	}

	for _, pool := range []string{"", ".", "..", "a/b", "a\\b", "a b", "a\tb", "a\nb"} {
		if _, err := New(valid, pool, testTTL); err == nil {
			t.Fatalf("invalid pool %q accepted", pool)
		}
	}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	s := testSigner(t)
	digests := []string{digest(1), digest(2)}
	token, err := s.Mint(42, digests)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if strings.Count(token, ".") != 1 {
		t.Fatalf("token must have exactly one separator: %q", token)
	}
	claims, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Pool != testPool {
		t.Fatalf("pool = %q, want %q", claims.Pool, testPool)
	}
	if claims.Donor != 42 {
		t.Fatalf("donor = %d, want 42", claims.Donor)
	}
	if len(claims.Digests) != 2 || claims.Digests[0] != digests[0] || claims.Digests[1] != digests[1] {
		t.Fatalf("digests = %v, want %v", claims.Digests, digests)
	}
	now := time.Now().Unix()
	if claims.Expires <= now {
		t.Fatalf("expires = %d, want > %d", claims.Expires, now)
	}
	if claims.Expires > now+int64(testTTL/time.Second)+1 {
		t.Fatalf("expires = %d too far in future", claims.Expires)
	}
}

func TestUint64Precision(t *testing.T) {
	s := testSigner(t)
	for _, donor := range []uint64{1<<53 + 1, 1 << 60, math.MaxUint64} {
		token, err := s.Mint(donor, nil)
		if err != nil {
			t.Fatalf("Mint(%d): %v", donor, err)
		}
		claims, err := s.Verify(token)
		if err != nil {
			t.Fatalf("Verify(%d): %v", donor, err)
		}
		if claims.Donor != donor {
			t.Fatalf("donor = %d, want %d", claims.Donor, donor)
		}
	}
}

func TestDigestOrderRetained(t *testing.T) {
	s := testSigner(t)
	digests := []string{digest(3), digest(1), digest(2)}
	token, err := s.Mint(7, digests)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for i := range digests {
		if claims.Digests[i] != digests[i] {
			t.Fatalf("digest %d = %q, want %q", i, claims.Digests[i], digests[i])
		}
	}
}

func TestEmptyDigestsAllowed(t *testing.T) {
	s := testSigner(t)
	for _, digests := range [][]string{nil, {}} {
		token, err := s.Mint(1, digests)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		claims, err := s.Verify(token)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if len(claims.Digests) != 0 {
			t.Fatalf("digests = %v, want empty", claims.Digests)
		}
	}
}

func TestMintRejectsInvalidInput(t *testing.T) {
	s := testSigner(t)
	if _, err := s.Mint(0, nil); err == nil {
		t.Fatal("zero donor accepted")
	}
	bad := []string{
		"",
		"sha256:",
		"sha256:short",
		"md5:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("A", 64),
		"sha256:" + strings.Repeat("g", 64),
		"sha256:" + strings.Repeat("a", 63),
		"sha256:" + strings.Repeat("a", 65),
	}
	for _, d := range bad {
		if _, err := s.Mint(1, []string{d}); err == nil {
			t.Fatalf("invalid digest %q accepted", d)
		}
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	s := testSigner(t)
	token, err := s.Mint(42, []string{digest(1)})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	raw["donor"] = float64(43)
	changed, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	tampered := base64.RawURLEncoding.EncodeToString(changed) + "." + parts[1]
	if _, err := s.Verify(tampered); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered payload err = %v, want ErrSignature", err)
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	s := testSigner(t)
	token, err := s.Mint(42, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	parts := strings.Split(token, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	sig[0] ^= 0xff
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(sig)
	if _, err := s.Verify(tampered); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered signature err = %v, want ErrSignature", err)
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	s := testSigner(t)
	other, err := New([]byte("fedcba9876543210fedcba9876543210"), testPool, testTTL)
	if err != nil {
		t.Fatalf("New other: %v", err)
	}
	token, err := s.Mint(1, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := other.Verify(token); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong key err = %v, want ErrSignature", err)
	}
}

func TestVerifyRejectsWrongPool(t *testing.T) {
	s := testSigner(t)
	other, err := New([]byte(testSecret), "pool-b", testTTL)
	if err != nil {
		t.Fatalf("New other: %v", err)
	}
	token, err := s.Mint(1, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := other.Verify(token); !errors.Is(err, ErrPoolMismatch) {
		t.Fatalf("wrong pool err = %v, want ErrPoolMismatch", err)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	s := testSigner(t)
	token := makeToken(t, []byte(testSecret), Claims{
		Pool:    testPool,
		Donor:   1,
		Digests: []string{digest(1)},
		Expires: time.Now().Add(-time.Minute).Unix(),
	})
	if _, err := s.Verify(token); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired err = %v, want ErrExpired", err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	s := testSigner(t)
	valid, err := s.Mint(1, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	cases := []string{
		"",
		".",
		"abc",
		"abc.",
		".abc",
		"abc.def",
		"abc.def.ghi",
		"!!!.!!!",
		"abc.!!!",
		"a=b.c=d",
		valid + ".extra",
		strings.Repeat("A", 100),
	}
	for _, token := range cases {
		if _, err := s.Verify(token); err == nil {
			t.Fatalf("malformed token %q accepted", token)
		}
	}
}

func TestVerifyRejectsUnknownFields(t *testing.T) {
	s := testSigner(t)
	payload := fmt.Sprintf(`{"pool":%q,"donor":1,"digests":[],"expires":%d,"extra":true}`,
		testPool, time.Now().Add(time.Minute).Unix())
	token := makeRawToken(t, []byte(testSecret), []byte(payload))
	if _, err := s.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unknown field err = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsTrailingJSON(t *testing.T) {
	s := testSigner(t)
	payload := fmt.Sprintf(`{"pool":%q,"donor":1,"digests":[],"expires":%d}{"x":1}`,
		testPool, time.Now().Add(time.Minute).Unix())
	token := makeRawToken(t, []byte(testSecret), []byte(payload))
	if _, err := s.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("trailing json err = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsInvalidClaims(t *testing.T) {
	s := testSigner(t)
	future := time.Now().Add(time.Minute).Unix()
	cases := []struct {
		name   string
		claims Claims
		want   error
	}{
		{"zero donor", Claims{Pool: testPool, Donor: 0, Expires: future}, ErrInvalidToken},
		{"bad digest", Claims{Pool: testPool, Donor: 1, Digests: []string{"sha256:zz"}, Expires: future}, ErrInvalidToken},
		{"zero expires", Claims{Pool: testPool, Donor: 1, Expires: 0}, ErrInvalidToken},
		{"negative expires", Claims{Pool: testPool, Donor: 1, Expires: -1}, ErrInvalidToken},
		{"wrong pool", Claims{Pool: "pool-b", Donor: 1, Expires: future}, ErrPoolMismatch},
		{"expired", Claims{Pool: testPool, Donor: 1, Expires: time.Now().Add(-time.Second).Unix()}, ErrExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := makeToken(t, []byte(testSecret), tc.claims)
			if _, err := s.Verify(token); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyRejectsOversizedToken(t *testing.T) {
	s := testSigner(t)
	token := strings.Repeat("a", 64*1024+1)
	if _, err := s.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("oversized err = %v, want ErrInvalidToken", err)
	}
}

func TestDeepCopy(t *testing.T) {
	s := testSigner(t)
	digests := []string{digest(1), digest(2)}
	token, err := s.Mint(1, digests)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	digests[0] = digest(9)
	claims, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Digests[0] != digest(1) {
		t.Fatalf("input mutation leaked into token: %q", claims.Digests[0])
	}
	claims.Digests[0] = digest(8)
	again, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify again: %v", err)
	}
	if again.Digests[0] != digest(1) {
		t.Fatalf("returned claims mutation leaked: %q", again.Digests[0])
	}
}

func TestNoSecretsInErrors(t *testing.T) {
	s := testSigner(t)
	secret := []byte(testSecret)
	other, err := New([]byte("fedcba9876543210fedcba9876543210"), testPool, testTTL)
	if err != nil {
		t.Fatalf("New other: %v", err)
	}
	token, err := s.Mint(1, nil)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	expired := makeToken(t, secret, Claims{Pool: testPool, Donor: 1, Expires: time.Now().Add(-time.Minute).Unix()})
	cases := []string{
		token,
		expired,
		"!!!.!!!",
		strings.Repeat("a", 64*1024+1),
	}
	for _, tok := range cases {
		if _, err := other.Verify(tok); err != nil && strings.Contains(err.Error(), testSecret) {
			t.Fatalf("error leaks secret: %v", err)
		}
	}
}

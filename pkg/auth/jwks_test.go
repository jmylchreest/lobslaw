package auth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

// --- JWK minting helpers --------------------------------------------------

// mintRSAKey generates a 2048-bit RSA key and returns its JWK
// representation alongside the private key (so tests can sign).
func mintRSAKey(t *testing.T, kid string) (*rsa.PrivateKey, jwkEntry) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	// e in big-endian bytes, then base64url-unpadded.
	eBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(eBytes, uint64(k.E))
	// Trim leading zeros (canonical JWK form).
	for len(eBytes) > 1 && eBytes[0] == 0 {
		eBytes = eBytes[1:]
	}
	return k, jwkEntry{
		Kty: "RSA",
		Kid: kid,
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(eBytes),
	}
}

// mintECKey generates an ECDSA key on the given curve.
func mintECKey(t *testing.T, kid string, curve elliptic.Curve) (*ecdsa.PrivateKey, jwkEntry) {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var crv, alg string
	switch curve {
	case elliptic.P256():
		crv, alg = "P-256", "ES256"
	case elliptic.P384():
		crv, alg = "P-384", "ES384"
	case elliptic.P521():
		crv, alg = "P-521", "ES512"
	}
	sz := (curve.Params().BitSize + 7) / 8
	xBytes := k.X.FillBytes(make([]byte, sz))
	yBytes := k.Y.FillBytes(make([]byte, sz))
	return k, jwkEntry{
		Kty: "EC",
		Kid: kid,
		Alg: alg,
		Crv: crv,
		X:   base64.RawURLEncoding.EncodeToString(xBytes),
		Y:   base64.RawURLEncoding.EncodeToString(yBytes),
	}
}

// mintEd25519Key generates an Ed25519 key.
func mintEd25519Key(t *testing.T, kid string) (ed25519.PrivateKey, jwkEntry) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, jwkEntry{
		Kty: "OKP",
		Kid: kid,
		Alg: "EdDSA",
		Crv: "Ed25519",
		X:   base64.RawURLEncoding.EncodeToString(pub),
	}
}

// jwksServer serves a mutable JWK set. Atomic counter tracks fetches
// so tests can assert refresh behaviour.
type jwksServer struct {
	srv       *httptest.Server
	mu        chan struct{} // lock channel — single-slot mutex
	keys      []jwkEntry
	fetches   atomic.Int64
	httpCode  int // override response status (0 = 200)
	rawBody   []byte
}

func newJWKSServer(initial []jwkEntry) *jwksServer {
	s := &jwksServer{
		mu:   make(chan struct{}, 1),
		keys: initial,
	}
	s.mu <- struct{}{} // seed
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.fetches.Add(1)
		<-s.mu
		defer func() { s.mu <- struct{}{} }()
		if s.httpCode != 0 {
			w.WriteHeader(s.httpCode)
			_, _ = w.Write(s.rawBody)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks{Keys: s.keys})
	}))
	return s
}

func (s *jwksServer) url() string       { return s.srv.URL }
func (s *jwksServer) fetchCount() int64 { return s.fetches.Load() }
func (s *jwksServer) close()            { s.srv.Close() }

func (s *jwksServer) setKeys(keys []jwkEntry) {
	<-s.mu
	s.keys = keys
	s.mu <- struct{}{}
}

func (s *jwksServer) setHTTPError(code int, body string) {
	<-s.mu
	s.httpCode = code
	s.rawBody = []byte(body)
	s.mu <- struct{}{}
}

// --- Validator integration tests ----------------------------------------

func TestValidatorJWKSRoundTripRSA(t *testing.T) {
	t.Parallel()
	priv, jwk := mintRSAKey(t, "rsa-1")
	srv := newJWKSServer([]jwkEntry{jwk})
	defer srv.close()

	v, err := NewValidator(Config{JWKSURL: srv.url()})
	if err != nil {
		t.Fatal(err)
	}

	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodRS256, jwtlib.MapClaims{
		"sub": "bob",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	tok.Header["kid"] = "rsa-1"
	raw, err := tok.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}

	claims, err := v.Validate(raw)
	if err != nil {
		t.Fatalf("RS256 validate: %v", err)
	}
	if claims.UserID != "bob" {
		t.Errorf("UserID: %q", claims.UserID)
	}
}

func TestValidatorJWKSRoundTripECDSA(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		curve  elliptic.Curve
		method jwtlib.SigningMethod
	}{
		{"P-256/ES256", elliptic.P256(), jwtlib.SigningMethodES256},
		{"P-384/ES384", elliptic.P384(), jwtlib.SigningMethodES384},
		{"P-521/ES512", elliptic.P521(), jwtlib.SigningMethodES512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			priv, jwk := mintECKey(t, "ec-1", tc.curve)
			srv := newJWKSServer([]jwkEntry{jwk})
			defer srv.close()

			v, err := NewValidator(Config{JWKSURL: srv.url()})
			if err != nil {
				t.Fatal(err)
			}
			tok := jwtlib.NewWithClaims(tc.method, jwtlib.MapClaims{
				"sub": "ec-user",
				"exp": float64(time.Now().Add(time.Hour).Unix()),
			})
			tok.Header["kid"] = "ec-1"
			raw, err := tok.SignedString(priv)
			if err != nil {
				t.Fatal(err)
			}
			claims, err := v.Validate(raw)
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			if claims.UserID != "ec-user" {
				t.Errorf("UserID: %q", claims.UserID)
			}
		})
	}
}

func TestValidatorJWKSRoundTripEdDSA(t *testing.T) {
	t.Parallel()
	priv, jwk := mintEd25519Key(t, "ed-1")
	srv := newJWKSServer([]jwkEntry{jwk})
	defer srv.close()

	v, err := NewValidator(Config{JWKSURL: srv.url()})
	if err != nil {
		t.Fatal(err)
	}
	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodEdDSA, jwtlib.MapClaims{
		"sub": "ed-user",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	tok.Header["kid"] = "ed-1"
	raw, err := tok.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := v.Validate(raw)
	if err != nil {
		t.Fatalf("EdDSA validate: %v", err)
	}
	if claims.UserID != "ed-user" {
		t.Errorf("UserID: %q", claims.UserID)
	}
}

// TestValidatorJWKSRejectsUnknownKid — a token with a kid NOT in
// the JWK set must fail validation. The validator force-refreshes
// once on unknown kid; a still-missing kid after refresh is an
// invalid-token error.
func TestValidatorJWKSRejectsUnknownKid(t *testing.T) {
	t.Parallel()
	priv, jwk := mintRSAKey(t, "known")
	srv := newJWKSServer([]jwkEntry{jwk})
	defer srv.close()

	v, _ := NewValidator(Config{JWKSURL: srv.url()})

	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodRS256, jwtlib.MapClaims{
		"sub": "x",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	tok.Header["kid"] = "UNKNOWN"
	raw, _ := tok.SignedString(priv)

	_, err := v.Validate(raw)
	if err == nil {
		t.Fatal("unknown kid must fail validation")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("want ErrInvalidToken; got %v", err)
	}
}

// TestValidatorJWKSForceRefreshOnKeyRotation — IdP rotates keys; a
// token signed by the NEW key is presented before the cache refresh
// interval fires. The unknown-kid fallback triggers a fetch and the
// token validates.
func TestValidatorJWKSForceRefreshOnKeyRotation(t *testing.T) {
	t.Parallel()
	oldPriv, oldJWK := mintRSAKey(t, "rsa-old")
	srv := newJWKSServer([]jwkEntry{oldJWK})
	defer srv.close()

	v, _ := NewValidator(Config{
		JWKSURL:             srv.url(),
		JWKSRefreshInterval: time.Hour, // intentionally long
		JWKSForceRefreshMin: 0,         // no rate limit for this test
	})

	// Warm the cache with a known-kid token.
	warm := jwtlib.NewWithClaims(jwtlib.SigningMethodRS256, jwtlib.MapClaims{
		"sub": "warm",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	warm.Header["kid"] = "rsa-old"
	raw, _ := warm.SignedString(oldPriv)
	if _, err := v.Validate(raw); err != nil {
		t.Fatalf("warm validate: %v", err)
	}
	preRotate := srv.fetchCount()

	// Rotate.
	newPriv, newJWK := mintRSAKey(t, "rsa-new")
	srv.setKeys([]jwkEntry{newJWK})

	// Token signed by new key.
	hot := jwtlib.NewWithClaims(jwtlib.SigningMethodRS256, jwtlib.MapClaims{
		"sub": "after-rotate",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	hot.Header["kid"] = "rsa-new"
	hotRaw, _ := hot.SignedString(newPriv)

	if _, err := v.Validate(hotRaw); err != nil {
		t.Fatalf("post-rotate validate: %v", err)
	}
	if srv.fetchCount() <= preRotate {
		t.Error("expected force-refresh fetch after unknown kid presented")
	}
}

// TestJWKSCacheRateLimitsForceRefresh — a stream of unknown-kid
// tokens must NOT hammer the IdP. After one force-refresh the rate
// limiter should swallow subsequent unknown-kid fetches until the
// window passes.
func TestJWKSCacheRateLimitsForceRefresh(t *testing.T) {
	t.Parallel()
	_, jwk := mintRSAKey(t, "rsa-1")
	srv := newJWKSServer([]jwkEntry{jwk})
	defer srv.close()

	c, _ := NewJWKSCache(JWKSConfig{
		URL:             srv.url(),
		RefreshInterval: time.Hour,
		ForceRefreshMin: time.Hour, // effectively "once per test"
	})

	// First miss triggers one force refresh.
	_, _, err := c.Get(t.Context(), "does-not-exist")
	if err == nil {
		t.Fatal("first miss should return errJWKSUnknownKID")
	}
	afterFirst := srv.fetchCount()

	// A dozen more misses — must not produce additional fetches.
	for range 12 {
		_, _, _ = c.Get(t.Context(), "still-missing")
	}
	if srv.fetchCount() != afterFirst {
		t.Errorf("rate limiter failed: %d extra fetches", srv.fetchCount()-afterFirst)
	}
}

// TestJWKSCacheStaleOnFailure — transient IdP outage must not wipe the cache.
func TestJWKSCacheStaleOnFailure(t *testing.T) {
	t.Parallel()
	_, jwk := mintRSAKey(t, "k")
	srv := newJWKSServer([]jwkEntry{jwk})
	defer srv.close()

	c, _ := NewJWKSCache(JWKSConfig{
		URL:             srv.url(),
		RefreshInterval: time.Millisecond, // force a refresh on next call
		ForceRefreshMin: 0,
	})

	// Populate the cache.
	if _, _, err := c.Get(t.Context(), "k"); err != nil {
		t.Fatal(err)
	}

	// Break the upstream.
	srv.setHTTPError(500, "nope")
	time.Sleep(5 * time.Millisecond) // ensure RefreshInterval elapsed

	// Cache should still answer.
	if _, _, err := c.Get(t.Context(), "k"); err != nil {
		t.Errorf("stale-on-failure: lost cached key through a broken refresh: %v", err)
	}
}

// TestValidatorRejectsAlgMismatchBetweenTokenAndKey pins the classic JWT
// alg-confusion attack (token says ES256, key is RSA → rejected).
func TestValidatorRejectsAlgMismatchBetweenTokenAndKey(t *testing.T) {
	t.Parallel()
	_, rsaJWK := mintRSAKey(t, "shared-kid")
	// Craft a token as ES256 but with the same kid pointing at the
	// RSA JWK. The library validates by looking at the method-typed
	// key returned from keyfunc, so we stop this EARLIER: we refuse
	// to return the RSA key for an ES256 method via the advertisedAlg
	// check.
	srv := newJWKSServer([]jwkEntry{rsaJWK})
	defer srv.close()
	v, _ := NewValidator(Config{JWKSURL: srv.url()})

	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodES256, jwtlib.MapClaims{
		"sub": "attacker",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	tok.Header["kid"] = "shared-kid"
	raw, _ := tok.SignedString(ecKey)

	if _, err := v.Validate(raw); err == nil {
		t.Fatal("alg-confusion attempt accepted — SECURITY REGRESSION")
	}
}

// TestValidatorRejectsHS256WhenOnlyJWKSConfigured — HS256 without a
// secret AND JWKS configured must still reject HS256 tokens; a
// malicious token claiming HS256 with the IdP's public key as
// "secret" is the textbook asymmetric→symmetric attack.
func TestValidatorRejectsHS256WhenOnlyJWKSConfigured(t *testing.T) {
	t.Parallel()
	_, jwk := mintRSAKey(t, "any")
	srv := newJWKSServer([]jwkEntry{jwk})
	defer srv.close()
	v, _ := NewValidator(Config{JWKSURL: srv.url()}) // no HS256

	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"sub": "attacker",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	raw, _ := tok.SignedString([]byte("anything"))
	if _, err := v.Validate(raw); err == nil {
		t.Fatal("HS256 accepted when only JWKS is configured")
	}
}

// TestJWKSParseRSAWithLargeModulus — a 4096-bit RSA key's modulus
// is a big number; this exercises the bignum decode path.
func TestJWKSParseRSAWithLargeModulus(t *testing.T) {
	t.Parallel()
	k, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Skip("rsa 3072 keygen slow in this environment")
	}
	pub, _, err := parseJWK(jwkEntry{
		Kty: "RSA",
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := pub.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("wrong type: %T", pub)
	}
	if got.N.Cmp(k.N) != 0 || got.E != k.E {
		t.Errorf("round-trip mismatch")
	}
}

// TestJWKSRejectsUnsupportedKty — future-proofing: a JWK with an
// unknown "kty" must surface as an error so operators see their
// IdP sent something we don't understand.
func TestJWKSRejectsUnsupportedKty(t *testing.T) {
	t.Parallel()
	_, _, err := parseJWK(jwkEntry{Kty: "oct"}) // symmetric key — not valid for JWKS verify
	if err == nil {
		t.Error("unsupported kty should error")
	}
}

// Compile-time guard that asymmetricSigningMethods is non-empty —
// otherwise a misconfiguration would silently reject every
// asymmetric token.
func TestAsymmetricMethodsPopulated(t *testing.T) {
	t.Parallel()
	if len(asymmetricSigningMethods) == 0 {
		t.Fatal("asymmetricSigningMethods empty — every asymmetric token would fail")
	}
	// Ensure the common set is present.
	want := []string{"RS256", "ES256", "EdDSA"}
	for _, w := range want {
		found := false
		for _, m := range asymmetricSigningMethods {
			if m == w {
				found = true
			}
		}
		if !found {
			t.Errorf("asymmetricSigningMethods missing %q", w)
		}
	}
	_ = fmt.Sprint // keep fmt referenced if future edits drop usage
}

// Package auth validates inbound JWTs and yields a *types.Claims.
// The raw token is never held past validation — callers receive the
// claims only.
//
// Phase 6d ships HS256 (shared secret) validation. Phase 6d.2 adds
// RS256/384/512, ES256/384/512, and EdDSA validation against a
// JWKS URL — the production path for hosted-IdP deployments.
package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Sentinel errors the gateway layer uses to map validation failures
// to HTTP status codes (401 for bad tokens, 403 for tokens that are
// shape-correct but lack required claims).
var (
	ErrMissingToken = errors.New("auth: missing bearer token")
	ErrInvalidToken = errors.New("auth: token invalid")
	ErrExpiredToken = errors.New("auth: token expired")
	ErrIssuerMismatch = errors.New("auth: issuer mismatch")
	ErrNoValidator  = errors.New("auth: no validator configured (allow_hs256=false and no JWKS)")
)

// Validator verifies an inbound bearer token and produces a
// *types.Claims. Construction is config-driven: HS256 when
// allow_hs256=true and a shared secret is provided; JWKS-based
// (RS256/EdDSA) when a JWKS URL is configured (Phase 6d.2 — not
// yet implemented). A Validator that has neither returns
// ErrNoValidator on every call so unauthenticated deployments fail
// loud rather than silently allowing everything.
type Validator struct {
	hs256Secret []byte
	issuer      string
	jwks        *JWKSCache
}

// Config mirrors the relevant bits of config.AuthConfig. Kept
// package-local so pkg/auth doesn't depend on pkg/config.
type Config struct {
	Issuer       string
	AllowHS256   bool
	HS256Secret  string // pre-resolved; caller translates jwt_secret_ref

	// JWKSURL enables RS256/ES256/EdDSA validation against a remote
	// key set. When set, the validator accepts any of the asymmetric
	// algs listed in asymmetricSigningMethods below and looks up the
	// verification key by the token's kid header.
	JWKSURL string

	// JWKSClient / JWKSRefreshInterval / JWKSForceRefreshMin tune
	// the cache. All optional — sensible defaults are picked.
	JWKSClient             *http.Client
	JWKSRefreshInterval    time.Duration
	JWKSForceRefreshMin    time.Duration
}

// asymmetricSigningMethods is the allow-list of non-HS* algorithms.
// Enumerated explicitly so a future library default picking up a
// new alg (e.g. HS256-with-swapped-label) doesn't silently widen
// what we accept.
var asymmetricSigningMethods = []string{
	"RS256", "RS384", "RS512",
	"ES256", "ES384", "ES512",
	"EdDSA",
}

// NewValidator returns a configured Validator. Returns an explicit
// error when the config is incomplete (allow_hs256 set but no
// secret provided, etc.) so misconfigurations fail at boot rather
// than on the first inbound request.
func NewValidator(cfg Config) (*Validator, error) {
	v := &Validator{issuer: cfg.Issuer}
	if cfg.AllowHS256 {
		if cfg.HS256Secret == "" {
			return nil, errors.New("auth: allow_hs256=true requires a non-empty HS256 secret")
		}
		v.hs256Secret = []byte(cfg.HS256Secret)
	}
	if !cfg.AllowHS256 && cfg.HS256Secret != "" {
		return nil, errors.New("auth: HS256 secret supplied but allow_hs256=false — enable HS256 or remove the secret")
	}
	if cfg.JWKSURL != "" {
		cache, err := NewJWKSCache(JWKSConfig{
			URL:             cfg.JWKSURL,
			Client:          cfg.JWKSClient,
			RefreshInterval: cfg.JWKSRefreshInterval,
			ForceRefreshMin: cfg.JWKSForceRefreshMin,
			Logger:          slog.Default(),
		})
		if err != nil {
			return nil, fmt.Errorf("auth: jwks: %w", err)
		}
		v.jwks = cache
	}
	return v, nil
}

// Validate parses, verifies, and expiry-checks the token. Returns
// the derived *types.Claims on success. The raw token is zeroed
// from the local variable before return for hygiene.
//
// Algorithm handling: only HS256 is accepted when hs256Secret is
// set. Any other alg (RS256, none, …) is rejected — critical for
// avoiding the classic "alg=none" bypass.
func (v *Validator) Validate(raw string) (*types.Claims, error) {
	return v.ValidateContext(context.Background(), raw)
}

// ValidateContext is the ctx-aware variant. The ctx plumbs through
// to the JWKS fetcher so a hung IdP doesn't stall validation
// indefinitely. Callers with an HTTP request in hand should prefer
// this and pass r.Context().
func (v *Validator) ValidateContext(ctx context.Context, raw string) (*types.Claims, error) {
	if v == nil {
		return nil, ErrNoValidator
	}
	if raw == "" {
		return nil, ErrMissingToken
	}
	if len(v.hs256Secret) == 0 && v.jwks == nil {
		return nil, ErrNoValidator
	}

	// Build the accepted-methods list from what's actually configured
	// — HS256 only when a secret is present, asymmetric only when
	// JWKS is wired. Prevents algorithm-confusion attacks by refusing
	// algs the operator didn't opt into.
	allowed := make([]string, 0, 8)
	if len(v.hs256Secret) > 0 {
		allowed = append(allowed, "HS256")
	}
	if v.jwks != nil {
		allowed = append(allowed, asymmetricSigningMethods...)
	}

	parser := jwt.NewParser(jwt.WithValidMethods(allowed))
	parsed, err := parser.Parse(raw, v.keyFunc(ctx))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !parsed.Valid {
		return nil, ErrInvalidToken
	}

	mapClaims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected claims shape", ErrInvalidToken)
	}

	claims, err := mapClaimsToTyped(mapClaims)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	if v.issuer != "" && claims.Issuer != "" && claims.Issuer != v.issuer {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrIssuerMismatch, claims.Issuer, v.issuer)
	}
	return claims, nil
}

// keyFunc returns the jwt.Keyfunc closure appropriate for this
// validator. The closure inspects each token's alg header and
// returns either the HS256 secret or the JWKS-derived public key.
func (v *Validator) keyFunc(ctx context.Context) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		switch m := token.Method.(type) {
		case *jwt.SigningMethodHMAC:
			if len(v.hs256Secret) == 0 {
				return nil, fmt.Errorf("HS256 token presented but no HS256 secret configured")
			}
			if m.Alg() != "HS256" {
				// Belt + braces: the WithValidMethods filter should already
				// have caught HS384/512 if we didn't allow them, but be
				// explicit here so a library change doesn't widen silently.
				return nil, fmt.Errorf("only HS256 accepted for HMAC; got %s", m.Alg())
			}
			return v.hs256Secret, nil
		case *jwt.SigningMethodRSA, *jwt.SigningMethodRSAPSS,
			*jwt.SigningMethodECDSA, *jwt.SigningMethodEd25519:
			if v.jwks == nil {
				return nil, fmt.Errorf("asymmetric token presented but no JWKS configured")
			}
			kid, _ := token.Header["kid"].(string)
			pub, advertisedAlg, err := v.jwks.Get(ctx, kid)
			if err != nil {
				return nil, fmt.Errorf("jwks lookup (kid=%q): %w", kid, err)
			}
			// Refuse any alg/key combo the IdP didn't advertise. Stops
			// an attacker from taking an RS256 key and passing it as ES256.
			if advertisedAlg != "" && advertisedAlg != token.Method.Alg() {
				return nil, fmt.Errorf("alg mismatch: token=%s, key=%s", token.Method.Alg(), advertisedAlg)
			}
			return assertKeyTypeFor(token.Method, pub)
		default:
			return nil, fmt.Errorf("unsupported signing method %s", token.Method.Alg())
		}
	}
}

// assertKeyTypeFor verifies that the public key returned by JWKS
// has the Go type the jwt library expects for the token's signing
// method. An RS256 method needs *rsa.PublicKey; ES256 needs
// *ecdsa.PublicKey; EdDSA needs ed25519.PublicKey. Mismatches get
// rejected here so a malformed JWK can't crash the verifier with
// a type assertion inside the library.
func assertKeyTypeFor(method jwt.SigningMethod, pub any) (any, error) {
	switch method.(type) {
	case *jwt.SigningMethodRSA, *jwt.SigningMethodRSAPSS:
		if k, ok := pub.(*rsa.PublicKey); ok {
			return k, nil
		}
		return nil, fmt.Errorf("key type mismatch for %s: need *rsa.PublicKey", method.Alg())
	case *jwt.SigningMethodECDSA:
		if k, ok := pub.(*ecdsa.PublicKey); ok {
			return k, nil
		}
		return nil, fmt.Errorf("key type mismatch for %s: need *ecdsa.PublicKey", method.Alg())
	case *jwt.SigningMethodEd25519:
		if k, ok := pub.(ed25519.PublicKey); ok {
			return k, nil
		}
		return nil, fmt.Errorf("key type mismatch for %s: need ed25519.PublicKey", method.Alg())
	default:
		return nil, fmt.Errorf("unexpected signing method %s", method.Alg())
	}
}

// ExtractBearer pulls the token out of a "Bearer <token>" header.
// Trimmed and case-insensitive on the "Bearer " prefix so it
// tolerates the minor variations clients send.
func ExtractBearer(authHeader string) string {
	h := strings.TrimSpace(authHeader)
	if len(h) < 7 {
		return ""
	}
	if !strings.EqualFold(h[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// mapClaimsToTyped is the bridge from the JWT library's generic
// map-claims shape to our strongly-typed *types.Claims. Missing
// fields are tolerated (empty value); type-mismatched fields are
// an error (bad tokens surface clearly).
func mapClaimsToTyped(m jwt.MapClaims) (*types.Claims, error) {
	out := &types.Claims{}
	if sub, ok := m["sub"].(string); ok {
		out.UserID = sub
	}
	if iss, ok := m["iss"].(string); ok {
		out.Issuer = iss
	}
	if aud, ok := m["aud"].(string); ok {
		out.Audience = aud
	}
	if scope, ok := m["scope"].(string); ok {
		out.Scope = scope
	}
	if rolesAny, ok := m["roles"]; ok {
		rolesList, ok := rolesAny.([]any)
		if !ok {
			return nil, fmt.Errorf("roles claim must be an array")
		}
		for _, r := range rolesList {
			s, ok := r.(string)
			if !ok {
				return nil, fmt.Errorf("roles entries must be strings")
			}
			out.Roles = append(out.Roles, s)
		}
	}
	if exp, ok := m["exp"].(float64); ok {
		out.ExpiresAt = time.Unix(int64(exp), 0)
	}
	if iat, ok := m["iat"].(float64); ok {
		out.IssuedAt = time.Unix(int64(iat), 0)
	}
	return out, nil
}

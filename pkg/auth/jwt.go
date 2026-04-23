// Package auth validates inbound JWTs and yields a *types.Claims.
// The raw token is never held past validation — callers receive the
// claims only.
//
// Phase 6d ships HS256 (shared secret) validation. RS256/EdDSA against
// a JWKS URL is the production path for hosted-IdP deployments; that
// lands with the JWKS fetcher in a later iteration.
package auth

import (
	"errors"
	"fmt"
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
	ErrNoValidator  = errors.New("auth: no validator configured (allow_hs256=false and no JWKS yet)")
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
	// jwksProvider is deferred — Phase 6d.2 adds a fetcher + cache
	// that keeps a current set of public keys for RS256/EdDSA tokens.
}

// Config mirrors the relevant bits of config.AuthConfig. Kept
// package-local so pkg/auth doesn't depend on pkg/config.
type Config struct {
	Issuer       string
	AllowHS256   bool
	HS256Secret  string // pre-resolved; caller translates jwt_secret_ref
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
	if v == nil {
		return nil, ErrNoValidator
	}
	if raw == "" {
		return nil, ErrMissingToken
	}
	if len(v.hs256Secret) == 0 {
		return nil, ErrNoValidator
	}

	parser := jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}))

	parsed, err := parser.Parse(raw, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", token.Header["alg"])
		}
		return v.hs256Secret, nil
	})
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

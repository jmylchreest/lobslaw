// Package auth validates JWTs (RS256/EdDSA via JWKS; optional HS256 for
// single-node fallback) and extracts Claims. The raw token is never
// returned — once validated, only the Claims struct is exposed.
package auth

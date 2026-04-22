package types

import "time"

// Claims is the validated JWT payload. The raw token is discarded
// at validation time and never held here.
//
// Scope is a routing and audit-attribution label, not a
// confidentiality boundary.
type Claims struct {
	UserID    string    `json:"sub"`
	Roles     []string  `json:"roles,omitempty"`
	ExpiresAt time.Time `json:"exp"`
	IssuedAt  time.Time `json:"iat"`
	Issuer    string    `json:"iss"`
	Audience  string    `json:"aud,omitempty"`
	Scope     string    `json:"scope,omitempty"`
}

func (c *Claims) HasRole(role string) bool {
	for _, r := range c.Roles {
		if r == role {
			return true
		}
	}
	return false
}

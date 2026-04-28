package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/oauth"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// CredentialsConfig wires the oauth_start / oauth_status /
// oauth_revoke / credentials_grant / credentials_revoke builtins.
//
// All five route through the same oauth.Tracker (in-memory device-
// code flows) + memory.CredentialService (raft-replicated encrypted
// token store). Operators declare IdPs in [security.oauth.<name>];
// the node wires one Provider entry per declared IdP.
//
// Default policy is deny — these builtins mutate authentication and
// per-skill ACLs and must only be reachable by scope:owner. The
// node-level seed installs the deny rules; operators flip allows
// per scope when they want a non-owner principal to manage creds.
type CredentialsConfig struct {
	Tracker   *oauth.Tracker
	Service   *memory.CredentialService
	Providers map[string]oauth.ProviderConfig
}

// RegisterCredentialsBuiltins installs the five credential-management
// builtins. Returns an error when Tracker or Service is missing — a
// half-wired set would let operators start flows that can't persist.
// Empty Providers is allowed (operator hasn't declared any IdP yet);
// oauth_start surfaces "no providers configured" at call time.
func RegisterCredentialsBuiltins(b *Builtins, cfg CredentialsConfig) error {
	if cfg.Tracker == nil {
		return errors.New("credentials builtins: oauth.Tracker required")
	}
	if cfg.Service == nil {
		return errors.New("credentials builtins: memory.CredentialService required")
	}
	if err := b.Register("oauth_start", newOAuthStartHandler(cfg)); err != nil {
		return err
	}
	if err := b.Register("oauth_status", newOAuthStatusHandler(cfg)); err != nil {
		return err
	}
	if err := b.Register("oauth_revoke", newOAuthRevokeHandler(cfg)); err != nil {
		return err
	}
	if err := b.Register("credentials_grant", newCredentialsGrantHandler(cfg)); err != nil {
		return err
	}
	return b.Register("credentials_revoke", newCredentialsRevokeHandler(cfg))
}

// CredentialsToolDefs returns the ToolDefs operators register into
// the agent's function-calling list. RiskTier is Irreversible for
// every entry — even "start" surfaces a verification URI to a human;
// "grant" / "revoke" mutate the per-skill ACL; "revoke" deletes a
// credential entirely.
func CredentialsToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "oauth_start",
			Path:        BuiltinScheme + "oauth_start",
			Description: "Begin an OAuth 2.0 Device Authorization Grant flow with one of the configured providers (e.g. \"google\", \"github\"). Returns a user_code + verification URI the operator must visit in any browser to authorize. Polling completes in the background; check progress with oauth_status. Pass provider (the configured name) and optional scopes (list, defaults to provider's default_scopes when omitted). Owner-only; default-deny policy applies.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"provider": {"type": "string", "description": "Configured provider name (google, github, ...)."},
					"scopes":   {"type": "array", "items": {"type": "string"}, "description": "Override the provider's default scopes."}
				},
				"required": ["provider"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskIrreversible,
		},
		{
			Name:        "oauth_status",
			Path:        BuiltinScheme + "oauth_status",
			Description: "List in-progress device-code flows + connected credentials. Flows show outcome (pending/complete/expired/denied/cancelled/error), user_code, and verification URI. Credentials show provider, subject, scopes, and per-skill ACL.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "oauth_revoke",
			Path:        BuiltinScheme + "oauth_revoke",
			Description: "Delete a stored credential entirely. Pass provider and subject (the bucket key). Skills lose access immediately; running flows are unaffected. Use credentials_revoke to narrow per-skill access without removing the credential.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"provider": {"type": "string", "description": "Provider name (google, github, ...)."},
					"subject":  {"type": "string", "description": "Authenticated user identifier (email, login)."}
				},
				"required": ["provider", "subject"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskIrreversible,
		},
		{
			Name:        "credentials_grant",
			Path:        BuiltinScheme + "credentials_grant",
			Description: "Authorize a skill to use a stored credential. Pass provider, subject, skill (the name from its manifest), and scopes (always a subset of the credential's granted scopes). Re-grant overwrites the prior scope list. Skills require an explicit grant before they can use a credential — use this builtin to issue that grant.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"provider": {"type": "string"},
					"subject":  {"type": "string"},
					"skill":    {"type": "string", "description": "Skill name from its manifest (e.g. gws-workspace)."},
					"scopes":   {"type": "array", "items": {"type": "string"}, "description": "Scope subset for this skill."}
				},
				"required": ["provider", "subject", "skill", "scopes"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskIrreversible,
		},
		{
			Name:        "credentials_revoke",
			Path:        BuiltinScheme + "credentials_revoke",
			Description: "Remove a skill from a credential's per-skill ACL. The credential itself stays — only this skill loses access. Use oauth_revoke to delete the credential entirely.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"provider": {"type": "string"},
					"subject":  {"type": "string"},
					"skill":    {"type": "string"}
				},
				"required": ["provider", "subject", "skill"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskIrreversible,
		},
	}
}

func newOAuthStartHandler(cfg CredentialsConfig) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		name := strings.TrimSpace(args["provider"])
		if name == "" {
			return nil, 2, errors.New("oauth_start: provider is required")
		}
		p, ok := cfg.Providers[name]
		if !ok {
			available := make([]string, 0, len(cfg.Providers))
			for k := range cfg.Providers {
				available = append(available, k)
			}
			return nil, 2, fmt.Errorf("oauth_start: provider %q not configured (available: %v)", name, available)
		}
		var scopes []string
		if raw := args["scopes"]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
				return nil, 2, fmt.Errorf("oauth_start: scopes must be a JSON array: %w", err)
			}
		}
		initiatedBy := args["__user_id"]
		if scope := args["__scope"]; scope != "" {
			initiatedBy = scope + ":" + initiatedBy
		}
		flow, err := cfg.Tracker.Start(ctx, p, scopes, initiatedBy, makePersistCallback(cfg))
		if err != nil {
			return nil, 1, fmt.Errorf("oauth_start: %w", err)
		}
		snap := flow.Snapshot()
		out, _ := json.Marshal(map[string]any{
			"flow_id":          snap.ID,
			"provider":         snap.Provider,
			"user_code":        snap.UserCode,
			"verification_uri": snap.VerificationURI,
			"expires_at":       snap.ExpiresAt,
			"scopes":           snap.Scopes,
		})
		return out, 0, nil
	}
}

func newOAuthStatusHandler(cfg CredentialsConfig) BuiltinFunc {
	return func(ctx context.Context, _ map[string]string) ([]byte, int, error) {
		flows := cfg.Tracker.List()
		creds, err := cfg.Service.List(ctx)
		if err != nil {
			return nil, 1, fmt.Errorf("oauth_status: list credentials: %w", err)
		}
		// Redact tokens — operators inspecting status don't need to
		// see the secret material, only the metadata.
		type credView struct {
			Provider              string              `json:"provider"`
			Subject               string              `json:"subject"`
			Scopes                []string            `json:"scopes"`
			AllowedSkills         []string            `json:"allowed_skills,omitempty"`
			AllowedScopesPerSkill map[string][]string `json:"allowed_scopes_per_skill,omitempty"`
			ExpiresAt             string              `json:"expires_at,omitempty"`
		}
		credViews := make([]credView, 0, len(creds))
		for _, c := range creds {
			v := credView{
				Provider:              c.Provider,
				Subject:               c.Subject,
				Scopes:                c.Scopes,
				AllowedSkills:         c.AllowedSkills,
				AllowedScopesPerSkill: c.AllowedScopesPerSkill,
			}
			if !c.ExpiresAt.IsZero() {
				v.ExpiresAt = c.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")
			}
			credViews = append(credViews, v)
		}
		out, _ := json.Marshal(map[string]any{
			"flows":       flows,
			"credentials": credViews,
		})
		return out, 0, nil
	}
}

func newOAuthRevokeHandler(cfg CredentialsConfig) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		provider := strings.TrimSpace(args["provider"])
		subject := strings.TrimSpace(args["subject"])
		if provider == "" || subject == "" {
			return nil, 2, errors.New("oauth_revoke: provider and subject are required")
		}
		if err := cfg.Service.Delete(ctx, provider, subject); err != nil {
			return nil, 1, fmt.Errorf("oauth_revoke: %w", err)
		}
		out, _ := json.Marshal(map[string]any{
			"provider": provider,
			"subject":  subject,
			"deleted":  true,
		})
		return out, 0, nil
	}
}

func newCredentialsGrantHandler(cfg CredentialsConfig) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		provider := strings.TrimSpace(args["provider"])
		subject := strings.TrimSpace(args["subject"])
		skill := strings.TrimSpace(args["skill"])
		if provider == "" || subject == "" || skill == "" {
			return nil, 2, errors.New("credentials_grant: provider, subject, skill are all required")
		}
		var scopes []string
		if raw := args["scopes"]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
				return nil, 2, fmt.Errorf("credentials_grant: scopes must be a JSON array: %w", err)
			}
		}
		if len(scopes) == 0 {
			return nil, 2, errors.New("credentials_grant: scopes must be a non-empty subset of the credential's granted scopes")
		}
		if err := cfg.Service.Grant(ctx, provider, subject, skill, scopes); err != nil {
			return nil, 1, fmt.Errorf("credentials_grant: %w", err)
		}
		out, _ := json.Marshal(map[string]any{
			"provider": provider,
			"subject":  subject,
			"skill":    skill,
			"scopes":   scopes,
			"granted":  true,
		})
		return out, 0, nil
	}
}

func newCredentialsRevokeHandler(cfg CredentialsConfig) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		provider := strings.TrimSpace(args["provider"])
		subject := strings.TrimSpace(args["subject"])
		skill := strings.TrimSpace(args["skill"])
		if provider == "" || subject == "" || skill == "" {
			return nil, 2, errors.New("credentials_revoke: provider, subject, skill are all required")
		}
		if err := cfg.Service.Revoke(ctx, provider, subject, skill); err != nil {
			return nil, 1, fmt.Errorf("credentials_revoke: %w", err)
		}
		out, _ := json.Marshal(map[string]any{
			"provider": provider,
			"subject":  subject,
			"skill":    skill,
			"revoked":  true,
		})
		return out, 0, nil
	}
}

// makePersistCallback closes over the credential service so the
// oauth tracker's flow-completion callback can write the resulting
// tokens into the encrypted bucket. Subject is resolved via the
// provider's UserInfoEndpoint so the bucket key stays stable across
// re-authentications (re-running oauth_start for the same Google
// account refreshes the existing record rather than creating a new
// one). When the IdP doesn't expose a UserInfoEndpoint or the call
// fails, we fall back to a flow-tied synthetic key so the credential
// still persists — operators see the failure in oauth_status.
func makePersistCallback(cfg CredentialsConfig) oauth.CompleteCallback {
	return func(ctx context.Context, flow *oauth.Flow, tok *oauth.TokenResponse) error {
		subject, err := oauth.FetchSubject(ctx, flow.Provider, tok)
		if err != nil {
			subject = "flow-" + flow.ID
		} else {
			cfg.Tracker.SetSubject(flow.ID, subject)
		}
		expiresAt := flow.StartedAt
		if tok.ExpiresIn > 0 {
			expiresAt = flow.StartedAt.Add(time.Duration(tok.ExpiresIn) * time.Second)
		}
		cred := &memory.PlaintextCredential{
			Provider:     flow.Provider.Name,
			Subject:      subject,
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			Scopes:       splitScope(tok.Scope, flow.Scopes),
			ExpiresAt:    expiresAt,
		}
		return cfg.Service.Put(ctx, cred)
	}
}

// splitScope picks the effective scope set: provider-narrowed scopes
// (from the token response) when present, else the originally
// requested set from the flow.
func splitScope(tokScope string, fallback []string) []string {
	tokScope = strings.TrimSpace(tokScope)
	if tokScope == "" {
		return append([]string(nil), fallback...)
	}
	parts := strings.Fields(strings.ReplaceAll(tokScope, ",", " "))
	if len(parts) == 0 {
		return append([]string(nil), fallback...)
	}
	return parts
}


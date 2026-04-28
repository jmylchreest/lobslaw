package node

import (
	"context"
	"fmt"
	"time"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/oauth"
)

// SubjectResolver maps an inbound request's channel context onto a
// credential subject for the given provider. Decouples "which user
// is this skill running for?" from the credentials service so the
// resolution policy can evolve (today: pick the only matching
// credential; future: per-(channel, channel_id) binding stored in
// raft for team/corporate deployments).
//
// Empty channel + channelID is the default for skill calls that
// haven't carried channel context through (e.g. scheduled tasks).
// Implementations may treat that as "use the only credential" or as
// "fail loudly" depending on the deployment's multi-user posture.
type SubjectResolver interface {
	ResolveSubject(ctx context.Context, channel, channelID, provider string) (string, error)
}

// SingleUserSubjectResolver picks the only credential bound for a
// provider, ignoring channel context. The right resolver for any
// deployment with one human operator. Team/corporate deployments
// will swap this for a binding-bucket-backed implementation when
// that lands.
type SingleUserSubjectResolver struct {
	Service *memory.CredentialService
}

func (r SingleUserSubjectResolver) ResolveSubject(ctx context.Context, _, _, provider string) (string, error) {
	cred, err := r.Service.FindOnlyForProvider(ctx, provider)
	if err != nil {
		return "", err
	}
	return cred.Subject, nil
}

// credentialIssuerAdapter satisfies skills.CredentialIssuer by
// resolving manifest credentials: declarations against the cluster's
// CredentialService + per-provider oauth.RefreshToken. The
// SubjectResolver decides which (provider, subject) tuple gets
// fetched when the manifest leaves Subject empty.
type credentialIssuerAdapter struct {
	svc      *memory.CredentialService
	provider func(name string) (oauth.ProviderConfig, bool)
	subjects SubjectResolver
}

func newCredentialIssuerAdapter(svc *memory.CredentialService, provider func(string) (oauth.ProviderConfig, bool)) *credentialIssuerAdapter {
	return &credentialIssuerAdapter{
		svc:      svc,
		provider: provider,
		subjects: SingleUserSubjectResolver{Service: svc},
	}
}

// IssueForSkillByManifest is the entry point the invoker calls per
// declared credential. Resolves subject when omitted, looks up the
// provider config, and dispatches to the credential service's
// IssueForSkill (which handles ACL validation + refresh-on-expiry).
func (a *credentialIssuerAdapter) IssueForSkillByManifest(ctx context.Context, skill, provider, subject string) (string, []string, time.Time, error) {
	if a.svc == nil {
		return "", nil, time.Time{}, fmt.Errorf("credentials: service not wired")
	}
	if subject == "" {
		// Channel context isn't plumbed through invoker → adapter
		// today; resolver gets empty values and (for single-user)
		// returns the only credential. Team/corporate deployments
		// that need per-user binding will thread channel info via
		// InvokeRequest in a follow-up; the resolver interface is
		// stable across that change.
		resolved, err := a.subjects.ResolveSubject(ctx, "", "", provider)
		if err != nil {
			return "", nil, time.Time{}, err
		}
		subject = resolved
	}
	pcfg, ok := a.provider(provider)
	var refresher memory.TokenRefresher
	if ok {
		refresher = func(rctx context.Context, refreshToken string) (string, string, int, string, error) {
			tok, err := oauth.RefreshToken(rctx, pcfg, refreshToken)
			if err != nil {
				return "", "", 0, "", err
			}
			return tok.AccessToken, tok.RefreshToken, tok.ExpiresIn, tok.Scope, nil
		}
	}
	issued, err := a.svc.IssueForSkill(ctx, provider, subject, skill, refresher)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	return issued.AccessToken, issued.Scopes, issued.ExpiresAt, nil
}

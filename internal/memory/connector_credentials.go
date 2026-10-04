package memory

import (
	"context"
	"errors"

	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type connectorAccessKey struct{}

// ConnectorCredentials isolates trusted connectors from the legacy skill token
// issuance surface while sharing the encrypted store and Raft refresh protocol.
// This handle is wired by the node, never selected by a tool argument.
type ConnectorCredentials struct {
	service *CredentialService
	name    string
}

func (s *CredentialService) ForConnector(name string) *ConnectorCredentials {
	return &ConnectorCredentials{service: s, name: name}
}

func authorizeCredential(ctx context.Context, rec *lobslawv1.CredentialRecord) error {
	connector, _ := ctx.Value(connectorAccessKey{}).(string)
	if rec.Owner == "" && rec.Connector == "" && connector == "" {
		return nil
	}
	id, ok := turn.IdentityFrom(ctx)
	if !ok || id.Principal == "" || rec.Owner != string(id.Principal) || rec.Connector == "" || connector != rec.Connector {
		return types.ErrNotFound
	}
	return nil
}

func (c *ConnectorCredentials) context(ctx context.Context) (context.Context, error) {
	id, ok := turn.IdentityFrom(ctx)
	if !ok || id.Principal == "" || c.name == "" {
		return nil, errors.New("connector: authenticated principal required")
	}
	return context.WithValue(ctx, connectorAccessKey{}, c.name), nil
}

func (c *ConnectorCredentials) Put(ctx context.Context, p *PlaintextCredential) error {
	ctx, err := c.context(ctx)
	if err != nil {
		return err
	}
	if p == nil {
		return errors.New("connector: credential required")
	}
	id, _ := turn.IdentityFrom(ctx)
	copy := *p
	copy.Owner, copy.Connector = string(id.Principal), c.name
	copy.AllowedSkills = []string{c.name}
	copy.AllowedScopesPerSkill = map[string][]string{c.name: append([]string(nil), copy.Scopes...)}
	return c.service.Put(ctx, &copy)
}

func (c *ConnectorCredentials) Get(ctx context.Context, provider, subject string) (*PlaintextCredential, error) {
	ctx, err := c.context(ctx)
	if err != nil {
		return nil, err
	}
	return c.service.Get(ctx, provider, subject)
}

func (c *ConnectorCredentials) List(ctx context.Context) ([]*PlaintextCredential, error) {
	ctx, err := c.context(ctx)
	if err != nil {
		return nil, err
	}
	return c.service.List(ctx)
}

func (c *ConnectorCredentials) Delete(ctx context.Context, provider, subject string) error {
	ctx, err := c.context(ctx)
	if err != nil {
		return err
	}
	return c.service.Delete(ctx, provider, subject)
}

func (c *ConnectorCredentials) Issue(ctx context.Context, provider, subject string, refresh TokenRefresher) (*SkillIssue, error) {
	ctx, err := c.context(ctx)
	if err != nil {
		return nil, err
	}
	return c.service.IssueForSkill(ctx, provider, subject, c.name, refresh)
}

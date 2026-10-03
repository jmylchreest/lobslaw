package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/google/calendar"
	"github.com/jmylchreest/lobslaw/internal/tools"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func (n *Node) wireCalendar(b *tools.Builtins) error {
	c := n.cfg.Security.GoogleCalendar
	if !c.Enabled {
		return nil
	}
	if n.credentialSvc == nil || n.integrationState == nil || n.policyEngine == nil {
		return errors.New("calendar requires local memory and policy services")
	}
	clientID, err := n.resolveAPIKey(c.ClientIDRef)
	if err != nil {
		return fmt.Errorf("calendar client ID: %w", err)
	}
	secret, err := n.resolveAPIKey(c.ClientSecretRef)
	if err != nil {
		return fmt.Errorf("calendar client secret: %w", err)
	}
	svc, err := calendar.New(calendar.Config{ClientID: clientID, ClientSecret: secret, RedirectURL: c.CallbackURL, Client: egress.For("integration/google-calendar").HTTPClient(), Credentials: n.credentialSvc.ForConnector("google-calendar"), State: n.integrationState, Authorize: n.authorizeCalendar})
	if err != nil {
		return err
	}
	n.calendarSvc = svc
	if err := tools.RegisterCalendarBuiltins(b, svc); err != nil {
		return err
	}
	for _, td := range tools.CalendarToolDefs() {
		if err := n.toolRegistry.Register(td); err != nil {
			return err
		}
	}
	for _, operation := range []string{"create", "update"} {
		n.executor.RegisterOperationGate("calendar_event_"+operation, func(ctx context.Context, _ *types.Claims, args map[string]string) error {
			m, err := tools.CalendarMutation(args)
			if err != nil {
				return err
			}
			p, err := svc.Prepare(ctx, operation, m)
			if err != nil {
				return err
			}
			if err := compute.ConfirmExactOperation(ctx, "calendar:change", p.ApprovalKey, p.Summary, types.ToolEffects{State: types.ToolWrites, Network: true, Reads: operation == "update"}); err != nil {
				return err
			}
			return svc.Approve(ctx, p.ID)
		})
	}
	return nil
}

// calendarManagement is reachable only from authenticated human management routes.
func (n *Node) calendarManagement() gateway.CalendarManagement {
	if n.calendarSvc == nil {
		return nil
	}
	return func(ctx context.Context, claims *types.Claims, req gateway.CalendarRequest) (any, error) {
		if claims == nil || claims.UserID == "" {
			return nil, errors.New("calendar: authentication required")
		}
		ctx = turn.WithIdentity(ctx, turn.Identity{UserID: claims.UserID, Principal: n.identityResolver().Resolve(claims.UserID), Scope: claims.Scope, Roles: claims.Roles})
		switch req.Operation {
		case "connect":
			return n.calendarSvc.Begin(ctx, req.Write)
		case "pending":
			return n.calendarSvc.Pending(ctx, req.ID)
		case "activate":
			return n.calendarSvc.Activate(ctx, req.ID, req.Email, req.Calendars)
		case "list":
			return n.calendarSvc.Connections(ctx)
		case "disconnect":
			return struct{}{}, n.calendarSvc.Disconnect(ctx, req.ID)
		default:
			return nil, errors.New("calendar: unknown management operation")
		}
	}
}

func (n *Node) authorizeCalendar(ctx context.Context, action, resource string) error {
	id, ok := turn.IdentityFrom(ctx)
	if !ok || id.Principal == "" {
		return errors.New("calendar: caller identity required")
	}
	decision, err := n.policyEngine.Evaluate(ctx, &types.Claims{UserID: id.UserID, Scope: id.Scope, Roles: id.Roles}, action, resource)
	if err != nil {
		return err
	}
	if decision.Effect != types.EffectAllow {
		return fmt.Errorf("calendar: %s denied by policy", action)
	}
	return nil
}

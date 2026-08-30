package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// `/grants` — seeing and undoing an approval where it was given.
//
// The CLI covers the operator. This covers the person who actually
// tapped the button, which is not always the same person and is never
// the same moment: an approval given mid-task in a chat is answered in
// seconds and regretted later, and until now the only way to undo it
// from there was /new — which also destroys the transcript.
//
// So the two are separated. "I regret that approval" and "forget what
// we discussed" are different requests, and answering the first with
// the second is why nobody performed it.

// GrantView is one standing grant, in the terms a chat reply needs.
// Declared here rather than reusing the proto so the gateway does not
// grow a dependency on the memory package's storage shape.
type GrantView struct {
	ID        string
	Action    string
	Resource  string
	GrantedBy string
	ExpiresIn time.Duration
}

// SessionGrants is what /grants needs: read one conversation's
// standing approvals, and drop them.
//
// No per-id revoke. In a chat the unit is the conversation the person
// is looking at; picking one grant out of a list means quoting an id
// with an unprintable separator in it, which is a CLI gesture and not
// a chat one.
type SessionGrants interface {
	ForSession(sessionID string) ([]GrantView, error)
	RevokeSession(ctx context.Context, sessionID string) (int, error)
}

// RegisterGrantCommands installs /grants. A nil store leaves it
// unregistered — a node with no replicated grants has nothing to show,
// and a command that always answers "none" would read as "you have
// approved nothing" rather than "this cannot be answered here".
func RegisterGrantCommands(cs *CommandSet, grants SessionGrants) {
	if cs == nil || grants == nil {
		return
	}
	cs.Register(&Command{
		Name:    "grants",
		Summary: "show what this conversation has approved, and undo it",
		// The conversation's own standing state, and everyone subject
		// to it can already trigger the operations it covers. Hiding it
		// from the room would mean the people it applies to are the
		// ones who cannot see it.
		SharedSafe: true,
		// It reads and writes ONE conversation's grants, so a channel
		// that cannot say which conversation must refuse rather than
		// act on the wrong one.
		SessionScoped: true,
		Handler: func(ctx context.Context, req CommandRequest) (string, error) {
			sessionID := req.Session.Channel + ":" + req.Session.ChannelID
			if strings.EqualFold(strings.TrimSpace(req.Args), "revoke") {
				return revokeConversationGrants(ctx, grants, sessionID)
			}
			return listConversationGrants(grants, sessionID)
		},
	})
}

func listConversationGrants(grants SessionGrants, sessionID string) (string, error) {
	held, err := grants.ForSession(sessionID)
	if err != nil {
		return "", err
	}
	if len(held) == 0 {
		return "Nothing standing — I'll ask before anything that needs approval.", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "This conversation has approved %d thing(s):\n", len(held))
	for _, g := range held {
		fmt.Fprintf(&b, "\n• %s", describeGrant(g))
	}
	b.WriteString("\n\nSend `/grants revoke` to drop all of them. " +
		"That leaves the conversation itself alone — `/new` is what forgets it.")
	return b.String(), nil
}

func revokeConversationGrants(ctx context.Context, grants SessionGrants, sessionID string) (string, error) {
	n, err := grants.RevokeSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if n == 0 {
		// Distinct from success. "Revoked 0" reads as a no-op somebody
		// has to interpret; this says the state was already what they
		// wanted.
		return "There was nothing to revoke — this conversation holds no standing approvals.", nil
	}
	return fmt.Sprintf("Revoked %d standing approval(s). I'll ask again next time.", n), nil
}

// describeGrant renders one grant for a chat.
//
// Action and resource rather than the id: the id carries an
// unprintable separator and names nothing a person recognises, while
// "shell:run — git status" is the thing they said yes to.
func describeGrant(g GrantView) string {
	line := g.Action
	if g.Resource != "" {
		line += " — " + g.Resource
	}
	var detail []string
	if g.GrantedBy != "" {
		detail = append(detail, "by "+g.GrantedBy)
	}
	switch {
	case g.ExpiresIn > 0:
		detail = append(detail, "expires in "+g.ExpiresIn.Round(time.Minute).String())
	case g.ExpiresIn < 0:
		// The sweeper runs periodically, so an expired grant can still
		// be listed. Saying so beats showing it as live.
		detail = append(detail, "expired")
	}
	if len(detail) > 0 {
		line += "  (" + strings.Join(detail, ", ") + ")"
	}
	return line
}

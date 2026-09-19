package node

import (
	"testing"
)

// A routine created by a bot runs as that bot, and one a person created
// runs as that person. Reading only CreatedBy (which nothing sets) made
// every routine a generic scheduler turn.
func TestSchedulerIdentity(t *testing.T) {
	t.Parallel()

	claims, botID, principal := schedulerIdentity("bot:designer")
	if botID != "designer" || principal.String() != "bot:designer" {
		t.Fatalf("bot owner resolved to botID=%q principal=%q", botID, principal)
	}
	if claims.UserID != "bot:designer" || claims.Scope != schedulerScope {
		t.Fatalf("bot claims = %+v", claims)
	}

	claims, botID, principal = schedulerIdentity("user:alice")
	if botID != "" || !principal.IsZero() {
		t.Fatalf("human owner grew a bot: botID=%q principal=%q", botID, principal)
	}
	if claims.UserID != "alice" || claims.Scope != schedulerScope {
		t.Fatalf("human claims = %+v", claims)
	}

	claims, _, principal = schedulerIdentity("")
	if claims.UserID != defaultSchedulerCreator || !principal.IsZero() {
		t.Fatalf("unowned task = %+v principal=%q", claims, principal)
	}
}

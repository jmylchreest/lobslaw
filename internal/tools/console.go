package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// CodeMinter mints a one-time console sign-in code for one account.
// Implemented by the gateway, which owns the code store.
type CodeMinter interface {
	MintLoginCode(ctx context.Context, principal string) (code, userID string, expiresIn int, err error)
}

// ConsoleCodeToolDef is the single tool: "give me a code to sign in".
func ConsoleCodeToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name: "console_code",
		Path: compute.BuiltinScheme + "console_code",
		Description: "Mint a one-time code so the operator can sign in to the web console. " +
			"Only an operator may call this. The code is single-use and expires in a few minutes; " +
			"present it plainly and say how long it is valid for.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {},
			"additionalProperties": false
		}`),
		RiskTier: types.RiskReversible,
	}
}

// RegisterConsoleCodeBuiltin installs console_code.
//
// The gate is the CALLER's roles, not the channel: a code is a console
// credential, so only somebody who already holds role:operator may ask
// for one. A nil minter leaves it unregistered — a node with no console
// has no code to give.
func RegisterConsoleCodeBuiltin(b *Builtins, m CodeMinter) error {
	if m == nil {
		return nil
	}
	return b.Register("console_code", func(ctx context.Context, _ map[string]string) ([]byte, int, error) {
		id, ok := turn.IdentityFrom(ctx)
		if !ok {
			return nil, 1, errors.New("console_code: no turn identity")
		}
		if !slices.Contains(id.Roles, identity.RoleOperator) {
			return nil, 1, errors.New("console_code: only an operator may mint a console sign-in code")
		}
		principal := id.Principal.String()
		if principal == "" {
			return nil, 1, errors.New("console_code: this turn has no account to mint a code for")
		}
		code, userID, expiresIn, err := m.MintLoginCode(ctx, principal)
		if err != nil {
			return nil, 1, fmt.Errorf("console_code: %w", err)
		}
		body, err := json.Marshal(map[string]any{
			"code":       strings.TrimSpace(code),
			"user_id":    userID,
			"expires_in": expiresIn,
			"note":       "single use; tell them how long it lasts and that it works once",
		})
		return body, 0, err
	})
}

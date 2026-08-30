package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Every confirmation used to be one-shot, so the same question came
// back forever and the operator learned to tap Approve without
// reading. "For this chat" fixed part of it; this is the rest.
//
// A permanent approval is a policy allow rule, not a second store
// beside the policy engine. The engine already evaluates
// (subject, action, resource) and already has allow as an effect, so
// two things deciding the same question would eventually disagree.
//
// The whole risk of a permanent grant is that the user forgets they
// gave it. created_by is what answers that: an approval-minted rule is
// findable and revocable as a class, rather than sitting anonymously
// among rules the operator wrote on purpose.

// ApprovalRulePrefix marks a rule as minted by an "always" approval.
// The full value is ApprovalRulePrefix + the prompt id.
const ApprovalRulePrefix = "approval:"

// Provenance for the rules lobslaw writes itself.
//
// Every automatic writer stamps one of these, so a rule carrying
// neither them nor ApprovalRulePrefix is one nothing in this process
// created — which is what makes it safe to remove a rule whose reason
// for existing has gone. Before these existed, a config-seeded rule
// and a builtin's tool:exec allow were indistinguishable from each
// other and from anything a person had put there by hand, so nothing
// could be reconciled and a rule deleted from config stayed in force
// for the life of the store.
const (
	// RuleSourceConfig marks a rule seeded from [[policy.rules]].
	// Reconciled against config on every boot: edited there, edited
	// here; removed there, removed here.
	RuleSourceConfig = "config"

	// RuleSourceSeed marks a default lobslaw writes for a registered
	// tool. Removed when the tool it is about stops being registered.
	RuleSourceSeed = "seed"
)

// ErrHardlineRule is returned when an approval would mint a rule for
// something the floor refuses.
var ErrHardlineRule = errors.New("policy: an approval cannot grant what the hardline floor denies")

type raftApplier interface {
	Apply(data []byte, timeout time.Duration) (any, error)
}

// ApprovalRules mints and revokes the rules behind "always".
type ApprovalRules struct {
	raft  raftApplier
	store *memory.Store
}

func NewApprovalRules(raft raftApplier, store *memory.Store) (*ApprovalRules, error) {
	if raft == nil || store == nil {
		return nil, errors.New("approval rules: Raft and Store are both required")
	}
	return &ApprovalRules{raft: raft, store: store}, nil
}

// MintRequest is one "always" approval.
type MintRequest struct {
	// PromptID becomes the rule's provenance and part of its id, so
	// re-approving the same prompt is idempotent rather than piling up
	// duplicate rules.
	PromptID string
	// Subject is the principal the grant belongs to, in the policy
	// engine's own vocabulary ("user:alice", "role:admin",
	// "scope:ops"). Required, and validated: an empty subject matches
	// everyone, and an unrecognised kind fails closed — which would
	// mint a rule that looks like a grant in a listing and grants
	// nothing in practice.
	//
	// NOT a conversation ("telegram:-100"). A permanent grant belongs
	// to a principal; the conversation-scoped equivalent is
	// SessionApprovals, which is keyed that way on purpose.
	Subject  string
	Action   string
	Resource string
}

// approvalSubjectKinds are the subject prefixes the engine can
// actually match. Kept in step with subjectMatches — a kind here that
// the engine does not know produces a dead rule.
var approvalSubjectKinds = []string{"user:", "role:", "scope:"}

// Mint records a permanent allow rule for an approved operation.
func (a *ApprovalRules) Mint(_ context.Context, req MintRequest) (*lobslawv1.PolicyRule, error) {
	promptID := strings.TrimSpace(req.PromptID)
	subject := strings.TrimSpace(req.Subject)
	action := strings.TrimSpace(req.Action)
	resource := strings.TrimSpace(req.Resource)

	switch {
	case promptID == "":
		return nil, errors.New("approval rule: prompt id is required for provenance")
	case subject == "":
		return nil, errors.New("approval rule: subject is required; an empty subject matches everyone")
	case action == "":
		return nil, errors.New("approval rule: action is required")
	case resource == "":
		return nil, errors.New("approval rule: resource is required")
	}

	if !hasAnyPrefix(subject, approvalSubjectKinds) {
		return nil, fmt.Errorf(
			"approval rule: subject %q is not a principal the engine can match; "+
				"use one of %v — a conversation-scoped grant is SessionApprovals, not a rule",
			subject, approvalSubjectKinds)
	}

	// A wildcard in either position turns "always allow this" into
	// "always allow everything of this kind", which is not what the
	// button offered. Refused rather than narrowed, because silently
	// granting something narrower than the user asked for would be its
	// own surprise.
	if strings.Contains(action, "*") || strings.Contains(resource, "*") {
		return nil, fmt.Errorf(
			"approval rule: refusing a wildcard grant (action=%q resource=%q); "+
				"an approval covers the operation the user saw, not a class of them",
			action, resource)
	}

	// The floor, checked here as well as in the executor. Belt and
	// braces on purpose: the executor check means such a rule could
	// never be USED, and this one means it is never WRITTEN — so a
	// rule listing never shows an operator a grant that reads as
	// though it works.
	if err := hardlineGuard(resource); err != nil {
		return nil, err
	}

	rule := &lobslawv1.PolicyRule{
		Id:        ApprovalRulePrefix + promptID,
		Subject:   subject,
		Action:    action,
		Resource:  resource,
		Effect:    "allow",
		CreatedBy: ApprovalRulePrefix + promptID,
		CreatedAt: timestamppb.Now(),
		// Below any operator-authored deny. An approval is the user
		// answering one prompt; it should not outrank a rule somebody
		// wrote deliberately.
		Priority: 1,
	}

	if err := a.apply(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      rule.Id,
		Payload: &lobslawv1.LogEntry_PolicyRule{PolicyRule: rule},
	}); err != nil {
		return nil, err
	}
	return rule, nil
}

// FromApprovals lists every rule an approval minted, newest first is
// not guaranteed — the caller sorts if it cares.
func (a *ApprovalRules) FromApprovals() ([]*lobslawv1.PolicyRule, error) {
	var out []*lobslawv1.PolicyRule
	err := a.store.ForEach(memory.BucketPolicyRules, func(_ string, raw []byte) error {
		var rule lobslawv1.PolicyRule
		if err := proto.Unmarshal(raw, &rule); err != nil {
			return nil //nolint:nilerr // one unreadable rule should not hide the rest
		}
		if strings.HasPrefix(rule.CreatedBy, ApprovalRulePrefix) {
			out = append(out, &rule)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list approval rules: %w", err)
	}
	return out, nil
}

// Revoke deletes an approval-minted rule.
//
// Refuses to delete anything else: an operator-authored rule removed
// by a "revoke my approvals" command would be a silent policy change
// nobody asked for.
func (a *ApprovalRules) Revoke(id string) error {
	raw, err := a.store.Get(memory.BucketPolicyRules, id)
	if err != nil {
		return fmt.Errorf("revoke %q: %w", id, err)
	}
	var rule lobslawv1.PolicyRule
	if err := proto.Unmarshal(raw, &rule); err != nil {
		return fmt.Errorf("revoke %q: decode: %w", id, err)
	}
	if !strings.HasPrefix(rule.CreatedBy, ApprovalRulePrefix) {
		return fmt.Errorf("revoke %q: not an approval-minted rule (created_by=%q)", id, rule.CreatedBy)
	}
	return a.apply(&lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_DELETE,
		Id: id,
		Payload: &lobslawv1.LogEntry_PolicyRule{
			PolicyRule: &lobslawv1.PolicyRule{Id: id},
		},
	})
}

// hardlineGuard refuses a grant the floor would deny anyway.
//
// Resources here are tool names and paths, so both entry points are
// consulted: a resource that names a protected path, and one that
// reads as a command.
func hardlineGuard(resource string) error {
	if verdict, hErr := CheckPath(resource); verdict == PathDenied {
		return fmt.Errorf("%w: %v", ErrHardlineRule, hErr)
	}
	if hErr := CheckCommand(resource); hErr != nil {
		return fmt.Errorf("%w: %v", ErrHardlineRule, hErr)
	}
	// A resource can now BE a command, so the paths inside one have to
	// be checked too: without this, "cat /etc/shadow" reads as an
	// unremarkable word to CheckPath and passes CheckCommand. The
	// executor refuses it long before a prompt is raised, so this is
	// belt and braces — but the point of the guard is that a rule
	// listing never shows an operator a grant that reads as though it
	// works.
	if hErr := CheckCommandPaths(resource); hErr != nil {
		return fmt.Errorf("%w: %v", ErrHardlineRule, hErr)
	}
	return nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) && len(s) > len(p) {
			return true
		}
	}
	return false
}

func (a *ApprovalRules) apply(entry *lobslawv1.LogEntry) error {
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("approval rule: marshal: %w", err)
	}
	res, err := a.raft.Apply(data, 5*time.Second)
	if err != nil {
		return fmt.Errorf("approval rule: raft apply: %w", err)
	}
	if ferr, ok := res.(error); ok && ferr != nil {
		return ferr
	}
	return nil
}

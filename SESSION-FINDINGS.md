# Session findings — decision candidates and bugs

Working document, not a deliverable. Two lists kept while working the
comment pass and the QwenCloud wiring. Review, then either promote to
`aide decision` records / issues, or delete this file.

Nothing here has been added to aide. Wording is deliberately phrased as
a rule rather than as a story, so a decision record can be lifted from
it directly.

---

## A. Decision record candidates

### A1. Comments must not assert point-in-time facts about other code

**Rule.** A comment may describe an invariant, a consequence, or a
constraint. It may not assert the current state of code elsewhere —
"X has no callers", "nothing calls Y", "not yet implemented", "Phase N
will replace this", "only the first step is dispatched today". Those
are true at the moment of writing and are not maintained by anything.

**Evidence.** 183 comment blocks in the tree assert such a fact. Eight
had gone stale, and of those, **six asserted the exact opposite of what
the code now does** — including one telling the reader that skills run
unsandboxed when `sandbox.Apply` is called nine lines away.

**Relationship to existing decisions.** Sharpens `code-documentation`
(2026-04-21, "write docs for WHY, not WHAT") with a testable rule. A
grep for the phrases above is a cheap lint.

**Reusable form.** *Describe what must be true, not what happens to be
true.*

### A2. Incident and threat-model comments are protected from cleanup

**Rule.** A comment recording a production incident, a near-miss, or a
threat model is load-bearing documentation and is exempt from comment
pruning. Delete it only when the mechanism it describes is gone.

**Evidence.** `memory/visibility.go` records that a `scopeFilter
string` with `""` meaning "everything" put one user's memories into
another user's prompt. `skills/registry.go` records the escalation path
where an agent-authored skill takes a signed skill's name by setting
version 99.0.0. `scripts/smoke.sh` records that R28 and R29 shipped
green and did not work. Nothing else in the repo records any of these.

**Why it needs saying.** A1 and A2 pull in opposite directions on
purpose. Without A2, a literal reading of A1 deletes the best comments
in the codebase.

### A3. Deleting a config field means deleting every shipped instance of it

**Rule.** When a config key is removed from a struct, remove it in the
same commit from `lobslaw init`'s template and every `deploy/*/`
config. Unknown keys are ignored silently, so a stale key is invisible
to the operator who sets it and expects it to do something.

**Evidence.** `SnapshotConfig` was reduced to `Target` alone, with a
comment correctly noting `cadence` and `retention` had been removed —
while `cmd/lobslaw/init.go` and both deploy configs kept writing them.
Every node ever produced by `lobslaw init` carries two dead keys.

**Reusable form.** *A config surface is the struct plus every file that
writes it.*

### A4. A driver must derive follow-up URLs from its configured host

**Rule.** When a driver's endpoint is configurable, every subsequent
URL in that exchange — poll, callback, status — is derived from the
configured host. A compiled-in default for the second URL is only ever
correct for the deployment the author happened to be using.

**Evidence.** `dashscope` submitted jobs to the configured host and
polled a hardcoded `dashscope-intl` URL. On a QwenCloud Token Plan
subscription that is HTTP 401 forever against work that is running and
billed. Beijing and US hosts fail identically. Fixed in #180.

**Reusable form.** *Second URLs are derived, not defaulted.*

### A5. Config that matches on model output needs a closed vocabulary

**Rule.** Any routing or policy rule keyed on free text a model
produced must constrain that model to a vocabulary derived from the
configured rules, and discard anything outside it. Otherwise the rule
matches by luck.

**Evidence.** Chain `trigger.domains` matches the preflight's `domains`
tags exactly. The judge prompt asks for "at most 3 lowercase
single-word tags"; live output included `distributed systems`,
`api-integration`, and more than three. Two models given the same
sentence agreed on zero tags. Filed as #181.

**Severity note.** The failure is silent and open — a `legal` or
`medical` rule that does not match sends the turn to a public-tier
provider instead of the private one intended.

### A6. An unpopulated security input must fail closed or fail loudly

**Rule.** Where an ACL, allowlist, or policy input can be empty, empty
must mean deny or must warn. Falling through to a permissive default is
indistinguishable from working.

**Evidence.** `egress.ACLInputs.SkillNetworks` is assigned an empty map
at boot and on every refresh, and populated nowhere. No `skill/<name>`
role is ever built, so every skill falls through to
`DefaultAllowedHosts` and a manifest's `network:` array constrains
nothing. The builder even contains an unreachable branch written to
deny that case with a useful message. Filed as #184.

### A7. A transport timeout must not be shorter than the application timeout it wraps

**Rule.** When two layers each bound the same operation, the outer one
must be the looser. Otherwise the inner layer's graceful degradation
can never run.

**Evidence.** The REST server's `WriteTimeout` is a hardcoded 60s and
`HardTimeout` defaults to 90s, so the socket is always killed before
the agent's forced-summary reply can be produced. The caller sees
`Empty reply from server` on a turn that completed server-side and
wrote its artifacts. Filed as #177.

**Reusable form.** *Order your deadlines outward.*

### A8. Shipping verification without signing leaves the secure tier unreachable

**Rule.** If a trust model has a verifying half, ship the producing
half with it, or document plainly that the secure tier is unusable.

**Evidence.** `trusted_publishers.toml` maps minisign keys to allowed
prefixes and `applySigningPolicy` verifies against it, but nothing in
lobslaw signs a bundle. A publisher must drive `minisign` by hand and
reproduce the layout of the unexported `canonicalBundleBytes`. In
practice every shared skill lands as `operator` tier, which means "I
trust whoever handed me this directory". Filed as #183.

### A9. Operational: `max_tokens` does not bound reasoning tokens

**Not a rule, a fact worth recording.** On the Qwen 3.x family,
`max_tokens: 800` returned `completion_tokens` of 9,064–12,385. Turn
latency cannot be capped that way, and any timeout sized from an
expected token count will be wrong. Measured on the hard prompt:
3.6-flash 91.6s, 3.8-flash 172.9s, 3.7-plus 212.6s, 3.8-max >240s.

Corollary worth keeping with it: on a *trivial* prompt the flagship is
FASTER than the mid-tier model (1.9s vs 3.9s), because it deliberates
less. "Bigger model = slower" does not hold; "harder prompt = slower"
does.

---

## B. Bugs

### Fixed

| # | Bug | Where |
|---|---|---|
| B1 | dashscope polled a compiled-in host instead of the one that took the job — 401 forever on any non-international deployment | merged, #180 |
| B2 | `cadence` / `retention` written by `lobslaw init` and both deploy configs for a struct that has neither | branch `chore/comment-pass` |
| B3 | Six comments asserting the opposite of current behaviour, incl. "skills run unsandboxed" | branch `chore/comment-pass` |

### Filed, open

| # | Bug | Issue |
|---|---|---|
| B4 | Video commitments submitted but never polled — jobs run, are billed, never delivered | #179 |
| B5 | REST turns >60s die with `Empty reply from server`; `WriteTimeout` hardcoded and shorter than `HardTimeout` | #177 |
| B6 | No DashScope/QwenCloud TTS driver; Qwen TTS is WebSocket-only and there is no websocket client in `go.mod` | #178 |
| B7 | Chain domain triggers match free-form model output — subject routing fails open and silent | #181 |
| B8 | No way to share an automation: schedules cannot be exported at all; skills have no publish or signing path | #183 |
| B9 | Per-skill egress ACLs never populated — `network:` in a manifest does not constrain egress | #184 |

### Found, not yet filed

| # | Bug | Notes |
|---|---|---|
| B10 | `RoleReranker` has no runtime callers | `reranker = "x"` is accepted, reported by `debug_providers`, and does nothing. Config that silently no-ops. Its only live behaviour is the fallback rule in `roles.go:136`. |
| B11 | `NewGenerationCommitment` takes an `iv time.Duration` it never uses; `startGenerationJob` passes `0` | The returned commitment carries `Trigger: "time"` with no interval and no next-fire. Prime suspect for B4 — verify before filing, likely a comment on #179 rather than its own issue. |
| B12 | `errManagerNotAvailable` declared, `//nolint:unused`, returned by no manager | Cosmetic; the sentinel was designed and never adopted. Either adopt it or delete it. |
| B13 | Schedule created via the agent did not persist | `schedule_list` returned 0 after the user set up weather alerts. Confounded — the creating turn was killed by the pre-fix `hard_timeout`. Re-test before filing. |
| B14 | `commitment_list` returns empty even with `include_history: true` for a commitment just created | Observed twice with video jobs. May be scoping rather than a bug. Part of B4's investigation. |

---

## C. Notes for the review

- A1 and A2 should be adopted together or not at all.
- A6 and A8 are both about the skills trust surface and may belong in
  one decision about untrusted-code confinement rather than two.
- B10 and B12 are the same shape as B9: a declared surface with no
  implementation behind it, accepted silently by config. If that
  pattern recurs a third time it is worth a decision of its own —
  something like *a config key that cannot affect behaviour must fail
  validation, not parse cleanly*.

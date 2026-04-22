---
topic: go-naming
decision: "Follow Go naming conventions: short packages, -er interfaces, Err prefix for sentinels, MixedCaps throughout"
decided_by: blueprint:go@0.0.59
date: 2026-04-21
references:
  - https://go.dev/doc/effective_go#names
---

# go-naming

**Decision:** Follow Go naming conventions: short packages, -er interfaces, Err prefix for sentinels, MixedCaps throughout

## Rationale

Consistent naming reduces cognitive load and makes code accessible to any Go developer. Go's naming conventions are well-established and enforced by community tooling.

## Details

- Packages: short, lowercase, no underscores/utils/helpers/common.
- Interfaces: -er suffix (Reader, UserFinder). No IUser/UserInterface.
- Sentinels: Err prefix. Constants: MixedCaps, no SCREAMING_SNAKE.
- Vars: short in narrow scope, descriptive in wide. user.New() not user.NewUser().


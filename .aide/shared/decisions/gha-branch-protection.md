---
topic: gha-branch-protection
decision: "Enforce PR workflow, required status checks, and up-to-date branches on main"
decided_by: blueprint:github-actions@0.0.59
date: 2026-04-21
references:
  - https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-a-branch-protection-rule/about-protected-branches
---

# gha-branch-protection

**Decision:** Enforce PR workflow, required status checks, and up-to-date branches on main

## Rationale

Branch protection prevents accidental direct pushes, ensures CI passes before merge, and catches integration issues from stale branches.

## Details

On main: require PR (no direct pushes), require status checks (lint, test, vuln), require up-to-date branches, dismiss stale approvals on new commits. Restrict push to maintainers + release bots. Optionally require signed commits.


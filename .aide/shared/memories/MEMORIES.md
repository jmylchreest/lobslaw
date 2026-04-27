# Team Memories

This folder contains team project memories — patterns, gotchas, and learnings —
grouped into one markdown file per category.

## With aide

Memories import automatically at session start when `AIDE_SHARE_AUTO_IMPORT=1` is
set in `.claude/settings.json`. Manually:

    aide share import --memories

Each memory is keyed by a ULID (in the `<!-- aide:id=... -->` metadata comment) so
teammate edits with a newer `updated=` timestamp land as in-place updates instead of
duplicates.

## Without aide

Each entry inside a category file is a self-contained memory with a short metadata
comment and a free-text body. Point your AI assistant at this folder as context —
the category headings group related notes.

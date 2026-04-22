---
topic: lobslaw-config-library
decision: "knadh/koanf/v2 with per-provider submodule imports (koanf/providers/file, koanf/parsers/toml, koanf/providers/env, koanf/providers/fsnotify). Rejected: Viper (too many bundled formats and indirect deps for our needs); hand-rolled (would duplicate env-override + file-watch logic already in koanf cleanly)"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-config-library

**Decision:** knadh/koanf/v2 with per-provider submodule imports (koanf/providers/file, koanf/parsers/toml, koanf/providers/env, koanf/providers/fsnotify). Rejected: Viper (too many bundled formats and indirect deps for our needs); hand-rolled (would duplicate env-override + file-watch logic already in koanf cleanly)

## Rationale

koanf is modular - import only the providers you use. Matches the aide go-third-party decision's principle of narrow-library-over-framework better than Viper does. Handles TOML + env override (LOBSLAW_SECTION_KEY format) + fsnotify-watch for hot-reload in a clean API. Roughly 1/10 the dependency weight of Viper


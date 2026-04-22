---
topic: lobslaw-config-env-override
decision: "All TOML config keys overridable via environment variables. Format: LOBSEY_SECTION_KEY_SUBKEY. Secrets always from env vars. Env vars override TOML values at load time"
date: 2026-04-22
---

# lobslaw-config-env-override

**Decision:** All TOML config keys overridable via environment variables. Format: LOBSEY_SECTION_KEY_SUBKEY. Secrets always from env vars. Env vars override TOML values at load time

## Rationale

Environment variable override is standard 12-factor app practice. Every value should be overridable without editing files, especially for secrets


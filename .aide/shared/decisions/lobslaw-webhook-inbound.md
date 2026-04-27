---
topic: lobslaw-webhook-inbound
decision: "New gateway channel type='webhook' accepts HTTP POST for inbound integrations (Zapier, IFTTT, n8n, GitHub Actions). Auth via Authorization: Bearer <shared_secret>; secret resolved via channel-secret-resolver (same as Telegram bot token). Each POST is one agent turn; reply returned as {reply, turn_id} JSON. Accepts JSON ({prompt: '...'}) or plaintext body. Mounted on the gateway HTTP mux at /webhook/<name> (or Path override). Empty shared_secret is refused at wire-up — no accidental unauthenticated webhooks."
date: 2026-04-24
---

# lobslaw-webhook-inbound

**Decision:** New gateway channel type='webhook' accepts HTTP POST for inbound integrations (Zapier, IFTTT, n8n, GitHub Actions). Auth via Authorization: Bearer <shared_secret>; secret resolved via channel-secret-resolver (same as Telegram bot token). Each POST is one agent turn; reply returned as {reply, turn_id} JSON. Accepts JSON ({prompt: '...'}) or plaintext body. Mounted on the gateway HTTP mux at /webhook/<name> (or Path override). Empty shared_secret is refused at wire-up — no accidental unauthenticated webhooks.

## Rationale

User wanted a way for external services to trigger the agent. Same REST port + TLS as the rest of the gateway so operators don't need a second listener. Shared-secret auth is simple operational practice for machine-to-machine calls (tokens rotate via redeploy). JSON + plaintext body both accepted because different services emit different shapes and the cost of supporting both is one strings.Contains check.


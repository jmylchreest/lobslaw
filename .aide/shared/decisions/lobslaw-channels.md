---
topic: lobslaw-channels
decision: "gRPC API with channel handlers on top. REST/HTTPS channel handler for web UI/chatbot. Telegram first (MVP), Slack later. Agent driven by incoming channel events"
date: 2026-04-22
---

# lobslaw-channels

**Decision:** gRPC API with channel handlers on top. REST/HTTPS channel handler for web UI/chatbot. Telegram first (MVP), Slack later. Agent driven by incoming channel events

## Rationale

Channels are first-class citizens, not afterthoughts. REST handler is just another channel. Telegram MVP aligns with ZeroClaw approach


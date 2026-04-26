# Local ML sidecars

How to keep multimodal work (STT, vision, PDF) entirely on-host instead of routing to OpenAI / OpenRouter / MiniMax.

The lobslaw side needs **zero code changes** for this. The capability-driven modality router (`compute.SelectByCapability`) is endpoint-agnostic — `endpoint = "http://whisper:8000/v1/audio/transcriptions"` and `endpoint = "https://api.openai.com/v1/audio/transcriptions"` are equivalent as long as both speak the OpenAI-compatible wire shape. The work is purely:

1. Pick a sidecar image that exposes the right surface.
2. Add a `[[compute.providers]]` entry pointing at it with the correct `capabilities = [...]` tag.
3. Set `priority` higher than any cloud entry so the local one wins.

This doc is reference material — it's not wired into the default deploy stack. Bring up only what you actually need.

---

## Audio → text (`capabilities = ["audio-transcription"]`)

### Option A: speaches (recommended starting point)

[`speaches`](https://github.com/speaches-ai/speaches) (the project formerly known as `faster-whisper-server`) ships an OpenAI Whisper-compatible API surface that supports **both** `faster-whisper` and Parakeet models behind the same endpoint. Choose model per request.

```yaml
services:
  speaches:
    image: ghcr.io/speaches-ai/speaches:latest-cpu
    ports:
      - "8001:8000"
    volumes:
      - speaches-models:/data
    environment:
      - WHISPER__MODEL=Systran/faster-whisper-base   # default model
      - WHISPER__INFERENCE_DEVICE=cpu

volumes:
  speaches-models:
```

Provider entries:

```toml
# Parakeet v3 — multilingual, ~25 languages, current SOTA on Open ASR Leaderboard.
# Fine on CPU via faster-parakeet / ONNX runtime; GPU optional.
[[compute.providers]]
label        = "local-parakeet"
endpoint     = "http://speaches:8000/v1/audio/transcriptions"
model        = "nvidia/parakeet-tdt-0.6b-v3"
api_key_ref  = ""
trust_tier   = "private"
capabilities = ["audio-transcription"]
priority     = 20

# Whisper as a fallback / for cases where you want a different family.
[[compute.providers]]
label        = "local-whisper"
endpoint     = "http://speaches:8000/v1/audio/transcriptions"
model        = "Systran/faster-whisper-base"
api_key_ref  = ""
trust_tier   = "private"
capabilities = ["audio-transcription"]
priority     = 15
```

`priority = 20` beats the cloud `fast-audio = 5` entries in the default config, so local Parakeet wins. When the runtime fallback chain lands (DEFERRED.md → "Modality fallback chain at runtime"), this becomes a true Parakeet → Whisper → cloud cascade on transient failure.

### Option B: standalone Parakeet via parakeet-rs

[`parakeet-rs`](https://github.com/Picovoice/parakeet) is the same Rust runtime [Handy](https://github.com/cjpais/Handy) uses for local push-to-talk on Linux. No server out of the box — needs a thin FastAPI / Axum wrapper exposing `/v1/audio/transcriptions`. ~50 lines of code, but worth it if you want zero-Python and very tight memory footprint.

Skip this unless `speaches` doesn't fit (memory pressure, OS that can't run the speaches image, etc.).

### Option C: NVIDIA Riva

NVIDIA's official inference server. Production-grade, very fast on CUDA. NOT OpenAI-compatible — you'd need to write a translation proxy. Defer unless you have NVIDIA hardware AND need the throughput; otherwise speaches covers the same ground with less setup.

---

## Vision (`capabilities = ["vision"]`)

### Option A: ollama (recommended)

`ollama` exposes an OpenAI-compatible `/v1/chat/completions` with multimodal content parts. Models like `llama3.2-vision:11b` and `llava:13b` work out of the box.

```yaml
services:
  ollama:
    image: ollama/ollama:latest
    ports:
      - "11434:11434"
    volumes:
      - ollama-models:/root/.ollama
    # Pull the vision model on first boot:
    #   podman exec ollama ollama pull llama3.2-vision:11b
    #   podman exec ollama ollama pull llava:13b

volumes:
  ollama-models:
```

Provider entry:

```toml
[[compute.providers]]
label        = "local-vision"
endpoint     = "http://ollama:11434/v1/chat/completions"
model        = "llama3.2-vision:11b"
api_key_ref  = ""
trust_tier   = "private"
capabilities = ["vision"]
priority     = 20
```

**CPU reality:** vision-on-CPU works but is slow — `llava:7b` does ~30s per image on a modern laptop CPU; `llama3.2-vision:11b` is multiple minutes without GPU. For interactive bot use you really want a GPU here, or stick with the cloud `read_image` path.

### Option B: llama.cpp server

`ghcr.io/ggerganov/llama.cpp:server` with a vision GGUF (LLaVA-1.5 / 1.6, MiniCPM-V). Lower memory than ollama, more setup. Same OpenAI-compatible endpoint shape. Skip unless ollama doesn't fit.

---

## PDF (`capabilities = ["pdf"]`)

There's no production-ready self-hosted sidecar for PDF *as a turnkey image* today. The two viable paths:

### Option A: route through the vision sidecar

Render PDF pages to images (poppler / pdf2image), feed each page to the local vision sidecar above. Wrap in a thin FastAPI proxy that exposes `/v1/chat/completions` accepting the OpenAI `file` content part shape, internally rasterising and re-issuing to ollama.

This is real work — ~150 lines of Python/Go + a reasonable test suite. Not worth doing until there's a clear need to keep PDFs on-host.

### Option B: keep cloud routing

Add an OpenRouter provider with `capabilities = ["pdf"]` (their `file` content part shape works with Gemini Flash and a few others). PDFs are typically rare in chat usage; this is the practical default.

---

## Bring-up sequence

If you want everything local:

```bash
# Pull / build sidecar images.
podman compose -f deploy/docker/docker-compose.yml \
               -f deploy/docker/sidecars.yml \
               pull

# Start the cluster + sidecars.
podman compose -f deploy/docker/docker-compose.yml \
               -f deploy/docker/sidecars.yml \
               up -d

# First-time model pulls (sidecars only — lobslaw nodes have no models).
podman exec ollama   ollama pull llama3.2-vision:11b
# speaches downloads its configured model on first request — no manual pull.
```

If `deploy/docker/sidecars.yml` doesn't exist yet, copy the YAML blocks from this doc into it. The compose-overlay pattern (`-f base.yml -f overlay.yml`) lets you opt in/out without editing the base.

---

## Hardware reality check

| Workload | CPU-only (no GPU) | Modern GPU (8GB+ VRAM) |
|---|---|---|
| **Parakeet v3 (audio)** | ✅ Fine for voice notes (10× realtime on modern laptop CPU) | ✅ Trivial (50-100× realtime) |
| **Whisper-base (audio)** | ✅ Trivial | ✅ Trivial |
| **Whisper-large-v3 (audio)** | ⚠️ Slow but viable (1-2× realtime) | ✅ Fast |
| **llava:7b (vision)** | ⚠️ ~30s/image — usable but laggy | ✅ Fast |
| **llama3.2-vision:11b (vision)** | ❌ Multiple minutes/image | ✅ Workable |
| **PDF via rasterise+vision** | ❌ Painful for multi-page docs | ⚠️ Workable for short PDFs |

Translation: with no GPU, **Parakeet for STT is great** but vision should stay on the cloud `read_image` path until you add a GPU.

---

## When the modality fallback chain lands

`DEFERRED.md → "Modality fallback chain at runtime"` will mean local sidecars become true preferred-with-fallback rather than first-match-wins. After that:

1. Parakeet (priority 20) tries first — if container is down or rate-limits, fail through to
2. Whisper (priority 15) — if also down, fail through to
3. Cloud fast-audio (priority 5)

Until then, the highest-priority provider that's *up at boot time* wins for the whole session. Restart lobslaw to re-discover after sidecar status changes.

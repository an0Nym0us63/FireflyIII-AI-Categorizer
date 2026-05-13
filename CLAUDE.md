# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go build ./...                          # build all packages
go run ./cmd/server                     # run the server
go run ./cmd/server -batch              # run batch mode (categorize all uncategorized withdrawals and exit)
go test ./...                           # run all tests
docker build -t firefly-ai-categorize . # build Docker image
```

There is no lint script configured. The project uses Go 1.22 modules (`go.mod`).

## Architecture

This is a Go HTTP server (chi router) that acts as a Firefly III webhook receiver. When Firefly III records a new withdrawal, it POSTs to `/webhook`. The app classifies the transaction via an LLM and writes the result back to Firefly III.

**Request flow:**

```
POST /webhook
  → internal/api/handler.go validates payload (must be STORE_TRANSACTION, withdrawal, no existing category)
  → Creates a Job in job.Registry (in-memory, emitted via SSE to UI)
  → Submits async Task to worker.Pool (configurable concurrency)
    → pipeline.Pipeline.Run()
      → firefly.Client.GetCategories() — paginated fetch, cached for 5 min
      → cache.Cache.GetHistory() — TTL-backed history of past classifications
      → classifier.Classifier.Classify() — sends to LLM with structured JSON output
      → firefly.Client.UpdateTransaction() — writes category_id, tag, and notes back
```

**Three-outcome classification model** (`internal/classifier/classifier.go`):
- `CLASSIFIED` — confident match; sets category + `<prefix>:classified` tag
- `ASSUMED` — best guess with conservative default; sets category + `<prefix>:assumed` tag + assumption in notes
- `NEEDS_REVIEW` — cannot classify; sets `<prefix>:needs-review` tag, no category

**Key packages:**
- `cmd/server/main.go` — entry point; wires config, pools, handler; supports `-batch` flag
- `internal/api/handler.go` — chi router, webhook handler, batch trigger, config/jobs/SSE API endpoints
- `internal/pipeline/pipeline.go` — shared classify-and-update flow used by webhook and batch
- `internal/classifier/` — `Classifier` interface + OpenAI, Gemini, Deepseek implementations; `build.go` selects provider
- `internal/firefly/client.go` — Firefly III API client (categories, transactions, update with tags/notes)
- `internal/job/registry.go` — in-memory job store with pub/sub for SSE updates
- `internal/worker/worker.go` — bounded goroutine pool; tasks run with `context.Background()` (not request context)
- `internal/cache/cache.go` — TTL-backed history cache of categorized transactions from Firefly
- `internal/config/config.go` — env var loading; `config/store.go` — JSON config file that overlays env vars at runtime

**Config persistence:** The UI can update config at runtime via `PUT /api/config`. Changes are stored in `config.json` (default; overridable via `CONFIG_FILE` env var) and override env vars. Hot-reload rebuilds the Firefly client, history cache, and pipeline without restarting.

**UI** (`public/`): optional real-time job monitor, served only when `ENABLE_UI=true`. Uses vanilla JS + SSE (`/events`) to render jobs and support batch runs/review.

**CI** (`.github/workflows/main.yml`): builds and pushes a multi-arch Docker image to `ghcr.io` on pushes to `main` and version tags.

## Environment variables

| Variable | Required | Default | Notes |
|----------|----------|---------|-------|
| `FIREFLY_URL` | Yes* | — | Trailing slash is stripped automatically |
| `FIREFLY_PERSONAL_TOKEN` | Yes* | — | |
| `AI_PROVIDER` | No | `openai` | `openai` \| `gemini` \| `deepseek` |
| `OPENAI_API_KEY` | Yes* (openai) | — | |
| `OPENAI_MODEL` | No | `gpt-4o-mini` | Any OpenAI-compatible model |
| `OPENAI_BASE_URL` | No | — | For Ollama, Azure, etc. |
| `GEMINI_API_KEY` | Yes* (gemini) | — | |
| `GEMINI_MODEL` | No | `gemini-3.1-flash-lite` | |
| `DEEPSEEK_API_KEY` | Yes* (deepseek) | — | |
| `DEEPSEEK_MODEL` | No | `deepseek-chat` | |
| `TAG_PREFIX` | No | `ai` | Produces `<prefix>:classified`, etc. |
| `ENABLE_UI` | No | `true` | Serve the job monitor at `/` |
| `PORT` | No | `3000` | |
| `WORKER_CONCURRENCY` | No | `1` | Concurrent webhook classifications |
| `BATCH_CONCURRENCY` | No | `5` | Concurrent batch classifications |
| `HISTORY_CONTEXT_LIMIT` | No | `5` | Max past classifications sent to LLM |
| `HISTORY_LOOKBACK_DAYS` | No | `90` | How far back to fetch history |
| `HISTORY_CACHE_TTL` | No | `10m` | Go duration string |
| `CONFIG_FILE` | No | `config.json` | Path to runtime config JSON |

\* Required fields are not validated at startup — the server starts and allows first-time configuration via the UI.

## Important implementation details

- Categories are cached in the pipeline for 5 minutes (`categoryTTL`), not fetched per-job. History cache has a configurable TTL (default 10m).
- `firefly.Client.UpdateTransaction()` sets `fire_webhooks: false` to prevent re-triggering.
- The classifier validates that the returned category name is an exact match; if not, falls back to `NEEDS_REVIEW`.
- Worker pools run tasks with `context.Background()` — HTTP request cancellations do not abort queued work.
- The `Handler` holds hot-swappable clients under an `RWMutex`; `reloadClients()` is called on config save.
- Batch mode is available both via the `-batch` CLI flag and via `POST /batch/run` from the UI.
- The history cache is written to immediately after a successful classification (`cache.Append`) so later jobs in the same batch benefit without waiting for TTL expiry.

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
npm install       # install dependencies
npm start         # run the server (requires env vars set)
npm test          # run tests (node --test tests/)
```

There is no lint script configured. The project uses ES modules (`"type": "module"` in package.json).

## Architecture

This is a Node.js Express server that acts as a Firefly III webhook receiver. When Firefly III records a new withdrawal, it POSTs to `/webhook`. The app classifies the transaction via an LLM and writes the result back to Firefly III.

**Request flow:**

```
POST /webhook
  → App.js validates webhook payload (must be STORE_TRANSACTION, withdrawal, no existing category)
  → Creates a Job in JobList (in-memory, emits via Socket.IO to UI)
  → Pushes async work onto a serial Queue (concurrency: 1)
    → FireflyService.getCategories() — paginated fetch of all Firefly III categories
    → ClassifierService.classify() — sends to OpenAI with structured JSON output
    → FireflyService.updateTransaction() — writes category_id, tag, and notes back
```

**Three-outcome classification model** (`src/ClassifierService.js`):
- `CLASSIFIED` — confident match; sets category + `ai:classified` tag
- `ASSUMED` — best guess with conservative default (non-deductible over deductible, personal over business); sets category + `ai:assumed` tag + assumption in notes
- `NEEDS_REVIEW` — cannot classify; sets `ai:needs-review` tag, no category

**Key classes:**
- `src/App.js` — Express/Socket.IO setup, webhook handler, job queue management
- `src/ClassifierService.js` — OpenAI client, system prompt, JSON response parsing, category validation
- `src/FireflyService.js` — Firefly III API client (paginated category fetch, transaction update with tags/notes)
- `src/JobList.js` — in-memory job store with EventEmitter for real-time UI updates
- `src/util.js` — `getConfigVariable(name, default)` — throws on missing required env vars

**UI** (`public/`): optional real-time job monitor, served only when `ENABLE_UI=true`. Uses vanilla JS + Socket.IO client to render jobs as they arrive.

**CI** (`.github/workflows/main.yml`): builds and pushes a multi-arch Docker image to `ghcr.io` on pushes to `main` and version tags.

## Environment variables

| Variable | Required | Default | Notes |
|----------|----------|---------|-------|
| `FIREFLY_URL` | Yes | — | Trailing slash is stripped automatically |
| `FIREFLY_PERSONAL_TOKEN` | Yes | — | |
| `OPENAI_API_KEY` | Yes | — | |
| `OPENAI_MODEL` | No | `gpt-4o-mini` | Any OpenAI-compatible model |
| `OPENAI_BASE_URL` | No | — | For Ollama, Azure, etc. |
| `TAG_PREFIX` | No | `ai` | Produces `<prefix>:classified`, etc. |
| `ENABLE_UI` | No | `false` | Serve the job monitor at `/` |
| `PORT` | No | `3000` | |

## Important implementation details

- The job queue runs with `concurrency: 1` — categories are re-fetched from Firefly III for every job rather than cached, so changes to categories take effect immediately.
- `FireflyService.updateTransaction()` sets `fire_webhooks: false` to prevent re-triggering the webhook on the update.
- The classifier validates that the returned category name is an exact match against the list; if not, it falls back to `NEEDS_REVIEW`.
- `ClassifierService` uses `response_format: { type: "json_object" }` for structured output and `temperature: 0.1` for determinism.
- The webhook handler only processes withdrawals with no existing category — all other transaction types are rejected with a `400`.

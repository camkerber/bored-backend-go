# Project: bored-backend-go

Go HTTP API serving the [bored](https://github.com/camkerber/bored) portfolio frontend. A port of the TypeScript/Express service at `../bored-backend`, running as a long-lived container rather than a Vercel serverless function.

Go 1.27 · chi v5 · mongo-driver v2 · `log/slog` · testify. No ORM, no DI container, no validation framework — dependencies are constructed in `internal/server.New` and passed explicitly.

## Required Go skills

Always load `samber/cc-skills-golang@golang-how-to` at the start of any task in this repository. It routes to the other Go skills based on what the task involves.

## Commands

- `make run` — start the API server on :3000 (reads `.env`)
- `make test` / `make test-race` — test suite, optionally under the race detector
- `make lint` / `make fmt` — golangci-lint (v2 config in `.golangci.yml`)
- `make indexes` — create MongoDB indexes; the server also does this at boot
- `make vulncheck` — govulncheck over the module tree

## Layout

- [cmd/server/](cmd/server/) — entrypoint: config → mongo → router → serve → drain, plus the cleanup ticker.
- [cmd/ensureindexes/](cmd/ensureindexes/) — one-shot index creation for ops.
- [internal/httpx/](internal/httpx/) — the response envelope, error taxonomy, and middleware. **Highest-risk package**: the envelope is the entire contract with the React client, which rejects any shape it does not recognise.
- [internal/mongox/](internal/mongox/) — client lifecycle, collection names, index management.
- [internal/config/](internal/config/) — environment loading with fail-fast validation.
- Feature packages — `games`, `wordle`, `spotify`, `bingo`, `watcher`. Each owns its types, business logic, and handlers, and exposes `Routes(chi.Router)`.
- [internal/server/](internal/server/) — router assembly and the unenveloped `GET /` index.

## Conventions

- Handlers return errors through `httpx.Fail`, which maps them via `httpx.Classify`. Do not write status codes by hand outside `httpx`.
- Domain errors are `*httpx.HTTPError` built with `httpx.Errorf`, carrying the status and the machine-readable code the frontend switches on.
- Request validation is hand-written on the request struct as a `validate()` method.
- Slices that reach MongoDB or JSON must be non-nil, so they serialise as `[]` rather than `null`.
- Timestamps use the layout in `httpx.timestampLayout` — never `time.RFC3339Nano`.

## Contract constraints

This service must stay wire-compatible with `../bored-backend` so `VITE_API_URL` can be flipped either way. Before changing any response shape, status code, or error code, check whether the TypeScript service does the same thing. The behavioural details that are easy to break — timestamp format, key ordering on `GET /games`, the unenveloped `GET /`, 404-not-405, CORS semantics, empty request bodies — are documented in [README.md](README.md#behavioural-notes) and pinned by tests.

New functionality should be **additive** (as SSE is) rather than a change to an existing route.

## Watcher SSE

`GET /api/watcher/sessions/{id}/events` streams state over SSE. The hub in [internal/watcher/sse.go](internal/watcher/sse.go) is in-process, so it is only correct with a single instance — `fly.toml` pins `max_machines_running = 1`. Scaling out requires moving fan-out to a MongoDB change stream on `watcher_sessions` first.

## Testing

Unit tests cover pure logic; handler tests use `httptest` against a lazily-constructed Mongo client, which never dials, so routing, middleware, and validation are all testable without a database. Integration tests requiring a real database read `TEST_MONGODB_URI` and skip when it is unset — keep that pattern so a clean checkout passes.

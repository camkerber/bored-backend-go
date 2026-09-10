# bored-backend-go

Go rewrite of [bored-backend](https://github.com/camkerber/bored-backend), the API behind the [bored](https://github.com/camkerber/bored) portfolio frontend at [camkerber.dev](https://camkerber.dev).

Every route returns the same `{ success, message, data | error, timestamp }` envelope as the TypeScript service, so the two are interchangeable behind `VITE_API_URL`.

## Why

The TypeScript service ran as a single Vercel serverless function. Most of its structure existed to survive that: a memoised connect promise, `attachDatabasePool`, a `connectDatabase` middleware on every database-backed router, and two HTTP endpoints standing in for a scheduler.

Running as a long-lived container removes all of it:

| Serverless workaround | Replaced by |
| --- | --- |
| Memoised connect + `attachDatabasePool` | One pooled `*mongo.Client` held for the process lifetime |
| `connectDatabase` middleware per router | Nothing — the pool is already open |
| Two daily Vercel Cron endpoints | An in-process ticker (the endpoints remain for manual runs) |
| `process.exit(0)` on SIGTERM | `http.Server.Shutdown` draining in-flight requests |
| 5-second client polling | Server-Sent Events, with polling retained as fallback |

## Stack

Go 1.27 · [chi v5](https://github.com/go-chi/chi) · [mongo-driver v2](https://go.mongodb.org/mongo-driver) · `log/slog` · testify

No ORM, no DI container, no validation framework. Dependencies are constructed in `internal/server.New` and passed explicitly.

## Layout

```
cmd/server/              entrypoint: config -> mongo -> router -> serve -> drain
cmd/ensureindexes/       one-shot index creation for ops
internal/
  config/                environment loading and validation
  httpx/                 response envelope, error taxonomy, middleware
  mongox/                client lifecycle and index management
  games/  wordle/        read-only endpoints
  spotify/               pass-through proxy for the Spotify Web API
  bingo/                 shareable 5x5 boards
  watcher/               two-player matching sessions, SSE hub, participant auth
  server/                router assembly
```

## Running locally

```bash
cp .env.example .env      # then set BORED_MONGODB_URI
make run                  # http://localhost:3000
```

Point the frontend at it with `VITE_API_URL=http://localhost:3000 pnpm dev`.

```bash
make test        # unit + handler tests
make test-race   # under the race detector
make lint        # golangci-lint
make indexes     # create MongoDB indexes (the server also does this at boot)
```

## Configuration

All configuration is environment-based; see [.env.example](.env.example). `BORED_MONGODB_URI` is required and the server refuses to start without it. The database name (`boring-db`) is fixed in code.

`SPOTIFY_CLIENT_ID` is **not** required. The TypeScript service only set it to satisfy the Spotify SDK constructor — it was never sent upstream. This service forwards the caller's own access token and drops the dependency.

## Endpoints

All under `/api`. `GET /` returns an unenveloped self-describing index; `GET /healthz` is the container health check.

**Games / Wordle** — `GET /games`, `GET /game/{id}`, `GET /wordle-dictionary`

**Spotify** (requires `Authorization: Bearer <spotify_user_token>`) — `GET /spotify/me/top/artists`, `GET /spotify/me/top/tracks`, both taking `time_range`, `limit` (1–50), `offset` (0–49).

**Bingo** (no auth; the minted `userId` is the capability) — `POST /bingo`, `GET /bingo/{boardId}` (mints a viewer), `GET /bingo/{boardId}/user/{userId}`, `PUT /bingo/{boardId}/user/{userId}/mark/{index}`, `GET /bingo/cron/cleanup`.

**Watcher** (per-session routes require `x-participant-token`) — `POST /watcher/sessions`, `POST /watcher/sessions/{code}/join`, `GET|PUT /watcher/sessions/{id}/entries`, `GET /watcher/sessions/{id}`, `POST .../ready`, `POST .../swipes`, `GET .../matches`, `POST .../rematch`, `GET /watcher/cron/cleanup`, and **`GET /watcher/sessions/{id}/events`** (SSE, new).

### Server-Sent Events

`GET /api/watcher/sessions/{id}/events` streams `PublicSessionState` on every transition. `EventSource` cannot set headers, so clients read it with `fetch` and parse the stream, keeping the token in `x-participant-token`.

The polling endpoint is unchanged and remains the fallback.

> The SSE hub is in-process, which is only correct with a single instance — `fly.toml` pins the machine count to 1. Scaling out requires moving fan-out to a MongoDB change stream on `watcher_sessions`.

## Behavioural notes

Details that are easy to break and are covered by tests:

- **Timestamps** use `2006-01-02T15:04:05.000Z07:00`, not `time.RFC3339Nano`, which trims trailing zeros and would diverge from JavaScript's `toISOString()`.
- **`GET /` is not enveloped**, unlike every other route.
- **`GET /games` preserves document key order** by decoding raw BSON. A `bson.M` would randomise the array.
- **An unmatched method returns 404**, not 405, matching Express.
- **A disallowed CORS origin still gets a normal response**, just without CORS headers.
- **`x-participant-token` is named explicitly** in the CORS allowlist. The Node `cors` package reflected it automatically; miss it and every watcher route fails in a browser while working in curl.
- **Empty request bodies are valid** on `POST .../ready`, `POST .../join`, and `PUT .../mark/{index}`, which the frontend sends with `Content-Type: application/json` and no body.

Deliberate divergences from the TypeScript service:

- Board shuffling uses `crypto/rand` rather than `Math.random()`.
- `CRON_SECRET` is compared in constant time.
- A 12-character board or session id is now a `400` rather than reaching the database, because Go's `ObjectIDFromHex` accepts only 24-char hex where JavaScript's `ObjectId.isValid()` also accepted any 12-character string.

## Deployment

Fly.io, via the multi-stage [Dockerfile](Dockerfile) (static binary on `distroless/static`). The image is host-portable if you'd rather use Railway or Render.

```bash
fly secrets set BORED_MONGODB_URI=... CRON_SECRET=... CORS_ORIGINS=...
fly deploy
```

Expiry is handled three ways on purpose: a MongoDB TTL index, application-level `expiresAt` checks, and the sweep. The TTL monitor only runs about once a minute, so the application checks are load-bearing.

## Migration status

Both services run against the same Atlas cluster, so cutover is a `VITE_API_URL` change and rollback is the same change in reverse. Contract parity should be confirmed by diffing responses against the deployed TypeScript app before the flip.

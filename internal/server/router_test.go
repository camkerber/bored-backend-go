package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/camkerber/bored-backend-go/internal/mongox"
)

// TestMain silences request logging, which would otherwise bury assertion
// failures in per-request noise.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// newTestApp builds the router against a lazily-constructed Mongo client. The
// driver does not dial until an operation runs, so routing, middleware, and
// validation are all exercisable without a live database. Handlers that do
// reach the database fail fast thanks to the short server-selection timeout.
func newTestApp(t *testing.T) *App {
	t.Helper()

	client, err := mongo.Connect(options.Client().
		ApplyURI("mongodb://127.0.0.1:27017").
		SetServerSelectionTimeout(100 * time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(t.Context()) })

	return New(Options{
		DB:         &mongox.DB{Client: client, Database: client.Database("test")},
		CronSecret: "s3cret",
	})
}

func do(t *testing.T, app *App, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// TestCronRouteBeatsBoardIDWildcard pins the one route-ordering dependency
// carried over from Express.
//
// There, /bingo/cron/cleanup only worked because it was registered before
// /bingo/:boardId. chi resolves this by trie precedence instead, so the
// dependency silently disappears - and would silently come back as a 400
// INVALID_BOARD_ID if a future router change regressed it.
func TestCronRouteBeatsBoardIDWildcard(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodGet, "/api/bingo/cron/cleanup", nil)

	// Reaching cronAuth (401) proves the request did not fall through to the
	// {boardId} handler, which would have rejected "cron" as an invalid id.
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	body := decodeError(t, rec)
	assert.Equal(t, "UNAUTHORIZED_CRON", body["error"].(map[string]any)["code"])
}

func TestWatcherCronRouteIsReachable(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodGet, "/api/watcher/cron/cleanup", nil)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	body := decodeError(t, rec)
	assert.Equal(t, "UNAUTHORIZED_CRON", body["error"].(map[string]any)["code"])
}

func TestCronAuthAcceptsTheConfiguredSecret(t *testing.T) {
	app := newTestApp(t)

	// A correct secret passes the middleware; the handler then fails on the
	// unreachable database, which is a 500 rather than a 401.
	rec := do(t, app, http.MethodGet, "/api/bingo/cron/cleanup",
		map[string]string{"Authorization": "Bearer s3cret"})

	assert.NotEqual(t, http.StatusUnauthorized, rec.Code)
}

func TestNotFoundUsesTheErrorEnvelope(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodGet, "/api/definitely-not-a-route", nil)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	body := decodeError(t, rec)
	assert.Equal(t, false, body["success"])
	assert.Equal(t, "Route GET /api/definitely-not-a-route not found", body["message"])
	assert.Equal(t, "NOT_FOUND", body["error"].(map[string]any)["code"])
}

// TestUnmatchedMethodReturns404 preserves Express behaviour: it had no 405, so
// a wrong method fell through to the same catch-all.
func TestUnmatchedMethodReturns404(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodDelete, "/api/games", nil)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "NOT_FOUND", decodeError(t, rec)["error"].(map[string]any)["code"])
}

// TestIndexIsNotEnveloped guards a deliberate inconsistency: GET / returns a
// bare object, unlike every other route.
func TestIndexIsNotEnveloped(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodGet, "/", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "bored-backend", body["name"])
	assert.NotContains(t, body, "success", "GET / must stay unenveloped")
	assert.NotContains(t, body, "data")
}

func TestHealthz(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodGet, "/healthz", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

// TestCORSPreflightAllowsParticipantToken covers the failure mode that only
// appears in a browser: the Node cors package reflected requested headers
// automatically, so x-participant-token never had to be named. chi's cors does
// not, and without it every watcher route fails preflight while curl succeeds.
func TestCORSPreflightAllowsParticipantToken(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodOptions, "/api/watcher/sessions/abc", map[string]string{
		"Origin":                         "https://camkerber.dev",
		"Access-Control-Request-Method":  http.MethodGet,
		"Access-Control-Request-Headers": "x-participant-token",
	})

	assert.Equal(t, "https://camkerber.dev", rec.Header().Get("Access-Control-Allow-Origin"))
	// Header names are case-insensitive on the wire; the middleware echoes the
	// canonical form.
	assert.Contains(t,
		strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers")),
		"x-participant-token")
}

func TestCORSAllowedOrigins(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		allowed bool
	}{
		{"production frontend", "https://camkerber.dev", true},
		{"vite dev server", "http://localhost:5173", true},
		{"vite dev server via loopback ip", "http://127.0.0.1:5173", true},
		{"vercel preview", "https://bored-abc123.vercel.app", true},
		{"unrelated origin", "https://evil.example.com", false},
		{"lookalike subdomain", "https://camkerber.dev.evil.com", false},
		{"vercel lookalike", "https://foo.vercel.app.evil.com", false},
	}

	app := newTestApp(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, app, http.MethodGet, "/healthz",
				map[string]string{"Origin": tc.origin})

			got := rec.Header().Get("Access-Control-Allow-Origin")
			if tc.allowed {
				assert.Equal(t, tc.origin, got)
			} else {
				assert.Empty(t, got)
			}
		})
	}
}

// TestCORSDeniedOriginStillServesTheRequest matches the previous behaviour: a
// rejected origin got a normal response with no CORS headers, not an error.
func TestCORSDeniedOriginStillServesTheRequest(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodGet, "/healthz",
		map[string]string{"Origin": "https://evil.example.com"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

// TestRequestWithNoOriginIsAllowed covers server-to-server and curl callers.
func TestRequestWithNoOriginIsAllowed(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodGet, "/healthz", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestWatcherRoutesRequireParticipantToken checks the first rung of the auth
// ladder, whose ordering is observable.
func TestWatcherRoutesRequireParticipantToken(t *testing.T) {
	app := newTestApp(t)

	for _, target := range []string{
		"/api/watcher/sessions/000000000000000000000000",
		"/api/watcher/sessions/000000000000000000000000/entries",
		"/api/watcher/sessions/000000000000000000000000/matches",
		"/api/watcher/sessions/000000000000000000000000/events",
	} {
		t.Run(target, func(t *testing.T) {
			rec := do(t, app, http.MethodGet, target, nil)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, "MISSING_PARTICIPANT_TOKEN",
				decodeError(t, rec)["error"].(map[string]any)["code"])
		})
	}
}

// TestInvalidSessionIDIsReportedBeforeLookup pins the ladder's second rung: a
// malformed id must produce INVALID_SESSION_ID, not SESSION_NOT_FOUND.
func TestInvalidSessionIDIsReportedBeforeLookup(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodGet, "/api/watcher/sessions/not-an-objectid",
		map[string]string{"x-participant-token": "whatever"})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "INVALID_SESSION_ID",
		decodeError(t, rec)["error"].(map[string]any)["code"])
}

func TestInvalidGameIDIsRejectedBeforeTheDatabase(t *testing.T) {
	app := newTestApp(t)

	for _, id := range []string{"has%20spaces", "_id", "with.dot", "$where",
		strings.Repeat("a", 65)} {
		t.Run(id, func(t *testing.T) {
			rec := do(t, app, http.MethodGet, "/api/game/"+id, nil)

			// 400 rather than a 500 from an unreachable database proves the
			// guard ran first.
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "INVALID_GAME_ID",
				decodeError(t, rec)["error"].(map[string]any)["code"])
		})
	}
}

// TestOutOfRangeMarkIndexIsAValidationError is a regression test.
//
// The previous implementation bounded :index in its request schema, so an
// out-of-range value surfaced as VALIDATION_ERROR. Letting it fall through to
// the service instead produced INVALID_MARK_INDEX, which a live diff caught.
func TestOutOfRangeMarkIndexIsAValidationError(t *testing.T) {
	app := newTestApp(t)
	board := "507f1f77bcf86cd799439011"
	user := "507f1f77bcf86cd799439012"

	for _, index := range []string{"25", "-1", "abc", "999"} {
		t.Run(index, func(t *testing.T) {
			rec := do(t, app, http.MethodPut,
				"/api/bingo/"+board+"/user/"+user+"/mark/"+index, nil)

			// 400 rather than a 500 from the unreachable database also proves
			// the check runs before any query.
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "VALIDATION_ERROR",
				decodeError(t, rec)["error"].(map[string]any)["code"])
		})
	}
}

// TestInRangeMarkIndexReachesTheService is the counterpart: a valid index must
// not be rejected by the bounds check.
func TestInRangeMarkIndexReachesTheService(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodPut,
		"/api/bingo/507f1f77bcf86cd799439011/user/507f1f77bcf86cd799439012/mark/24", nil)

	assert.NotEqual(t, http.StatusBadRequest, rec.Code,
		"index 24 is in range and must reach the service")
}

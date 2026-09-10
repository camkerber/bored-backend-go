package spotify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upstream stands in for the Spotify Web API.
type upstream struct {
	status      int
	body        string
	retryAfter  string
	lastQuery   url.Values
	lastAuthHdr string
}

func newTestHandler(t *testing.T, up *upstream) (http.Handler, *upstream) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.lastQuery = r.URL.Query()
		up.lastAuthHdr = r.Header.Get("Authorization")
		if up.retryAfter != "" {
			w.Header().Set("Retry-After", up.retryAfter)
		}
		w.WriteHeader(up.status)
		_, _ = w.Write([]byte(up.body))
	}))
	t.Cleanup(server.Close)

	r := chi.NewRouter()
	NewHandler(NewClient(server.URL)).Routes(r)
	return r, up
}

func get(t *testing.T, h http.Handler, target, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok, "response has no error object: %s", rec.Body.String())
	code, _ := errObj["code"].(string)
	return code
}

// TestTopItemsRelaysTheUpstreamPageVerbatim is the core proxy guarantee: the
// frontend consumes Spotify's own page shape, so the body must survive
// unchanged inside the envelope.
func TestTopItemsRelaysTheUpstreamPageVerbatim(t *testing.T) {
	page := `{"items":[{"id":"abc","name":"Artist"}],"total":1,"limit":20,"offset":0}`
	h, up := newTestHandler(t, &upstream{status: http.StatusOK, body: page})

	rec := get(t, h, "/spotify/me/top/artists", "Bearer user-token")

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.JSONEq(t, page, string(body["data"]))
	assert.Equal(t, "Bearer user-token", up.lastAuthHdr,
		"the caller's token must be forwarded unchanged")
}

func TestTopItemsAppliesQueryDefaults(t *testing.T) {
	h, up := newTestHandler(t, &upstream{status: http.StatusOK, body: `{}`})

	get(t, h, "/spotify/me/top/tracks", "Bearer t")

	assert.Equal(t, "medium_term", up.lastQuery.Get("time_range"))
	assert.Equal(t, "20", up.lastQuery.Get("limit"))
	assert.Equal(t, "0", up.lastQuery.Get("offset"))
}

func TestTopItemsForwardsExplicitQuery(t *testing.T) {
	h, up := newTestHandler(t, &upstream{status: http.StatusOK, body: `{}`})

	get(t, h, "/spotify/me/top/artists?time_range=long_term&limit=50&offset=49", "Bearer t")

	assert.Equal(t, "long_term", up.lastQuery.Get("time_range"))
	assert.Equal(t, "50", up.lastQuery.Get("limit"))
	assert.Equal(t, "49", up.lastQuery.Get("offset"))
}

func TestMissingOrMalformedAuthorization(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{name: "absent", header: ""},
		{name: "not bearer", header: "Basic abc123"},
		{name: "bearer with no token", header: "Bearer"},
		{name: "bearer with empty token", header: "Bearer "},
	}

	h, _ := newTestHandler(t, &upstream{status: http.StatusOK, body: `{}`})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, h, "/spotify/me/top/artists", tc.header)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, "MISSING_ACCESS_TOKEN", errorCode(t, rec))
		})
	}
}

// TestBearerSchemeIsCaseInsensitive matches the /^Bearer\s+(.+)$/i regex the
// TypeScript controller used.
func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	h, up := newTestHandler(t, &upstream{status: http.StatusOK, body: `{}`})

	rec := get(t, h, "/spotify/me/top/artists", "bEaReR mytoken")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Bearer mytoken", up.lastAuthHdr)
}

func TestQueryValidationErrorCodes(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		wantCode string
	}{
		{name: "bad time range", query: "?time_range=yesterday", wantCode: "INVALID_TIME_RANGE"},
		{name: "limit below minimum", query: "?limit=0", wantCode: "INVALID_LIMIT"},
		{name: "limit above maximum", query: "?limit=51", wantCode: "INVALID_LIMIT"},
		{name: "non-numeric limit", query: "?limit=lots", wantCode: "INVALID_LIMIT"},
		{name: "negative offset", query: "?offset=-1", wantCode: "INVALID_OFFSET"},
		{name: "offset above maximum", query: "?offset=50", wantCode: "INVALID_OFFSET"},
		{name: "non-numeric offset", query: "?offset=far", wantCode: "INVALID_OFFSET"},
	}

	h, _ := newTestHandler(t, &upstream{status: http.StatusOK, body: `{}`})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, h, "/spotify/me/top/artists"+tc.query, "Bearer t")

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, tc.wantCode, errorCode(t, rec))
		})
	}
}

func TestUpstreamErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantStatus int
		wantCode   string
	}{
		{name: "expired token", status: http.StatusUnauthorized,
			wantStatus: http.StatusUnauthorized, wantCode: "SPOTIFY_TOKEN_INVALID"},
		{name: "app not in extended quota mode", status: http.StatusForbidden,
			wantStatus: http.StatusForbidden, wantCode: "SPOTIFY_FORBIDDEN"},
		{name: "rate limited", status: http.StatusTooManyRequests,
			wantStatus: http.StatusTooManyRequests, wantCode: "SPOTIFY_RATE_LIMITED"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t, &upstream{status: tc.status, body: `{"error":"nope"}`})

			rec := get(t, h, "/spotify/me/top/artists", "Bearer t")

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantCode, errorCode(t, rec))
		})
	}
}

// TestRetryAfterIsForwarded matters because the client is expected to honour
// it; dropping the header turns a recoverable 429 into a retry storm.
func TestRetryAfterIsForwarded(t *testing.T) {
	h, _ := newTestHandler(t, &upstream{
		status:     http.StatusTooManyRequests,
		body:       `{"error":"slow down"}`,
		retryAfter: "30",
	})

	rec := get(t, h, "/spotify/me/top/artists", "Bearer t")

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "30", rec.Header().Get("Retry-After"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	details := body["error"].(map[string]any)["details"].(map[string]any)
	assert.EqualValues(t, 30, details["retryAfterSeconds"])
}

// TestUnexpectedUpstreamStatusIsScrubbed: a 500 from Spotify must not leak
// upstream detail to the browser.
func TestUnexpectedUpstreamStatusIsScrubbed(t *testing.T) {
	h, _ := newTestHandler(t, &upstream{
		status: http.StatusInternalServerError,
		body:   `{"internal":"stack trace with secrets"}`,
	})

	rec := get(t, h, "/spotify/me/top/artists", "Bearer t")

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "INTERNAL_ERROR", errorCode(t, rec))
	assert.NotContains(t, rec.Body.String(), "stack trace with secrets")
}

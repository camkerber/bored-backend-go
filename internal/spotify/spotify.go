// Package spotify proxies the authenticated user's top artists and tracks.
//
// There is no OAuth here. The browser performs the authorization-code flow and
// sends its own user access token on every request; this service forwards it
// and relays the response. Nothing is stored, refreshed, or exchanged, and no
// client secret is involved.
package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/camkerber/bored-backend-go/internal/httpx"
)

// DefaultBaseURL is Spotify's Web API root. Tests override it.
const DefaultBaseURL = "https://api.spotify.com/v1"

// maxUpstreamBody caps how much of a Spotify response is buffered.
const maxUpstreamBody = 8 << 20

// Query defaults and bounds, matching the previous zod schema.
const (
	defaultTimeRange = "medium_term"
	defaultLimit     = 20
	minLimit         = 1
	maxLimit         = 50
	defaultOffset    = 0
	minOffset        = 0
	maxOffset        = 49
)

var validTimeRanges = map[string]struct{}{
	"short_term":  {},
	"medium_term": {},
	"long_term":   {},
}

// apiError carries an upstream failure with the detail the client needs.
type apiError struct {
	status            int
	retryAfterSeconds int
	hasRetryAfter     bool
	body              string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("spotify request failed with status %d", e.status)
}

// Client talks to the Spotify Web API on behalf of a caller's token.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a Client. An empty baseURL uses DefaultBaseURL.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// topItems fetches one page of the user's top artists or tracks.
//
// The response body is relayed verbatim as raw JSON rather than being decoded
// into Go structs. The frontend consumes Spotify's own page shape, so modelling
// it here would add a second place to keep in sync for no benefit.
func (c *Client) topItems(
	ctx context.Context, token, itemType string, q topItemsQuery,
) (json.RawMessage, error) {
	endpoint := c.baseURL + "/me/top/" + itemType + "?" + url.Values{
		"time_range": {q.timeRange},
		"limit":      {strconv.Itoa(q.limit)},
		"offset":     {strconv.Itoa(q.offset)},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call spotify: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxUpstreamBody))
	if err != nil {
		return nil, fmt.Errorf("read spotify response: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		upstream := &apiError{status: res.StatusCode, body: string(body)}
		if raw := res.Header.Get("Retry-After"); raw != "" {
			if seconds, convErr := strconv.Atoi(raw); convErr == nil {
				upstream.retryAfterSeconds = seconds
				upstream.hasRetryAfter = true
			}
		}
		return nil, upstream
	}

	return json.RawMessage(body), nil
}

// topItemsQuery is the validated query string.
type topItemsQuery struct {
	timeRange string
	limit     int
	offset    int
}

// parseTopItemsQuery validates the query string, emitting the same per-field
// error codes the TypeScript controller hand-mapped from zod issues.
func parseTopItemsQuery(values url.Values) (topItemsQuery, error) {
	q := topItemsQuery{
		timeRange: defaultTimeRange,
		limit:     defaultLimit,
		offset:    defaultOffset,
	}

	if raw := values.Get("time_range"); raw != "" {
		if _, ok := validTimeRanges[raw]; !ok {
			return q, httpx.Errorf(http.StatusBadRequest, "INVALID_TIME_RANGE",
				"Invalid time_range. Must be one of: short_term, medium_term, long_term.")
		}
		q.timeRange = raw
	}

	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < minLimit || limit > maxLimit {
			return q, httpx.Errorf(http.StatusBadRequest, "INVALID_LIMIT",
				"Invalid limit. Must be an integer between %d and %d.", minLimit, maxLimit)
		}
		q.limit = limit
	}

	if raw := values.Get("offset"); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < minOffset || offset > maxOffset {
			return q, httpx.Errorf(http.StatusBadRequest, "INVALID_OFFSET",
				"Invalid offset. Must be an integer between %d and %d.", minOffset, maxOffset)
		}
		q.offset = offset
	}

	return q, nil
}

// Handler serves the Spotify proxy routes.
type Handler struct {
	client *Client
}

// NewHandler builds a Handler over client.
func NewHandler(client *Client) *Handler {
	return &Handler{client: client}
}

// Routes registers the Spotify endpoints on r. These routes touch no database.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/spotify/me/top/artists", h.topItems("artists"))
	r.Get("/spotify/me/top/tracks", h.topItems("tracks"))
}

// topItems builds the handler for one item type. The TypeScript version
// duplicated this body, including ~50 lines of error mapping, once per type.
func (h *Handler) topItems(itemType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := httpx.BearerToken(r)
		if token == "" {
			httpx.Fail(w, httpx.Errorf(http.StatusUnauthorized, "MISSING_ACCESS_TOKEN",
				"Missing or malformed Authorization header. "+
					"Expected 'Authorization: Bearer <spotify_access_token>'."))
			return
		}

		query, err := parseTopItemsQuery(r.URL.Query())
		if err != nil {
			httpx.Fail(w, err)
			return
		}

		page, err := h.client.topItems(r.Context(), token, itemType, query)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		httpx.OK(w, page)
	}
}

// writeUpstreamError translates a Spotify failure into the client-facing
// envelope, preserving the exact codes and messages clients already handle.
func writeUpstreamError(w http.ResponseWriter, err error) {
	var upstream *apiError
	if !errors.As(err, &upstream) {
		httpx.Fail(w, err)
		return
	}

	// An empty upstream body is omitted rather than sent as "".
	var details any
	if upstream.body != "" {
		details = upstream.body
	}

	switch upstream.status {
	case http.StatusUnauthorized:
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.NewError(
			"Spotify rejected the access token. Re-authenticate the user.",
			"SPOTIFY_TOKEN_INVALID", details))
	case http.StatusForbidden:
		httpx.WriteJSON(w, http.StatusForbidden, httpx.NewError(
			"Spotify denied the request. If this app is in Development Mode, "+
				"your Spotify account must be added to the app's User Management "+
				"allowlist in the Spotify Developer Dashboard. Alternatively, the "+
				"app may need Extended Quota Mode to serve all users.",
			"SPOTIFY_FORBIDDEN", details))
	case http.StatusTooManyRequests:
		if upstream.hasRetryAfter {
			w.Header().Set("Retry-After", strconv.Itoa(upstream.retryAfterSeconds))
		}
		rateDetails := map[string]any{"body": upstream.body}
		if upstream.hasRetryAfter {
			rateDetails["retryAfterSeconds"] = upstream.retryAfterSeconds
		}
		httpx.WriteJSON(w, http.StatusTooManyRequests, httpx.NewError(
			"Spotify rate limit exceeded. Honor the Retry-After header before retrying.",
			"SPOTIFY_RATE_LIMITED", rateDetails))
	default:
		// Anything else surfaces as a generic internal error, as before.
		httpx.Fail(w, errors.New("spotify upstream failure"))
	}
}

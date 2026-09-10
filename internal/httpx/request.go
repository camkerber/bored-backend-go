package httpx

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// maxBodyBytes matches the 1mb limit express.json() enforced.
const maxBodyBytes = 1 << 20

// bearerRE extracts a bearer token, case-insensitively, as the TypeScript
// handlers did with /^Bearer\s+(.+)$/i.
var bearerRE = regexp.MustCompile(`(?i)^Bearer\s+(.+)$`)

// BearerToken returns the token from the Authorization header, or "" if the
// header is missing or malformed.
func BearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	match := bearerRE.FindStringSubmatch(header)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// DecodeJSON reads a JSON body into v.
//
// An absent or empty body is not an error. The frontend sends several requests
// with Content-Type: application/json and no body at all (POST .../ready,
// POST .../join) or a bare {} (PUT .../mark/{index}); express.json() tolerated
// that by leaving req.body as {}, and handlers relied on it.
func DecodeJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return &HTTPError{
				Status:  http.StatusRequestEntityTooLarge,
				Code:    "PAYLOAD_TOO_LARGE",
				Message: "Request body is too large",
			}
		}
		return &ValidationError{Issues: []ValidationIssue{
			{Path: "body", Message: "could not be read"},
		}}
	}

	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}

	if err := json.Unmarshal(body, v); err != nil {
		return &ValidationError{Issues: []ValidationIssue{
			{Path: "body", Message: "must be valid JSON"},
		}}
	}
	return nil
}

// CronAuth gates the cleanup endpoints on Authorization: Bearer <CRON_SECRET>.
//
// The comparison is constant-time. The TypeScript version used a plain !==
// here, unlike its participant-token path which was already timing-safe.
func CronAuth(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				WriteJSON(w, http.StatusUnauthorized, NewError(
					"Cron endpoint is not configured", "CRON_NOT_CONFIGURED", nil))
				return
			}
			token := BearerToken(r)
			if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
				WriteJSON(w, http.StatusUnauthorized, NewError(
					"Unauthorized", "UNAUTHORIZED_CRON", nil))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

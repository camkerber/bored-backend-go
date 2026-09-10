package httpx

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// baselineOrigins are always allowed, matching BASELINE_ORIGINS in the
// TypeScript app.
var baselineOrigins = []string{
	"https://camkerber.dev",
	"http://localhost:5173",
	"http://127.0.0.1:5173",
}

// vercelPreviewRE allows any Vercel preview deployment of the frontend.
var vercelPreviewRE = regexp.MustCompile(`^https://[a-z0-9-]+\.vercel\.app$`)

// CORS builds the CORS middleware. extraOrigins is the parsed CORS_ORIGINS
// value, unioned with the baseline list.
//
// Two behaviours are load-bearing and easy to get wrong:
//
//   - A request with no Origin header is allowed (server-to-server, curl).
//   - A disallowed origin does not fail the request; it simply receives no CORS
//     headers, and the browser blocks it. Returning an error here would change
//     observable status codes.
//
// x-participant-token must be listed explicitly. The Node cors package
// reflected Access-Control-Request-Headers automatically, so the TypeScript
// version never had to name it; omitting it here breaks every watcher route in
// the browser while leaving curl working.
func CORS(extraOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(baselineOrigins)+len(extraOrigins))
	for _, o := range baselineOrigins {
		allowed[o] = struct{}{}
	}
	for _, o := range extraOrigins {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = struct{}{}
		}
	}

	return cors.Handler(cors.Options{
		AllowOriginFunc: func(_ *http.Request, origin string) bool {
			if origin == "" {
				return true
			}
			if _, ok := allowed[origin]; ok {
				return true
			}
			return vercelPreviewRE.MatchString(origin)
		},
		AllowedMethods: []string{
			http.MethodGet, http.MethodPost, http.MethodPut,
			http.MethodDelete, http.MethodOptions,
		},
		AllowedHeaders: []string{
			"Accept", "Authorization", "Content-Type", "Cache-Control",
			"Last-Event-ID", "x-participant-token",
		},
		ExposedHeaders: []string{"Retry-After"},
		MaxAge:         300,
	})
}

// RequestLogger replaces the console.log line emitted by the TypeScript
// requestLogger with a structured slog record.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// Recoverer converts a panic into the standard 500 envelope. Express caught
// thrown errors on its own; in Go an unrecovered panic in a handler goroutine
// tears down the whole process, so this is required rather than defensive.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// ErrAbortHandler is the caller signalling a deliberate abort;
				// it must keep propagating rather than becoming a 500.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				slog.Error("panic recovered",
					"method", r.Method, "path", r.URL.Path, "panic", rec)
				WriteJSON(w, http.StatusInternalServerError,
					NewError("Internal server error", "INTERNAL_ERROR", nil))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// NotFound reproduces the Express catch-all: "Route GET /api/nope not found".
// It is wired to both chi's NotFound and MethodNotAllowed hooks because Express
// had no 405 - an unmatched method fell through to the same handler.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusNotFound, NewError(
		"Route "+r.Method+" "+r.URL.RequestURI()+" not found", "NOT_FOUND", nil))
}

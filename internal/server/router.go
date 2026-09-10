// Package server wires the feature packages into a single HTTP handler.
package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/camkerber/bored-backend-go/internal/bingo"
	"github.com/camkerber/bored-backend-go/internal/games"
	"github.com/camkerber/bored-backend-go/internal/httpx"
	"github.com/camkerber/bored-backend-go/internal/mongox"
	"github.com/camkerber/bored-backend-go/internal/spotify"
	"github.com/camkerber/bored-backend-go/internal/watcher"
	"github.com/camkerber/bored-backend-go/internal/wordle"
)

// Options configures the router.
type Options struct {
	DB *mongox.DB
	// CORSOrigins are extra allowed origins beyond the baseline list.
	CORSOrigins []string
	// CronSecret gates the cleanup routes.
	CronSecret string
	// SpotifyBaseURL overrides the Spotify API root; empty uses the default.
	SpotifyBaseURL string
}

// App bundles the router with the long-lived services the process also needs
// (the cleanup sweep calls the same services the handlers do).
type App struct {
	Handler http.Handler
	Watcher *watcher.Service
	Bingo   *bingo.Service
	Hub     *watcher.Hub
}

// New builds the application. Dependencies are constructed here and passed
// explicitly; there is no container.
func New(opts Options) *App {
	hub := watcher.NewHub()
	watcherSvc := watcher.NewService(opts.DB, hub)
	bingoSvc := bingo.NewService(opts.DB)

	gamesHandler := games.New(opts.DB)
	wordleHandler := wordle.New(opts.DB)
	spotifyHandler := spotify.NewHandler(spotify.NewClient(opts.SpotifyBaseURL))
	watcherHandler := watcher.NewHandler(watcherSvc, hub)
	bingoHandler := bingo.NewHandler(bingoSvc)

	cronAuth := httpx.CronAuth(opts.CronSecret)

	r := chi.NewRouter()
	r.Use(httpx.Recoverer)
	r.Use(httpx.RequestLogger)
	r.Use(httpx.CORS(opts.CORSOrigins))

	// Express had no 405: an unmatched method fell through to the same 404
	// catch-all, so both hooks point at the same handler.
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.NotFound)

	// Liveness probe for the container platform. Not part of the API contract.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	r.Get("/", serveIndex)

	r.Route("/api", func(api chi.Router) {
		gamesHandler.Routes(api)
		wordleHandler.Routes(api)
		spotifyHandler.Routes(api)
		watcherHandler.Routes(api, cronAuth)
		bingoHandler.Routes(api, cronAuth)
	})

	return &App{Handler: r, Watcher: watcherSvc, Bingo: bingoSvc, Hub: hub}
}

// indexResponse is the self-describing endpoint listing served at GET /.
//
// Deliberately NOT wrapped in the success envelope - the previous
// implementation returned a bare object here and something may be scraping it.
// A struct rather than a map so key order is stable across responses.
type indexResponse struct {
	Name      string        `json:"name"`
	Endpoints indexEndpoint `json:"endpoints"`
}

type indexEndpoint struct {
	WordleDictionary      string `json:"wordleDictionary"`
	AllGames              string `json:"allGames"`
	GameByID              string `json:"gameById"`
	SpotifyMyTopArtists   string `json:"spotifyMyTopArtists"`
	SpotifyMyTopTracks    string `json:"spotifyMyTopTracks"`
	WatcherCreateSession  string `json:"watcherCreateSession"`
	WatcherJoinSession    string `json:"watcherJoinSession"`
	WatcherSessionState   string `json:"watcherSessionState"`
	WatcherSessionEvents  string `json:"watcherSessionEvents"`
	WatcherSessionEntries string `json:"watcherSessionEntries"`
	WatcherSessionReady   string `json:"watcherSessionReady"`
	WatcherSessionSwipes  string `json:"watcherSessionSwipes"`
	WatcherSessionMatches string `json:"watcherSessionMatches"`
	WatcherSessionRematch string `json:"watcherSessionRematch"`
	BingoCreateBoard      string `json:"bingoCreateBoard"`
	BingoMintUserBoard    string `json:"bingoMintUserBoard"`
	BingoUserBoard        string `json:"bingoUserBoard"`
	BingoToggleMark       string `json:"bingoToggleMark"`
}

func serveIndex(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, indexResponse{
		Name: "bored-backend",
		Endpoints: indexEndpoint{
			WordleDictionary: "GET /api/wordle-dictionary",
			AllGames:         "GET /api/games",
			GameByID:         "GET /api/game/:id",
			SpotifyMyTopArtists: "GET /api/spotify/me/top/artists?" +
				"time_range=short_term|medium_term|long_term&limit=1..50&offset=0..49",
			SpotifyMyTopTracks: "GET /api/spotify/me/top/tracks?" +
				"time_range=short_term|medium_term|long_term&limit=1..50&offset=0..49",
			WatcherCreateSession: "POST /api/watcher/sessions",
			WatcherJoinSession:   "POST /api/watcher/sessions/:code/join",
			WatcherSessionState:  "GET /api/watcher/sessions/:id",
			// New in the Go implementation; the polling route above is retained.
			WatcherSessionEvents:  "GET /api/watcher/sessions/:id/events (SSE)",
			WatcherSessionEntries: "PUT|GET /api/watcher/sessions/:id/entries",
			WatcherSessionReady:   "POST /api/watcher/sessions/:id/ready",
			WatcherSessionSwipes:  "POST /api/watcher/sessions/:id/swipes",
			WatcherSessionMatches: "GET /api/watcher/sessions/:id/matches",
			WatcherSessionRematch: "POST /api/watcher/sessions/:id/rematch",
			BingoCreateBoard:      "POST /api/bingo",
			BingoMintUserBoard:    "GET /api/bingo/:boardId",
			BingoUserBoard:        "GET /api/bingo/:boardId/user/:userId",
			BingoToggleMark:       "PUT /api/bingo/:boardId/user/:userId/mark/:index",
		},
	})
}

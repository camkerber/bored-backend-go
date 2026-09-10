// Package games serves the Connections ("Camnections") puzzles.
//
// The camnections collection holds a single document whose top-level keys are
// game ids: {_id, "some-game": {...}, "another": {...}}. Games are keys on one
// document, not documents in their own right.
package games

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/camkerber/bored-backend-go/internal/httpx"
	"github.com/camkerber/bored-backend-go/internal/mongox"
)

// gameIDRE mirrors the zod schema on :id. It also keeps "$" and "." out of the
// projection key, so a hostile id cannot become an operator or a dotted path.
var gameIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Connection is one of the four category groups in a puzzle.
type Connection struct {
	Category    string   `json:"category"    bson:"category"`
	Description string   `json:"description" bson:"description"`
	Options     []string `json:"options"     bson:"options"`
}

// Game is a single Connections puzzle.
type Game struct {
	ID    string `json:"id"               bson:"id"`
	Title string `json:"title"            bson:"title"`
	// Pointer, not string: some stored games carry author:"" and the previous
	// implementation emitted it. A plain string with omitempty would drop it,
	// changing the response shape.
	Author      *string      `json:"author,omitempty" bson:"author,omitempty"`
	Connections []Connection `json:"connections"      bson:"connections"`
}

// Handler serves the games routes.
type Handler struct {
	coll *mongo.Collection
}

// New builds a Handler bound to the camnections collection.
func New(db *mongox.DB) *Handler {
	return &Handler{coll: db.Collection(mongox.CollCamnections)}
}

// Routes registers the games endpoints on r.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/games", h.listGames)
	r.Get("/game/{id}", h.getGame)
}

// listGames returns every game on the singleton document, in document order.
//
// Order matters: the response is a JSON array, and the TypeScript version built
// it with Object.values(), which preserves key insertion order. Decoding into a
// bson.M here would silently randomise the list on every request, so the
// document is read as raw BSON and walked element by element.
func (h *Handler) listGames(w http.ResponseWriter, r *http.Request) {
	var doc bson.Raw
	if err := h.coll.FindOne(r.Context(), bson.D{}).Decode(&doc); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			httpx.Fail(w, &httpx.HTTPError{
				Status:  http.StatusNotFound,
				Code:    "GAMES_NOT_FOUND",
				Message: "Games collection not found",
			})
			return
		}
		httpx.Fail(w, err)
		return
	}

	elements, err := doc.Elements()
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	// Non-nil so an empty result serialises as [] rather than null.
	games := make([]Game, 0, len(elements))
	for _, element := range elements {
		if element.Key() == "_id" {
			continue
		}
		game, ok := decodeGame(element.Value())
		if !ok {
			continue
		}
		games = append(games, game)
	}

	httpx.OK(w, games)
}

// getGame returns one game, projecting only the requested key so the rest of
// the (large) document never leaves the server.
func (h *Handler) getGame(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !gameIDRE.MatchString(id) || id == "_id" {
		httpx.Fail(w, &httpx.HTTPError{
			Status:  http.StatusBadRequest,
			Code:    "INVALID_GAME_ID",
			Message: "Invalid game id",
		})
		return
	}

	notFound := &httpx.HTTPError{
		Status:  http.StatusNotFound,
		Code:    "GAME_NOT_FOUND",
		Message: "Game " + id + " not found",
	}

	var doc bson.Raw
	err := h.coll.FindOne(r.Context(), bson.D{},
		options.FindOne().SetProjection(bson.D{{Key: id, Value: 1}})).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			httpx.Fail(w, notFound)
			return
		}
		httpx.Fail(w, err)
		return
	}

	value, err := doc.LookupErr(id)
	if err != nil {
		httpx.Fail(w, notFound)
		return
	}
	game, ok := decodeGame(value)
	if !ok {
		httpx.Fail(w, notFound)
		return
	}

	httpx.OK(w, game)
}

// decodeGame applies the same structural check as the TypeScript isGame guard:
// the value must be a document carrying a `connections` array. Mongo data is
// untrusted here - the collection has no schema.
func decodeGame(value bson.RawValue) (Game, bool) {
	doc, ok := value.DocumentOK()
	if !ok {
		return Game{}, false
	}
	connections, err := doc.LookupErr("connections")
	if err != nil {
		return Game{}, false
	}
	if _, ok := connections.ArrayOK(); !ok {
		return Game{}, false
	}

	var game Game
	if err := value.Unmarshal(&game); err != nil {
		return Game{}, false
	}
	return game, true
}

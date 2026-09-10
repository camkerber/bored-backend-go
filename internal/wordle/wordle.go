// Package wordle serves the Wordle dictionary.
//
// Like camnections, the wordle collection holds a single document: the whole
// dictionary as top-level word -> definition keys.
package wordle

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/camkerber/bored-backend-go/internal/httpx"
	"github.com/camkerber/bored-backend-go/internal/mongox"
)

// Handler serves the wordle routes.
type Handler struct {
	coll *mongo.Collection
}

// New builds a Handler bound to the wordle collection.
func New(db *mongox.DB) *Handler {
	return &Handler{coll: db.Collection(mongox.CollWordle)}
}

// Routes registers the wordle endpoints on r.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/wordle-dictionary", h.getDictionary)
}

// getDictionary returns the singleton document minus _id.
//
// Unlike /api/games this response is a JSON object, so key order is not
// observable and decoding into a map is safe.
func (h *Handler) getDictionary(w http.ResponseWriter, r *http.Request) {
	var doc bson.M
	if err := h.coll.FindOne(r.Context(), bson.D{}).Decode(&doc); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			httpx.Fail(w, &httpx.HTTPError{
				Status:  http.StatusNotFound,
				Code:    "WORDLE_NOT_FOUND",
				Message: "Wordle dictionary not found",
			})
			return
		}
		httpx.Fail(w, err)
		return
	}

	dictionary := make(map[string]string, len(doc))
	for key, value := range doc {
		if key == "_id" {
			continue
		}
		if word, ok := value.(string); ok {
			dictionary[key] = word
		}
	}

	httpx.OK(w, dictionary)
}

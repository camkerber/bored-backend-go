package bingo

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/camkerber/bored-backend-go/internal/httpx"
)

// Handler serves the bingo routes.
type Handler struct {
	svc *Service
}

// NewHandler builds a Handler over svc.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Routes registers the bingo endpoints on r. cronAuth gates the cleanup route.
//
// Route order does not matter to chi - its trie prefers a static segment over a
// wildcard, so /bingo/cron/cleanup wins over /bingo/{boardId} regardless of
// registration order. The Express version depended on registration order for
// exactly this pair; TestCronRouteBeatsBoardIDWildcard pins the behaviour.
func (h *Handler) Routes(r chi.Router, cronAuth func(http.Handler) http.Handler) {
	r.Post("/bingo", h.createBoard)
	r.With(cronAuth).Get("/bingo/cron/cleanup", h.cronCleanup)
	r.Get("/bingo/{boardId}", h.mintUserBoard)
	r.Get("/bingo/{boardId}/user/{userId}", h.getUserBoard)
	r.Put("/bingo/{boardId}/user/{userId}/mark/{index}", h.toggleMark)
}

// createBoardRequest is the POST /bingo body.
type createBoardRequest struct {
	BingoBoard []string `json:"bingoBoard"`
	Name       string   `json:"name"`
}

// validate trims and bounds-checks the payload, matching the zod schemas in
// bingoController.ts. Entries are trimmed here so the service sees clean input.
func (req *createBoardRequest) validate() (entries []string, name string, err error) {
	if len(req.BingoBoard) != boardSize {
		return nil, "", httpx.Invalid("bingoBoard",
			"must contain exactly "+strconv.Itoa(boardSize)+" entries")
	}

	entries = make([]string, len(req.BingoBoard))
	for i, raw := range req.BingoBoard {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || len(trimmed) > maxEntryLength {
			return nil, "", httpx.Invalid("bingoBoard."+strconv.Itoa(i),
				"Each space must be 1-"+strconv.Itoa(maxEntryLength)+
					" characters (non-whitespace)")
		}
		entries[i] = trimmed
	}

	name = strings.TrimSpace(req.Name)
	if len(name) > maxNameLength {
		return nil, "", httpx.Invalid("name",
			"Name must be at most "+strconv.Itoa(maxNameLength)+" characters")
	}

	return entries, name, nil
}

func (h *Handler) createBoard(w http.ResponseWriter, r *http.Request) {
	var req createBoardRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}

	entries, name, err := req.validate()
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	result, err := h.svc.CreateBoard(r.Context(), entries, name)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, result)
}

func (h *Handler) mintUserBoard(w http.ResponseWriter, r *http.Request) {
	result, err := h.svc.MintUserBoard(r.Context(), chi.URLParam(r, "boardId"))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, result)
}

func (h *Handler) getUserBoard(w http.ResponseWriter, r *http.Request) {
	result, err := h.svc.GetUserBoard(r.Context(),
		chi.URLParam(r, "boardId"), chi.URLParam(r, "userId"))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, result)
}

func (h *Handler) toggleMark(w http.ResponseWriter, r *http.Request) {
	// Bounds are enforced here, not just in the service, because the previous
	// implementation validated :index in its request schema. That means an
	// out-of-range index surfaces as VALIDATION_ERROR rather than the service's
	// INVALID_MARK_INDEX, which stays as an unreachable inner guard.
	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil || index < 0 || index >= boardSize {
		httpx.Fail(w, httpx.Invalid("index",
			"must be an integer between 0 and "+strconv.Itoa(boardSize-1)))
		return
	}

	marks, err := h.svc.ToggleMark(r.Context(),
		chi.URLParam(r, "boardId"), chi.URLParam(r, "userId"), index)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string][]int{"marks": marks})
}

func (h *Handler) cronCleanup(w http.ResponseWriter, r *http.Request) {
	deleted, err := h.svc.CleanupExpired(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]int64{"deleted": deleted})
}

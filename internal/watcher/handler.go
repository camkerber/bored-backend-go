package watcher

import (
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/camkerber/bored-backend-go/internal/httpx"
)

const (
	maxEntriesPerRequest = 10
	maxSwipesPerRequest  = 20
	maxEntryIDLength     = 64
	maxTitleLength       = 200
	maxDescriptionLength = 2000
	maxServiceLength     = 100
)

// joinCodeRE matches the five-digit join code.
var joinCodeRE = regexp.MustCompile(`^\d{5}$`)

// Handler serves the watcher routes.
type Handler struct {
	svc *Service
	hub *Hub
}

// NewHandler builds a Handler over svc and hub.
func NewHandler(svc *Service, hub *Hub) *Handler {
	return &Handler{svc: svc, hub: hub}
}

// Routes registers the watcher endpoints on r.
func (h *Handler) Routes(r chi.Router, cronAuth func(http.Handler) http.Handler) {
	r.Post("/watcher/sessions", h.createSession)
	r.Post("/watcher/sessions/{code}/join", h.joinSession)
	r.With(cronAuth).Get("/watcher/cron/cleanup", h.cronCleanup)

	// Every per-session route is gated by the participant token.
	r.Group(func(r chi.Router) {
		r.Use(h.svc.RequireParticipant)
		r.Get("/watcher/sessions/{id}", h.getSession)
		r.Get("/watcher/sessions/{id}/events", h.events)
		r.Put("/watcher/sessions/{id}/entries", h.putEntries)
		r.Get("/watcher/sessions/{id}/entries", h.getDeck)
		r.Post("/watcher/sessions/{id}/ready", h.markReady)
		r.Post("/watcher/sessions/{id}/swipes", h.submitSwipes)
		r.Get("/watcher/sessions/{id}/matches", h.getMatches)
		r.Post("/watcher/sessions/{id}/rematch", h.rematch)
	})
}

// requireParticipant pulls the resolved participant off the context. The
// middleware guarantees it is present, so absence is a wiring bug.
func requireParticipant(w http.ResponseWriter, r *http.Request) (ParticipantContext, bool) {
	ctx, ok := participantFrom(r.Context())
	if !ok {
		httpx.Fail(w, httpx.Errorf(http.StatusUnauthorized,
			"MISSING_PARTICIPANT_CONTEXT", "Participant context not attached to request"))
		return ParticipantContext{}, false
	}
	return ctx, true
}

type createSessionRequest struct {
	Mode Mode `json:"mode"`
}

func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if req.Mode != ModeSolo && req.Mode != ModeDual {
		httpx.Fail(w, httpx.Invalid("mode", "must be solo-entry or dual-entry"))
		return
	}

	result, err := h.svc.CreateSession(r.Context(), req.Mode)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, httpx.NewSuccess(result, ""))
}

func (h *Handler) joinSession(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if !joinCodeRE.MatchString(code) {
		httpx.Fail(w, httpx.Invalid("code", "Code must be a 5-digit string"))
		return
	}

	result, err := h.svc.JoinSession(r.Context(), code)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, result)
}

func (h *Handler) getSession(w http.ResponseWriter, r *http.Request) {
	ctx, ok := requireParticipant(w, r)
	if !ok {
		return
	}
	state, err := h.svc.GetState(r.Context(), ctx.SessionID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, state)
}

type entriesRequest struct {
	Entries []MovieShow `json:"entries"`
}

// validate applies the request-level bounds. The mode-specific counts are
// enforced separately in the service, which is why this accepts a wider range.
func (req *entriesRequest) validate() error {
	if len(req.Entries) < 1 || len(req.Entries) > maxEntriesPerRequest {
		return httpx.Invalid("entries", "must contain between 1 and 10 items")
	}
	for i, entry := range req.Entries {
		field := "entries." + strconv.Itoa(i)
		switch {
		case len(entry.ID) < 1 || len(entry.ID) > maxEntryIDLength:
			return httpx.Invalid(field+".id", "must be 1-64 characters")
		case len(entry.Title) < 1 || len(entry.Title) > maxTitleLength:
			return httpx.Invalid(field+".title", "must be 1-200 characters")
		case strLen(entry.Description) > maxDescriptionLength:
			return httpx.Invalid(field+".description", "must be at most 2000 characters")
		case strLen(entry.Service) > maxServiceLength:
			return httpx.Invalid(field+".service", "must be at most 100 characters")
		}
	}
	return nil
}

func (h *Handler) putEntries(w http.ResponseWriter, r *http.Request) {
	ctx, ok := requireParticipant(w, r)
	if !ok {
		return
	}

	var req entriesRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Fail(w, err)
		return
	}

	// Session mode comes from the document the auth middleware already loaded.
	state, err := h.svc.PutEntries(r.Context(), ctx.SessionID, ctx.Slot,
		req.Entries, ctx.Session.Mode)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, state)
}

func (h *Handler) markReady(w http.ResponseWriter, r *http.Request) {
	ctx, ok := requireParticipant(w, r)
	if !ok {
		return
	}
	state, err := h.svc.MarkReady(r.Context(), ctx.SessionID, ctx.Slot)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, state)
}

func (h *Handler) getDeck(w http.ResponseWriter, r *http.Request) {
	ctx, ok := requireParticipant(w, r)
	if !ok {
		return
	}
	deck, err := h.svc.GetDeck(r.Context(), ctx.SessionID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string][]MovieShow{"entries": deck})
}

type swipesRequest struct {
	Likes    []string `json:"likes"`
	Dislikes []string `json:"dislikes"`
}

func (req *swipesRequest) validate() error {
	if len(req.Likes) > maxSwipesPerRequest {
		return httpx.Invalid("likes", "must contain at most 20 items")
	}
	if len(req.Dislikes) > maxSwipesPerRequest {
		return httpx.Invalid("dislikes", "must contain at most 20 items")
	}
	for field, ids := range map[string][]string{"likes": req.Likes, "dislikes": req.Dislikes} {
		for _, id := range ids {
			if id == "" {
				return httpx.Invalid(field, "entry ids must be non-empty")
			}
		}
	}
	return nil
}

func (h *Handler) submitSwipes(w http.ResponseWriter, r *http.Request) {
	ctx, ok := requireParticipant(w, r)
	if !ok {
		return
	}

	var req swipesRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := req.validate(); err != nil {
		httpx.Fail(w, err)
		return
	}

	// Non-nil so an empty swipe list persists as [] rather than null.
	likes, dislikes := req.Likes, req.Dislikes
	if likes == nil {
		likes = []string{}
	}
	if dislikes == nil {
		dislikes = []string{}
	}

	state, err := h.svc.SubmitSwipes(r.Context(), ctx.SessionID, ctx.Slot, likes, dislikes)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, state)
}

func (h *Handler) getMatches(w http.ResponseWriter, r *http.Request) {
	ctx, ok := requireParticipant(w, r)
	if !ok {
		return
	}
	matches, err := h.svc.GetMatches(r.Context(), ctx.SessionID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string][]string{"matches": matches})
}

type rematchRequest struct {
	Mode string `json:"mode"`
}

func (h *Handler) rematch(w http.ResponseWriter, r *http.Request) {
	ctx, ok := requireParticipant(w, r)
	if !ok {
		return
	}

	var req rematchRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if req.Mode != "narrow" && req.Mode != "retry" {
		httpx.Fail(w, httpx.Invalid("mode", "must be narrow or retry"))
		return
	}

	state, err := h.svc.Rematch(r.Context(), ctx.SessionID, req.Mode)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, state)
}

func (h *Handler) cronCleanup(w http.ResponseWriter, r *http.Request) {
	deleted, err := h.svc.CleanupExpired(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]int64{"deleted": deleted})
}

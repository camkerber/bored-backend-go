package watcher

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/camkerber/bored-backend-go/internal/httpx"
)

// ParticipantHeader carries the capability token minted at create or join time.
const ParticipantHeader = "x-participant-token"

// ParticipantContext is what the auth middleware resolves and hands to
// handlers, replacing the `req.participant` property the TypeScript middleware
// bolted onto the Express request type.
type ParticipantContext struct {
	SessionID string
	Session   SessionDoc
	Slot      Slot
}

type participantCtxKey struct{}

// participantFrom retrieves the resolved participant from a request context.
func participantFrom(ctx context.Context) (ParticipantContext, bool) {
	value, ok := ctx.Value(participantCtxKey{}).(ParticipantContext)
	return value, ok
}

// tokenMatches compares two tokens in constant time. The length guard is
// required because ConstantTimeCompare returns 0 immediately for mismatched
// lengths, which would otherwise leak length through timing.
func tokenMatches(stored, provided string) bool {
	if stored == "" || len(stored) != len(provided) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(provided)) == 1
}

// RequireParticipant authenticates a session request and attaches the resolved
// participant to the context.
//
// The rejection ladder is ordered exactly as the TypeScript middleware was;
// the order is observable, since a request with both a bad token and a bad
// session id must still get INVALID_SESSION_ID.
func (s *Service) RequireParticipant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(ParticipantHeader)
		if token == "" {
			httpx.Fail(w, httpx.Errorf(http.StatusUnauthorized,
				"MISSING_PARTICIPANT_TOKEN", "Missing participant token"))
			return
		}

		sessionID := chi.URLParam(r, "id")
		id, err := bson.ObjectIDFromHex(sessionID)
		if err != nil {
			httpx.Fail(w, httpx.Errorf(http.StatusBadRequest,
				"INVALID_SESSION_ID", "Invalid session id"))
			return
		}

		var session SessionDoc
		if err := s.coll.FindOne(r.Context(),
			bson.D{{Key: "_id", Value: id}}).Decode(&session); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				httpx.Fail(w, errSessionNotFound)
				return
			}
			httpx.Fail(w, err)
			return
		}

		if !session.ExpiresAt.After(nowUTC()) {
			httpx.Fail(w, errSessionExpired)
			return
		}

		var slot Slot
		switch {
		case tokenMatches(session.Participants.P1.Token, token):
			slot = SlotP1
		case tokenMatches(session.Participants.P2.Token, token):
			slot = SlotP2
		default:
			httpx.Fail(w, httpx.Errorf(http.StatusForbidden,
				"INVALID_PARTICIPANT_TOKEN", "Invalid participant token for this session"))
			return
		}

		// The session document is carried forward so handlers that only need
		// the mode or slot do not re-read it. Handlers that must observe
		// post-write state still re-read deliberately.
		ctx := context.WithValue(r.Context(), participantCtxKey{}, ParticipantContext{
			SessionID: sessionID,
			Session:   session,
			Slot:      slot,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

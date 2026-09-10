// Package watcher implements "What Are We Watching": two-participant sessions
// for picking something to stream.
//
// A session moves through waiting-for-partner -> form -> matching -> results.
// In solo-entry mode p1 supplies 2-10 candidates and p2 only swipes; in
// dual-entry mode each side supplies 1-5. Sessions expire after two hours.
package watcher

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/camkerber/bored-backend-go/internal/httpx"
	"github.com/camkerber/bored-backend-go/internal/mongox"
)

const (
	sessionTTL            = 2 * time.Hour
	codeGenerationRetries = 6

	soloMin = 2
	soloMax = 10
	dualMin = 1
	dualMax = 5
)

// Mode is how entries are collected for a session.
type Mode string

// Session modes.
const (
	ModeSolo Mode = "solo-entry"
	ModeDual Mode = "dual-entry"
)

// Status is a session's position in the flow.
type Status string

// Session statuses.
const (
	StatusWaiting  Status = "waiting-for-partner"
	StatusForm     Status = "form"
	StatusMatching Status = "matching"
	StatusResults  Status = "results"
)

// Slot identifies which participant a request is acting as.
type Slot string

// Participant slots.
const (
	SlotP1 Slot = "p1"
	SlotP2 Slot = "p2"
)

// MovieShow is one candidate title.
type MovieShow struct {
	ID    string `json:"id"                    bson:"id"`
	Title string `json:"title"                 bson:"title"`
	// Pointers so that an explicitly-empty value round-trips as "" instead of
	// being dropped by omitempty, matching the previous implementation.
	Description *string `json:"description,omitempty" bson:"description,omitempty"`
	Service     *string `json:"service,omitempty"     bson:"service,omitempty"`
}

// ParticipantState is one side's private state. Token is never serialised to
// JSON - it is the bearer capability for the session.
type ParticipantState struct {
	Token      string      `bson:"token,omitempty"`
	Entries    []MovieShow `bson:"entries"`
	FormReady  bool        `bson:"formReady"`
	Likes      []string    `bson:"likes"`
	Dislikes   []string    `bson:"dislikes"`
	SwipesDone bool        `bson:"swipesDone"`
}

// Participants holds both sides.
type Participants struct {
	P1 ParticipantState `bson:"p1"`
	P2 ParticipantState `bson:"p2"`
}

// SessionDoc is the watcher_sessions document.
type SessionDoc struct {
	ID           bson.ObjectID `bson:"_id"`
	Code         string        `bson:"code"`
	Mode         Mode          `bson:"mode"`
	Status       Status        `bson:"status"`
	CreatedAt    time.Time     `bson:"createdAt"`
	ExpiresAt    time.Time     `bson:"expiresAt"`
	Participants Participants  `bson:"participants"`
	ActiveDeck   []string      `bson:"activeDeck,omitempty"`
	Rounds       int           `bson:"rounds"`
}

// PublicParticipant is the redacted view of a participant: counts and flags
// only, never the other side's token or entries.
type PublicParticipant struct {
	Present    bool `json:"present"`
	FormReady  bool `json:"formReady"`
	SwipesDone bool `json:"swipesDone"`
	EntryCount int  `json:"entryCount"`
}

// PublicParticipants holds both redacted views.
type PublicParticipants struct {
	P1 PublicParticipant `json:"p1"`
	P2 PublicParticipant `json:"p2"`
}

// PublicSession is the session state clients poll for (and now receive over
// SSE).
type PublicSession struct {
	SessionID    string             `json:"sessionId"`
	Code         string             `json:"code"`
	Mode         Mode               `json:"mode"`
	Status       Status             `json:"status"`
	Rounds       int                `json:"rounds"`
	ExpiresAt    string             `json:"expiresAt"`
	Participants PublicParticipants `json:"participants"`
}

// CreateSessionResult is returned to p1 on session creation.
type CreateSessionResult struct {
	SessionID        string `json:"sessionId"`
	Code             string `json:"code"`
	ParticipantToken string `json:"participantToken"`
}

// JoinSessionResult is returned to p2 on join.
type JoinSessionResult struct {
	SessionID        string        `json:"sessionId"`
	ParticipantToken string        `json:"participantToken"`
	State            PublicSession `json:"state"`
}

var (
	errSessionNotFound = httpx.Errorf(http.StatusNotFound, "SESSION_NOT_FOUND", "Session not found")
	errSessionExpired  = httpx.Errorf(http.StatusGone, "SESSION_EXPIRED", "Session expired")
)

// isoTimestamp renders a time the way JavaScript's toISOString() does, so
// expiresAt is byte-identical to the previous implementation.
func isoTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func nowUTC() time.Time { return time.Now().UTC() }

// strLen returns the length of an optional string, treating absent as empty.
func strLen(s *string) int {
	if s == nil {
		return 0
	}
	return len(*s)
}

func emptyParticipant() ParticipantState {
	// Slices are non-nil so they persist as [] and $size/$concatArrays work.
	return ParticipantState{
		Entries:  []MovieShow{},
		Likes:    []string{},
		Dislikes: []string{},
	}
}

func toPublicParticipant(p ParticipantState) PublicParticipant {
	return PublicParticipant{
		Present:    p.Token != "",
		FormReady:  p.FormReady,
		SwipesDone: p.SwipesDone,
		EntryCount: len(p.Entries),
	}
}

// ToPublic redacts a session document for the wire.
func ToPublic(doc SessionDoc) PublicSession {
	return PublicSession{
		SessionID: doc.ID.Hex(),
		Code:      doc.Code,
		Mode:      doc.Mode,
		Status:    doc.Status,
		Rounds:    doc.Rounds,
		ExpiresAt: isoTimestamp(doc.ExpiresAt),
		Participants: PublicParticipants{
			P1: toPublicParticipant(doc.Participants.P1),
			P2: toPublicParticipant(doc.Participants.P2),
		},
	}
}

func deckLimits(mode Mode) (minEntries, maxEntries int) {
	if mode == ModeSolo {
		return soloMin, soloMax
	}
	return dualMin, dualMax
}

// validateEntries enforces the mode-specific bounds. This is deliberately
// stricter than the request-level schema, which allows 1-10 for both modes;
// the tighter check produces INVALID_ENTRY_COUNT rather than VALIDATION_ERROR.
func validateEntries(entries []MovieShow, mode Mode) error {
	minEntries, maxEntries := deckLimits(mode)
	if len(entries) < minEntries || len(entries) > maxEntries {
		return httpx.Errorf(http.StatusBadRequest, "INVALID_ENTRY_COUNT",
			"Entries must be between %d and %d for %s mode", minEntries, maxEntries, mode)
	}

	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if _, dup := seen[entry.ID]; dup {
			return httpx.Errorf(http.StatusBadRequest, "DUPLICATE_ENTRY_ID",
				"Duplicate entry id: %s", entry.ID)
		}
		seen[entry.ID] = struct{}{}
	}
	return nil
}

// bothFormsReady reports whether the deck can be assembled. In solo-entry mode
// p2 never fills a form, so their presence is enough.
func bothFormsReady(doc SessionDoc) bool {
	if doc.Mode == ModeSolo {
		return doc.Participants.P1.FormReady && doc.Participants.P2.Token != ""
	}
	return doc.Participants.P1.FormReady && doc.Participants.P2.FormReady
}

func hasRequiredEntries(p ParticipantState, mode Mode) bool {
	minEntries, maxEntries := deckLimits(mode)
	return len(p.Entries) >= minEntries && len(p.Entries) <= maxEntries
}

func (d SessionDoc) participant(slot Slot) ParticipantState {
	if slot == SlotP1 {
		return d.Participants.P1
	}
	return d.Participants.P2
}

// generateCode returns a five-digit join code, equivalent to the previous
// randomInt(10000, 100000) but sourced from a CSPRNG.
func generateCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(90000))
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(n.Int64()+10000, 10), nil
}

// Service owns watcher session logic and broadcasts every state change.
type Service struct {
	coll *mongo.Collection
	hub  *Hub
}

// NewService builds a Service bound to the watcher_sessions collection.
func NewService(db *mongox.DB, hub *Hub) *Service {
	return &Service{coll: db.Collection(mongox.CollWatcherSessions), hub: hub}
}

// publish pushes a state change to any SSE subscribers.
func (s *Service) publish(state PublicSession) PublicSession {
	if s.hub != nil {
		s.hub.Broadcast(state.SessionID, state)
	}
	return state
}

// CreateSession mints a session and p1's token, retrying on join-code
// collisions (the code index is unique).
func (s *Service) CreateSession(ctx context.Context, mode Mode) (CreateSessionResult, error) {
	now := time.Now().UTC()
	token := uuid.NewString()

	var lastErr error
	for range codeGenerationRetries {
		code, err := generateCode()
		if err != nil {
			return CreateSessionResult{}, err
		}

		sessionID := bson.NewObjectID()
		p1 := emptyParticipant()
		p1.Token = token

		doc := SessionDoc{
			ID:           sessionID,
			Code:         code,
			Mode:         mode,
			Status:       StatusWaiting,
			CreatedAt:    now,
			ExpiresAt:    now.Add(sessionTTL),
			Participants: Participants{P1: p1, P2: emptyParticipant()},
			Rounds:       1,
		}

		if _, err := s.coll.InsertOne(ctx, doc); err != nil {
			lastErr = err
			if mongo.IsDuplicateKeyError(err) {
				continue
			}
			return CreateSessionResult{}, err
		}

		return CreateSessionResult{
			SessionID:        sessionID.Hex(),
			Code:             code,
			ParticipantToken: token,
		}, nil
	}

	return CreateSessionResult{}, httpx.Errorf(http.StatusServiceUnavailable,
		"CODE_GENERATION_FAILED",
		"Could not generate a unique session code after %d attempts: %v",
		codeGenerationRetries, lastErr)
}

// JoinSession claims the p2 slot for a join code.
//
// The claim is a conditional update on participants.p2.token not existing, so
// two people racing on the same code cannot both become p2.
func (s *Service) JoinSession(ctx context.Context, code string) (JoinSessionResult, error) {
	now := time.Now().UTC()
	token := uuid.NewString()

	var session SessionDoc
	if err := s.coll.FindOne(ctx, bson.D{{Key: "code", Value: code}}).Decode(&session); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return JoinSessionResult{}, httpx.Errorf(http.StatusNotFound,
				"SESSION_NOT_FOUND", "No session found for that code")
		}
		return JoinSessionResult{}, err
	}
	if !session.ExpiresAt.After(now) {
		return JoinSessionResult{}, errSessionExpired
	}
	if session.Participants.P2.Token != "" {
		return JoinSessionResult{}, httpx.Errorf(http.StatusConflict,
			"SESSION_FULL", "Session is full")
	}

	nextStatus := StatusForm
	if session.Mode == ModeSolo && session.Participants.P1.FormReady {
		nextStatus = StatusMatching
	}

	filter := bson.D{
		{Key: "_id", Value: session.ID},
		{Key: "participants.p2.token", Value: bson.D{{Key: "$exists", Value: false}}},
		{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: now}}},
	}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "participants.p2.token", Value: token},
		{Key: "status", Value: nextStatus},
	}}}

	var updated SessionDoc
	err := s.coll.FindOneAndUpdate(ctx, filter, update,
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&updated)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return JoinSessionResult{}, httpx.Errorf(http.StatusConflict,
				"SESSION_FULL", "Session was claimed by another participant")
		}
		return JoinSessionResult{}, err
	}

	return JoinSessionResult{
		SessionID:        updated.ID.Hex(),
		ParticipantToken: token,
		State:            s.publish(ToPublic(updated)),
	}, nil
}

// find loads a session by hex id.
func (s *Service) find(ctx context.Context, sessionID string) (SessionDoc, error) {
	id, err := bson.ObjectIDFromHex(sessionID)
	if err != nil {
		return SessionDoc{}, httpx.Errorf(http.StatusBadRequest,
			"INVALID_SESSION_ID", "Invalid session id")
	}
	var doc SessionDoc
	if err := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&doc); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return SessionDoc{}, errSessionNotFound
		}
		return SessionDoc{}, err
	}
	return doc, nil
}

// update applies an update to a session and returns the resulting document.
func (s *Service) update(ctx context.Context, sessionID string, update any) (SessionDoc, error) {
	id, err := bson.ObjectIDFromHex(sessionID)
	if err != nil {
		return SessionDoc{}, httpx.Errorf(http.StatusBadRequest,
			"INVALID_SESSION_ID", "Invalid session id")
	}

	var updated SessionDoc
	err = s.coll.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: id}}, update,
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&updated)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return SessionDoc{}, errSessionNotFound
		}
		return SessionDoc{}, err
	}
	return updated, nil
}

// GetState returns the redacted state for a session.
func (s *Service) GetState(ctx context.Context, sessionID string) (PublicSession, error) {
	doc, err := s.find(ctx, sessionID)
	if err != nil {
		return PublicSession{}, err
	}
	return ToPublic(doc), nil
}

// PutEntries replaces the caller's candidate list.
func (s *Service) PutEntries(
	ctx context.Context, sessionID string, slot Slot, entries []MovieShow, mode Mode,
) (PublicSession, error) {
	if err := validateEntries(entries, mode); err != nil {
		return PublicSession{}, err
	}

	updated, err := s.update(ctx, sessionID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "participants." + string(slot) + ".entries", Value: entries},
	}}})
	if err != nil {
		return PublicSession{}, err
	}
	return s.publish(ToPublic(updated)), nil
}

// MarkReady flags the caller's form as complete, advancing the session to
// matching once both sides are done.
func (s *Service) MarkReady(ctx context.Context, sessionID string, slot Slot) (PublicSession, error) {
	session, err := s.find(ctx, sessionID)
	if err != nil {
		return PublicSession{}, err
	}

	switch {
	case session.Mode == ModeSolo && slot == SlotP2:
		return PublicSession{}, httpx.Errorf(http.StatusBadRequest,
			"READY_NOT_APPLICABLE", "p2 does not submit entries in solo-entry mode")
	case !hasRequiredEntries(session.participant(slot), session.Mode):
		return PublicSession{}, httpx.Errorf(http.StatusBadRequest,
			"ENTRIES_REQUIRED", "Cannot mark ready before entries are submitted")
	}

	updated, err := s.update(ctx, sessionID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "participants." + string(slot) + ".formReady", Value: true},
	}}})
	if err != nil {
		return PublicSession{}, err
	}

	if bothFormsReady(updated) && updated.Status != StatusMatching {
		if transitioned, ok := s.transition(ctx, updated.ID, StatusMatching); ok {
			updated = transitioned
		}
	}
	return s.publish(ToPublic(updated)), nil
}

// transition moves a session to status, guarded so a concurrent request cannot
// apply it twice. A failure here is not fatal: the caller keeps the state it
// already has.
func (s *Service) transition(ctx context.Context, id bson.ObjectID, status Status) (SessionDoc, bool) {
	var updated SessionDoc
	err := s.coll.FindOneAndUpdate(ctx,
		bson.D{
			{Key: "_id", Value: id},
			{Key: "status", Value: bson.D{{Key: "$ne", Value: status}}},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: status}}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&updated)
	if err != nil {
		return SessionDoc{}, false
	}
	return updated, true
}

// GetDeck returns the shared swipe deck: both sides' entries concatenated,
// narrowed to activeDeck when a previous round narrowed it.
func (s *Service) GetDeck(ctx context.Context, sessionID string) ([]MovieShow, error) {
	doc, err := s.find(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !bothFormsReady(doc) {
		return nil, httpx.Errorf(http.StatusConflict, "FORMS_NOT_READY",
			"Both participants must complete their forms before fetching the deck")
	}

	combined := make([]MovieShow, 0,
		len(doc.Participants.P1.Entries)+len(doc.Participants.P2.Entries))
	combined = append(combined, doc.Participants.P1.Entries...)
	combined = append(combined, doc.Participants.P2.Entries...)

	if len(doc.ActiveDeck) == 0 {
		return combined, nil
	}

	active := make(map[string]struct{}, len(doc.ActiveDeck))
	for _, id := range doc.ActiveDeck {
		active[id] = struct{}{}
	}
	filtered := make([]MovieShow, 0, len(combined))
	for _, entry := range combined {
		if _, ok := active[entry.ID]; ok {
			filtered = append(filtered, entry)
		}
	}
	return filtered, nil
}

// SubmitSwipes records the caller's likes and dislikes, advancing to results
// once both sides have finished.
func (s *Service) SubmitSwipes(
	ctx context.Context, sessionID string, slot Slot, likes, dislikes []string,
) (PublicSession, error) {
	prefix := "participants." + string(slot) + "."
	updated, err := s.update(ctx, sessionID, bson.D{{Key: "$set", Value: bson.D{
		{Key: prefix + "likes", Value: likes},
		{Key: prefix + "dislikes", Value: dislikes},
		{Key: prefix + "swipesDone", Value: true},
	}}})
	if err != nil {
		return PublicSession{}, err
	}

	if updated.Participants.P1.SwipesDone &&
		updated.Participants.P2.SwipesDone &&
		updated.Status != StatusResults {
		if transitioned, ok := s.transition(ctx, updated.ID, StatusResults); ok {
			updated = transitioned
		}
	}
	return s.publish(ToPublic(updated)), nil
}

// intersectLikes returns the ids both sides liked, in p2's order (matching the
// TypeScript implementation, which filtered p2's likes against a set of p1's).
func intersectLikes(doc SessionDoc) []string {
	p1 := make(map[string]struct{}, len(doc.Participants.P1.Likes))
	for _, id := range doc.Participants.P1.Likes {
		p1[id] = struct{}{}
	}
	matches := make([]string, 0, len(doc.Participants.P2.Likes))
	for _, id := range doc.Participants.P2.Likes {
		if _, ok := p1[id]; ok {
			matches = append(matches, id)
		}
	}
	return matches
}

// GetMatches returns the titles both participants liked.
func (s *Service) GetMatches(ctx context.Context, sessionID string) ([]string, error) {
	doc, err := s.find(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !doc.Participants.P1.SwipesDone || !doc.Participants.P2.SwipesDone {
		return nil, httpx.Errorf(http.StatusConflict, "SWIPES_NOT_DONE",
			"Both participants must finish swiping before matches are available")
	}
	return intersectLikes(doc), nil
}

// Rematch starts a new round. "narrow" restricts the deck to the previous
// round's matches; "retry" clears any narrowing and swipes the full deck again.
func (s *Service) Rematch(ctx context.Context, sessionID string, mode string) (PublicSession, error) {
	doc, err := s.find(ctx, sessionID)
	if err != nil {
		return PublicSession{}, err
	}
	if !doc.Participants.P1.SwipesDone || !doc.Participants.P2.SwipesDone {
		return PublicSession{}, httpx.Errorf(http.StatusConflict, "SWIPES_NOT_DONE",
			"Cannot rematch until both participants have finished swiping")
	}

	set := bson.D{
		{Key: "participants.p1.likes", Value: []string{}},
		{Key: "participants.p1.dislikes", Value: []string{}},
		{Key: "participants.p1.swipesDone", Value: false},
		{Key: "participants.p2.likes", Value: []string{}},
		{Key: "participants.p2.dislikes", Value: []string{}},
		{Key: "participants.p2.swipesDone", Value: false},
		{Key: "status", Value: StatusMatching},
		{Key: "rounds", Value: doc.Rounds + 1},
	}

	update := bson.D{}
	if mode == "narrow" {
		activeDeck := intersectLikes(doc)
		if len(activeDeck) < 2 {
			return PublicSession{}, httpx.Errorf(http.StatusBadRequest,
				"NOT_ENOUGH_MATCHES_TO_NARROW",
				"Need at least 2 prior matches to narrow the deck")
		}
		set = append(set, bson.E{Key: "activeDeck", Value: activeDeck})
		update = append(update, bson.E{Key: "$set", Value: set})
	} else {
		update = append(update,
			bson.E{Key: "$set", Value: set},
			bson.E{Key: "$unset", Value: bson.D{{Key: "activeDeck", Value: ""}}})
	}

	updated, err := s.update(ctx, sessionID, update)
	if err != nil {
		return PublicSession{}, err
	}
	return s.publish(ToPublic(updated)), nil
}

// CleanupExpired deletes sessions past their TTL.
func (s *Service) CleanupExpired(ctx context.Context) (int64, error) {
	res, err := s.coll.DeleteMany(ctx, bson.D{
		{Key: "expiresAt", Value: bson.D{{Key: "$lte", Value: time.Now().UTC()}}},
	})
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

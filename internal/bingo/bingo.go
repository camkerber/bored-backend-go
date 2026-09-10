// Package bingo implements shareable 5x5 bingo boards.
//
// A board stores its canonical 25 entries once. Each viewer gets a freshly
// shuffled view minted into an embedded userBoards map keyed by an opaque
// userId, which is the only capability - there is no authentication. Boards
// expire after seven days.
package bingo

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/camkerber/bored-backend-go/internal/httpx"
	"github.com/camkerber/bored-backend-go/internal/mongox"
)

const (
	boardTTL       = 7 * 24 * time.Hour
	boardSize      = 25
	maxUserBoards  = 10
	freeSpaceIndex = 12
	// freeSpaceSentinel is matched case-insensitively after trimming, then
	// stored in exactly this form.
	freeSpaceSentinel = "free space"
	maxEntryLength    = 75
	maxNameLength     = 50
)

// UserBoardState is one viewer's shuffled board and their marked cells.
type UserBoardState struct {
	Board []string `json:"board" bson:"board"`
	Marks []int    `json:"marks" bson:"marks"`
}

// BoardDoc is the bingo_boards document.
type BoardDoc struct {
	ID           bson.ObjectID             `bson:"_id"`
	BingoBoard   []string                  `bson:"bingoBoard"`
	HasFreeSpace bool                      `bson:"hasFreeSpace"`
	UserBoards   map[string]UserBoardState `bson:"userBoards"`
	CreatedAt    time.Time                 `bson:"createdAt"`
	ExpiresAt    time.Time                 `bson:"expiresAt"`
	Name         string                    `bson:"name,omitempty"`
}

// UserBoardResponse is the wire shape returned by every bingo endpoint except
// the mark toggle.
type UserBoardResponse struct {
	BoardID string   `json:"boardId"`
	UserID  string   `json:"userId"`
	Board   []string `json:"board"`
	Marks   []int    `json:"marks"`
	Name    string   `json:"name,omitempty"`
}

// Service holds the bingo business logic.
type Service struct {
	coll *mongo.Collection
}

// NewService builds a Service bound to the bingo_boards collection.
func NewService(db *mongox.DB) *Service {
	return &Service{coll: db.Collection(mongox.CollBingoBoards)}
}

func errBoard(status int, code, format string, args ...any) *httpx.HTTPError {
	return httpx.Errorf(status, code, format, args...)
}

var (
	errBoardNotFound = errBoard(http.StatusNotFound, "BOARD_NOT_FOUND", "Bingo board not found")
	errBoardExpired  = errBoard(http.StatusGone, "BOARD_EXPIRED", "Bingo board has expired")
	errInvalidBoard  = errBoard(http.StatusBadRequest, "INVALID_BOARD_ID", "Invalid board id")
	errUserNotFound  = errBoard(http.StatusNotFound, "USER_BOARD_NOT_FOUND",
		"User board not found for this bingo board")
	errLimitReached = errBoard(http.StatusConflict, "USER_BOARD_LIMIT_REACHED",
		"This bingo board has reached the maximum of %d user boards", maxUserBoards)
)

// isFreeSpace reports whether an entry is the free-space sentinel.
func isFreeSpace(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), freeSpaceSentinel)
}

// normalizeEntries rewrites any free-space entry to the exact sentinel form and
// reports whether the board has one. Non-sentinel entries are left untouched -
// they were already trimmed during request validation.
func normalizeEntries(entries []string) ([]string, bool) {
	normalized := make([]string, len(entries))
	hasFreeSpace := false
	for i, raw := range entries {
		if isFreeSpace(raw) {
			hasFreeSpace = true
			normalized[i] = freeSpaceSentinel
			continue
		}
		normalized[i] = raw
	}
	return normalized, hasFreeSpace
}

// shuffle performs a Fisher-Yates shuffle over a copy of src.
//
// This uses crypto/rand where the TypeScript version used Math.random(). Board
// layouts are shared between players, so a predictable shuffle is a (mild)
// information leak; the cost here is negligible.
func shuffle(src []string) ([]string, error) {
	out := make([]string, len(src))
	copy(out, src)
	for i := len(out) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return nil, fmt.Errorf("shuffle board: %w", err)
		}
		j := n.Int64()
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// buildUserBoard mints a shuffled view. When the board has a free space it is
// swapped into the centre cell and pre-marked.
func buildUserBoard(source []string, hasFreeSpace bool) (UserBoardState, error) {
	board, err := shuffle(source)
	if err != nil {
		return UserBoardState{}, err
	}

	marks := []int{}
	if hasFreeSpace {
		for i, entry := range board {
			if entry == freeSpaceSentinel {
				if i != freeSpaceIndex && freeSpaceIndex < len(board) {
					board[i], board[freeSpaceIndex] = board[freeSpaceIndex], board[i]
				}
				break
			}
		}
		marks = []int{freeSpaceIndex}
	}

	return UserBoardState{Board: board, Marks: marks}, nil
}

func toResponse(boardID bson.ObjectID, userID string, state UserBoardState, name string) UserBoardResponse {
	marks := state.Marks
	if marks == nil {
		// Must serialise as [] rather than null.
		marks = []int{}
	}
	return UserBoardResponse{
		BoardID: boardID.Hex(),
		UserID:  userID,
		Board:   state.Board,
		Marks:   marks,
		Name:    name,
	}
}

// parseBoardID converts a board id from the URL, rejecting anything that is not
// a 24-character hex ObjectID.
func parseBoardID(raw string) (bson.ObjectID, error) {
	id, err := bson.ObjectIDFromHex(raw)
	if err != nil {
		return bson.ObjectID{}, errInvalidBoard
	}
	return id, nil
}

// CreateBoard stores a new board and mints the creator's view.
func (s *Service) CreateBoard(ctx context.Context, entries []string, name string) (UserBoardResponse, error) {
	if len(entries) != boardSize {
		return UserBoardResponse{}, errBoard(http.StatusBadRequest, "INVALID_BOARD_SIZE",
			"Bingo board must have exactly %d entries", boardSize)
	}

	normalized, hasFreeSpace := normalizeEntries(entries)
	userBoard, err := buildUserBoard(normalized, hasFreeSpace)
	if err != nil {
		return UserBoardResponse{}, err
	}

	now := time.Now().UTC()
	boardID := bson.NewObjectID()
	userID := bson.NewObjectID().Hex()

	doc := BoardDoc{
		ID:           boardID,
		BingoBoard:   normalized,
		HasFreeSpace: hasFreeSpace,
		UserBoards:   map[string]UserBoardState{userID: userBoard},
		CreatedAt:    now,
		ExpiresAt:    now.Add(boardTTL),
		Name:         strings.TrimSpace(name),
	}

	if _, err := s.coll.InsertOne(ctx, doc); err != nil {
		return UserBoardResponse{}, err
	}
	return toResponse(boardID, userID, userBoard, doc.Name), nil
}

// MintUserBoard creates a new viewer identity with its own shuffled view.
//
// The user-board cap is enforced inside the update filter with $expr so two
// simultaneous mints cannot both slip past a read-then-write check.
func (s *Service) MintUserBoard(ctx context.Context, boardIDRaw string) (UserBoardResponse, error) {
	boardID, err := parseBoardID(boardIDRaw)
	if err != nil {
		return UserBoardResponse{}, err
	}

	now := time.Now().UTC()
	var doc BoardDoc
	if err := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: boardID}}).Decode(&doc); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return UserBoardResponse{}, errBoardNotFound
		}
		return UserBoardResponse{}, err
	}
	if !doc.ExpiresAt.After(now) {
		return UserBoardResponse{}, errBoardExpired
	}
	if len(doc.UserBoards) >= maxUserBoards {
		return UserBoardResponse{}, errLimitReached
	}

	userID := bson.NewObjectID().Hex()
	userBoard, err := buildUserBoard(doc.BingoBoard, doc.HasFreeSpace)
	if err != nil {
		return UserBoardResponse{}, err
	}

	filter := bson.D{
		{Key: "_id", Value: boardID},
		{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: now}}},
		{Key: "$expr", Value: bson.D{{Key: "$lt", Value: bson.A{
			bson.D{{Key: "$size", Value: bson.D{{Key: "$objectToArray", Value: bson.D{
				{Key: "$ifNull", Value: bson.A{"$userBoards", bson.D{}}},
			}}}}},
			maxUserBoards,
		}}}},
	}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "userBoards." + userID, Value: userBoard},
	}}}

	var updated BoardDoc
	err = s.coll.FindOneAndUpdate(ctx, filter, update,
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&updated)
	if err != nil {
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return UserBoardResponse{}, err
		}
		// The filter matched nothing: either the board expired between the read
		// and the write, or another request took the last slot. Re-read to say
		// which.
		var fresh BoardDoc
		if findErr := s.coll.FindOne(ctx,
			bson.D{{Key: "_id", Value: boardID}}).Decode(&fresh); findErr == nil {
			if fresh.ExpiresAt.After(now) && len(fresh.UserBoards) >= maxUserBoards {
				return UserBoardResponse{}, errLimitReached
			}
		}
		return UserBoardResponse{}, errBoardExpired
	}

	return toResponse(boardID, userID, userBoard, doc.Name), nil
}

// GetUserBoard returns an existing viewer's board and marks.
func (s *Service) GetUserBoard(ctx context.Context, boardIDRaw, userID string) (UserBoardResponse, error) {
	boardID, err := parseBoardID(boardIDRaw)
	if err != nil {
		return UserBoardResponse{}, err
	}

	var doc BoardDoc
	if err := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: boardID}}).Decode(&doc); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return UserBoardResponse{}, errBoardNotFound
		}
		return UserBoardResponse{}, err
	}
	if !doc.ExpiresAt.After(time.Now().UTC()) {
		return UserBoardResponse{}, errBoardExpired
	}

	userBoard, ok := doc.UserBoards[userID]
	if !ok {
		return UserBoardResponse{}, errUserNotFound
	}
	return toResponse(boardID, userID, userBoard, doc.Name), nil
}

// ToggleMark flips the mark at index for one viewer.
//
// The flip runs as an aggregation-pipeline update so that add-if-absent /
// remove-if-present is evaluated server-side; two concurrent toggles from the
// same viewer cannot lose an update the way a read-modify-write would.
func (s *Service) ToggleMark(ctx context.Context, boardIDRaw, userID string, index int) ([]int, error) {
	if index < 0 || index >= boardSize {
		return nil, errBoard(http.StatusBadRequest, "INVALID_MARK_INDEX",
			"Mark index must be an integer between 0 and %d", boardSize-1)
	}

	boardID, err := parseBoardID(boardIDRaw)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var doc BoardDoc
	if err := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: boardID}}).Decode(&doc); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, errBoardNotFound
		}
		return nil, err
	}
	if !doc.ExpiresAt.After(now) {
		return nil, errBoardExpired
	}
	if _, ok := doc.UserBoards[userID]; !ok {
		return nil, errUserNotFound
	}
	if doc.HasFreeSpace && index == freeSpaceIndex {
		return nil, errBoard(http.StatusBadRequest, "FREE_SPACE_NOT_TOGGLEABLE",
			"The free space cannot be toggled")
	}

	// userID is a minted ObjectID hex and was just confirmed to be a key of
	// userBoards, so it cannot inject a dotted path here.
	marksPath := "userBoards." + userID + ".marks"
	marksRef := "$" + marksPath

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$set", Value: bson.D{{Key: marksPath, Value: bson.D{
			{Key: "$cond", Value: bson.A{
				bson.D{{Key: "$in", Value: bson.A{index, marksRef}}},
				bson.D{{Key: "$filter", Value: bson.D{
					{Key: "input", Value: marksRef},
					{Key: "as", Value: "m"},
					{Key: "cond", Value: bson.D{{Key: "$ne", Value: bson.A{"$$m", index}}}},
				}}},
				bson.D{{Key: "$concatArrays", Value: bson.A{marksRef, bson.A{index}}}},
			}},
		}}}}},
	}

	var updated BoardDoc
	err = s.coll.FindOneAndUpdate(ctx,
		bson.D{
			{Key: "_id", Value: boardID},
			{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: now}}},
		},
		pipeline,
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&updated)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, errBoardExpired
		}
		return nil, err
	}

	marks := updated.UserBoards[userID].Marks
	if marks == nil {
		marks = []int{}
	}
	return marks, nil
}

// CleanupExpired deletes boards past their TTL. The MongoDB TTL monitor does
// this too, but only about once a minute, and this endpoint predates it.
func (s *Service) CleanupExpired(ctx context.Context) (int64, error) {
	res, err := s.coll.DeleteMany(ctx, bson.D{
		{Key: "expiresAt", Value: bson.D{{Key: "$lte", Value: time.Now().UTC()}}},
	})
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

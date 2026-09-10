package games

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// rawValue encodes a document as the BSON value that decodeGame receives when
// walking the camnections document.
func rawValue(t *testing.T, doc bson.D) bson.RawValue {
	t.Helper()
	encoded, err := bson.Marshal(doc)
	require.NoError(t, err)
	return bson.RawValue{Type: bson.TypeEmbeddedDocument, Value: encoded}
}

func validGame(extra ...bson.E) bson.D {
	doc := bson.D{
		{Key: "id", Value: "g1"},
		{Key: "title", Value: "A Puzzle"},
		{Key: "connections", Value: bson.A{
			bson.D{
				{Key: "category", Value: "yellow"},
				{Key: "description", Value: "desserts"},
				{Key: "options", Value: bson.A{"cake", "pie"}},
			},
		}},
	}
	return append(doc, extra...)
}

// TestAuthorIsOnlyOmittedWhenAbsent is a regression test.
//
// Several stored games carry author:"" and the previous implementation emitted
// it as an empty string. Modelling Author as a plain string with omitempty
// dropped the key entirely, which a live diff against the old service caught.
func TestAuthorIsOnlyOmittedWhenAbsent(t *testing.T) {
	tests := []struct {
		name        string
		doc         bson.D
		wantKey     bool
		wantValue   string
		description string
	}{
		{
			name:    "author absent from the document",
			doc:     validGame(),
			wantKey: false,
		},
		{
			name:      "author stored as an empty string",
			doc:       validGame(bson.E{Key: "author", Value: ""}),
			wantKey:   true,
			wantValue: "",
		},
		{
			name:      "author populated",
			doc:       validGame(bson.E{Key: "author", Value: "Cam Kerber"}),
			wantKey:   true,
			wantValue: "Cam Kerber",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			game, ok := decodeGame(rawValue(t, tc.doc))
			require.True(t, ok)

			encoded, err := json.Marshal(game)
			require.NoError(t, err)

			var out map[string]any
			require.NoError(t, json.Unmarshal(encoded, &out))

			value, present := out["author"]
			assert.Equal(t, tc.wantKey, present,
				"author key presence must mirror the stored document")
			if tc.wantKey {
				assert.Equal(t, tc.wantValue, value)
			}
		})
	}
}

// TestDecodeGameRejectsNonGames mirrors the structural isGame guard: the
// camnections collection is schemaless, so sibling keys must be skipped rather
// than trusted.
func TestDecodeGameRejectsNonGames(t *testing.T) {
	tests := []struct {
		name  string
		value bson.RawValue
	}{
		{
			name:  "connections missing",
			value: rawValue(t, bson.D{{Key: "id", Value: "x"}, {Key: "title", Value: "T"}}),
		},
		{
			name: "connections is not an array",
			value: rawValue(t, bson.D{
				{Key: "id", Value: "x"},
				{Key: "connections", Value: "not-an-array"},
			}),
		},
		{
			name:  "value is a bare string",
			value: bson.RawValue{Type: bson.TypeString, Value: []byte("nope")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := decodeGame(tc.value)
			assert.False(t, ok)
		})
	}
}

func TestDecodeGameAcceptsAnEmptyConnectionsArray(t *testing.T) {
	doc := bson.D{{Key: "id", Value: "x"}, {Key: "connections", Value: bson.A{}}}

	_, ok := decodeGame(rawValue(t, doc))

	assert.True(t, ok, "an empty connections array still satisfies the shape check")
}

func TestGameIDPattern(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"67", true},
		{"cam-1", true},
		{"with_underscore", true},
		{"has space", false},
		{"with.dot", false},
		{"$where", false},
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			assert.Equal(t, tc.want, gameIDRE.MatchString(tc.id))
		})
	}
}

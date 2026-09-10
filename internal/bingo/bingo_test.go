package bingo

import (
	"sort"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// entries builds a board of n distinct placeholder entries.
func entries(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "entry-" + strconv.Itoa(i)
	}
	return out
}

func TestIsFreeSpace(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"free space", true},
		{"Free Space", true},
		{"FREE SPACE", true},
		{"  free space  ", true},
		{"FrEe SpAcE", true},
		{"free  space", false}, // two spaces is a different string
		{"freespace", false},
		{"free space!", false},
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			assert.Equal(t, tc.want, isFreeSpace(tc.value))
		})
	}
}

func TestNormalizeEntriesRewritesOnlyTheSentinel(t *testing.T) {
	input := []string{"Alpha", "  FREE SPACE ", "Beta"}

	normalized, hasFreeSpace := normalizeEntries(input)

	assert.True(t, hasFreeSpace)
	assert.Equal(t, []string{"Alpha", freeSpaceSentinel, "Beta"}, normalized,
		"non-sentinel entries must be passed through untouched")
}

func TestNormalizeEntriesWithoutFreeSpace(t *testing.T) {
	normalized, hasFreeSpace := normalizeEntries([]string{"Alpha", "Beta"})

	assert.False(t, hasFreeSpace)
	assert.Equal(t, []string{"Alpha", "Beta"}, normalized)
}

// TestShufflePreservesTheMultiset is the invariant that matters: a shuffle may
// reorder but must never add, drop, or duplicate an entry.
func TestShufflePreservesTheMultiset(t *testing.T) {
	source := entries(boardSize)

	shuffled, err := shuffle(source)
	require.NoError(t, err)
	require.Len(t, shuffled, boardSize)

	sortedSource := append([]string{}, source...)
	sortedShuffled := append([]string{}, shuffled...)
	sort.Strings(sortedSource)
	sort.Strings(sortedShuffled)

	assert.Equal(t, sortedSource, sortedShuffled)
	assert.Equal(t, entries(boardSize), source, "source must not be mutated")
}

func TestBuildUserBoardPlacesFreeSpaceInTheCentre(t *testing.T) {
	source := entries(boardSize)
	source[0] = freeSpaceSentinel

	// Repeat: the free space starts at a random position after shuffling, so a
	// single run could pass by luck.
	for range 50 {
		state, err := buildUserBoard(source, true)
		require.NoError(t, err)

		require.Len(t, state.Board, boardSize)
		assert.Equal(t, freeSpaceSentinel, state.Board[freeSpaceIndex],
			"free space must be swapped into the centre cell")
		assert.Equal(t, []int{freeSpaceIndex}, state.Marks,
			"free space must be pre-marked")
	}
}

func TestBuildUserBoardWithoutFreeSpaceStartsUnmarked(t *testing.T) {
	state, err := buildUserBoard(entries(boardSize), false)
	require.NoError(t, err)

	assert.Len(t, state.Board, boardSize)
	assert.Empty(t, state.Marks)
	assert.NotNil(t, state.Marks, "marks must serialise as [] rather than null")
}

func TestParseBoardID(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"valid hex object id", "507f1f77bcf86cd799439011", false},
		{"too short", "507f1f77", true},
		{"non-hex", "zzzzzzzzzzzzzzzzzzzzzzzz", true},
		{"empty", "", true},
		// JavaScript's ObjectId.isValid() also accepted any 12-character
		// string, so this input used to reach the database and 404. Go accepts
		// only 24-char hex, so it is now a 400. Deliberate divergence.
		{"twelve character string", "abcdefghijkl", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBoardID(tc.raw)
			if tc.wantErr {
				assert.ErrorIs(t, err, errInvalidBoard)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCreateBoardRequestValidate(t *testing.T) {
	tests := []struct {
		name        string
		req         createBoardRequest
		wantErr     bool
		wantEntry0  string
		wantName    string
		description string
	}{
		{
			name:       "trims each entry",
			req:        createBoardRequest{BingoBoard: append([]string{"  padded  "}, entries(24)...)},
			wantEntry0: "padded",
		},
		{
			name:     "trims the name",
			req:      createBoardRequest{BingoBoard: entries(25), Name: "  My Board  "},
			wantName: "My Board",
		},
		{
			name:    "rejects too few entries",
			req:     createBoardRequest{BingoBoard: entries(24)},
			wantErr: true,
		},
		{
			name:    "rejects too many entries",
			req:     createBoardRequest{BingoBoard: entries(26)},
			wantErr: true,
		},
		{
			name:    "rejects a whitespace-only entry",
			req:     createBoardRequest{BingoBoard: append([]string{"   "}, entries(24)...)},
			wantErr: true,
		},
		{
			name: "rejects an over-long entry",
			req: createBoardRequest{BingoBoard: append(
				[]string{string(make([]byte, maxEntryLength+1))}, entries(24)...)},
			wantErr: true,
		},
		{
			name: "rejects an over-long name",
			req: createBoardRequest{
				BingoBoard: entries(25),
				Name:       string(make([]byte, maxNameLength+1)),
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotEntries, gotName, err := tc.req.validate()

			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, gotEntries, boardSize)
			if tc.wantEntry0 != "" {
				assert.Equal(t, tc.wantEntry0, gotEntries[0])
			}
			if tc.wantName != "" {
				assert.Equal(t, tc.wantName, gotName)
			}
		})
	}
}

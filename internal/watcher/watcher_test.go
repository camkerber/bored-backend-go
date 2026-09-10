package watcher

import (
	"encoding/json"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func shows(n int) []MovieShow {
	out := make([]MovieShow, n)
	for i := range out {
		out[i] = MovieShow{ID: "id-" + strconv.Itoa(i), Title: "Title " + strconv.Itoa(i)}
	}
	return out
}

func TestValidateEntriesBoundsPerMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     Mode
		count    int
		wantErr  bool
		wantCode string
	}{
		{name: "solo below minimum", mode: ModeSolo, count: 1, wantErr: true, wantCode: "INVALID_ENTRY_COUNT"},
		{name: "solo at minimum", mode: ModeSolo, count: 2},
		{name: "solo at maximum", mode: ModeSolo, count: 10},
		{name: "solo above maximum", mode: ModeSolo, count: 11, wantErr: true, wantCode: "INVALID_ENTRY_COUNT"},
		{name: "dual at minimum", mode: ModeDual, count: 1},
		{name: "dual at maximum", mode: ModeDual, count: 5},
		{name: "dual above maximum", mode: ModeDual, count: 6, wantErr: true, wantCode: "INVALID_ENTRY_COUNT"},
		{name: "dual with zero entries", mode: ModeDual, count: 0, wantErr: true, wantCode: "INVALID_ENTRY_COUNT"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEntries(shows(tc.count), tc.mode)

			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantCode)
		})
	}
}

func TestValidateEntriesRejectsDuplicateIDs(t *testing.T) {
	duplicated := []MovieShow{
		{ID: "same", Title: "First"},
		{ID: "same", Title: "Second"},
	}

	err := validateEntries(duplicated, ModeDual)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DUPLICATE_ENTRY_ID")
}

// TestBothFormsReady covers the asymmetry between modes: in solo-entry, p2
// never fills a form, so merely being present is enough.
func TestBothFormsReady(t *testing.T) {
	tests := []struct {
		name      string
		mode      Mode
		p1Ready   bool
		p2Ready   bool
		p2Present bool
		want      bool
	}{
		{name: "solo, p1 ready and p2 joined", mode: ModeSolo, p1Ready: true, p2Present: true, want: true},
		{name: "solo, p1 ready but nobody joined", mode: ModeSolo, p1Ready: true, want: false},
		{name: "solo, p2 joined but p1 not ready", mode: ModeSolo, p2Present: true, want: false},
		{name: "dual, both ready", mode: ModeDual, p1Ready: true, p2Ready: true, p2Present: true, want: true},
		{name: "dual, only p1 ready", mode: ModeDual, p1Ready: true, p2Present: true, want: false},
		{name: "dual, only p2 ready", mode: ModeDual, p2Ready: true, p2Present: true, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := SessionDoc{Mode: tc.mode}
			doc.Participants.P1.FormReady = tc.p1Ready
			doc.Participants.P2.FormReady = tc.p2Ready
			if tc.p2Present {
				doc.Participants.P2.Token = "token"
			}

			assert.Equal(t, tc.want, bothFormsReady(doc))
		})
	}
}

func TestIntersectLikes(t *testing.T) {
	tests := []struct {
		name     string
		p1Likes  []string
		p2Likes  []string
		expected []string
	}{
		{
			name:     "overlap is returned in p2 order",
			p1Likes:  []string{"c", "a", "b"},
			p2Likes:  []string{"b", "c"},
			expected: []string{"b", "c"},
		},
		{
			name:     "no overlap",
			p1Likes:  []string{"a"},
			p2Likes:  []string{"b"},
			expected: []string{},
		},
		{
			name:     "one side liked nothing",
			p1Likes:  []string{},
			p2Likes:  []string{"a", "b"},
			expected: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := SessionDoc{}
			doc.Participants.P1.Likes = tc.p1Likes
			doc.Participants.P2.Likes = tc.p2Likes

			got := intersectLikes(doc)

			assert.Equal(t, tc.expected, got)
			assert.NotNil(t, got, "matches must serialise as [] rather than null")
		})
	}
}

func TestGenerateCodeIsAlwaysFiveDigits(t *testing.T) {
	fiveDigits := regexp.MustCompile(`^\d{5}$`)

	for range 500 {
		code, err := generateCode()
		require.NoError(t, err)
		require.Regexp(t, fiveDigits, code)

		n, err := strconv.Atoi(code)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, n, 10000)
		assert.LessOrEqual(t, n, 99999)
	}
}

// TestToPublicNeverLeaksSecrets is the important redaction test: the public
// projection must expose counts and flags only, never a token or the other
// side's candidate titles.
func TestToPublicNeverLeaksSecrets(t *testing.T) {
	doc := SessionDoc{
		ID:        bson.NewObjectID(),
		Code:      "12345",
		Mode:      ModeDual,
		Status:    StatusMatching,
		Rounds:    2,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	doc.Participants.P1 = ParticipantState{
		Token:     "p1-secret-token",
		Entries:   shows(3),
		FormReady: true,
	}
	doc.Participants.P2 = ParticipantState{
		Token:      "p2-secret-token",
		Entries:    shows(2),
		SwipesDone: true,
		Likes:      []string{"id-0"},
	}

	serialised, err := json.Marshal(ToPublic(doc))
	require.NoError(t, err)

	assert.NotContains(t, string(serialised), "p1-secret-token")
	assert.NotContains(t, string(serialised), "p2-secret-token")
	assert.NotContains(t, string(serialised), "Title 0", "entries must not be exposed")

	public := ToPublic(doc)
	assert.True(t, public.Participants.P1.Present)
	assert.Equal(t, 3, public.Participants.P1.EntryCount)
	assert.True(t, public.Participants.P1.FormReady)
	assert.Equal(t, 2, public.Participants.P2.EntryCount)
	assert.True(t, public.Participants.P2.SwipesDone)
}

func TestToPublicMarksAbsentParticipant(t *testing.T) {
	doc := SessionDoc{ID: bson.NewObjectID(), Mode: ModeSolo, ExpiresAt: time.Now()}
	doc.Participants.P1.Token = "present"

	public := ToPublic(doc)

	assert.True(t, public.Participants.P1.Present)
	assert.False(t, public.Participants.P2.Present, "an unclaimed p2 slot is absent")
}

func TestISOTimestampMatchesJavaScript(t *testing.T) {
	// Milliseconds ending in zero are where RFC3339Nano would diverge by
	// trimming, so pin that case explicitly.
	at := time.Date(2026, 9, 9, 12, 34, 56, 780_000_000, time.UTC)

	assert.Equal(t, "2026-09-09T12:34:56.780Z", isoTimestamp(at))
}

func TestTokenMatches(t *testing.T) {
	tests := []struct {
		name     string
		stored   string
		provided string
		want     bool
	}{
		{name: "identical", stored: "abc", provided: "abc", want: true},
		{name: "different", stored: "abc", provided: "xyz", want: false},
		{name: "different lengths", stored: "abc", provided: "abcd", want: false},
		{name: "empty stored slot never matches", stored: "", provided: "", want: false},
		{name: "empty stored against a real token", stored: "", provided: "abc", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tokenMatches(tc.stored, tc.provided))
		})
	}
}

// TestEmptyParticipantUsesNonNilSlices matters at the storage layer: a nil
// slice persists as null, which breaks the $size and $concatArrays expressions
// used elsewhere.
func TestEmptyParticipantUsesNonNilSlices(t *testing.T) {
	p := emptyParticipant()

	assert.NotNil(t, p.Entries)
	assert.NotNil(t, p.Likes)
	assert.NotNil(t, p.Dislikes)
	assert.Empty(t, p.Token)
	assert.False(t, p.FormReady)
	assert.False(t, p.SwipesDone)
}

func TestHasRequiredEntries(t *testing.T) {
	tests := []struct {
		name  string
		mode  Mode
		count int
		want  bool
	}{
		{name: "solo with 2", mode: ModeSolo, count: 2, want: true},
		{name: "solo with 1", mode: ModeSolo, count: 1, want: false},
		{name: "solo with 11", mode: ModeSolo, count: 11, want: false},
		{name: "dual with 1", mode: ModeDual, count: 1, want: true},
		{name: "dual with 0", mode: ModeDual, count: 0, want: false},
		{name: "dual with 6", mode: ModeDual, count: 6, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := ParticipantState{Entries: shows(tc.count)}
			assert.Equal(t, tc.want, hasRequiredEntries(p, tc.mode))
		})
	}
}

package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// jsToISOString is the exact shape of JavaScript's Date.toISOString(): always
// three fractional-second digits, always a literal Z.
var jsToISOString = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

func TestTimestampMatchesJavaScriptToISOString(t *testing.T) {
	assert.Regexp(t, jsToISOString, now(),
		"timestamp must match Date.toISOString(); RFC3339Nano trims trailing zeros")
}

func TestSuccessEnvelopeKeyOrderAndShape(t *testing.T) {
	body, err := json.Marshal(NewSuccess([]string{"a"}, ""))
	require.NoError(t, err)

	// Key order is asserted literally because the golden-file contract tests
	// diff raw bytes against the TypeScript backend's responses.
	assert.Regexp(t,
		`^\{"success":true,"message":"Operation completed successfully",`+
			`"data":\["a"\],"timestamp":"[^"]+"\}$`,
		string(body))
}

func TestSuccessEnvelopeCustomMessage(t *testing.T) {
	var got SuccessResponse
	body, err := json.Marshal(NewSuccess(nil, "custom"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &got))

	assert.True(t, got.Success)
	assert.Equal(t, "custom", got.Message)
	assert.Nil(t, got.Data)
}

func TestErrorEnvelopeDuplicatesMessage(t *testing.T) {
	body, err := json.Marshal(NewError("boom", "SOME_CODE", nil))
	require.NoError(t, err)

	// message appears at the top level AND inside error - createErrorResponse
	// set both from the same argument and the client falls back between them.
	assert.Regexp(t,
		`^\{"success":false,"message":"boom",`+
			`"error":\{"message":"boom","code":"SOME_CODE"\},"timestamp":"[^"]+"\}$`,
		string(body))
}

func TestErrorEnvelopeOmitsEmptyCodeAndDetails(t *testing.T) {
	body, err := json.Marshal(NewError("boom", "", nil))
	require.NoError(t, err)

	// JSON.stringify drops undefined keys entirely; omitempty reproduces that.
	assert.NotContains(t, string(body), `"code"`)
	assert.NotContains(t, string(body), `"details"`)
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
		wantDetails any
	}{
		{
			name:        "HTTPError carries its own status and code",
			err:         &HTTPError{Status: 410, Code: "BOARD_EXPIRED", Message: "Bingo board has expired"},
			wantStatus:  http.StatusGone,
			wantCode:    "BOARD_EXPIRED",
			wantMessage: "Bingo board has expired",
		},
		{
			name:        "wrapped HTTPError is still unwrapped",
			err:         errors.Join(errors.New("context"), &HTTPError{Status: 404, Code: "X", Message: "m"}),
			wantStatus:  http.StatusNotFound,
			wantCode:    "X",
			wantMessage: "m",
		},
		{
			name:        "validation error",
			err:         Invalid("mode", "must be solo-entry or dual-entry"),
			wantStatus:  http.StatusBadRequest,
			wantCode:    "VALIDATION_ERROR",
			wantMessage: "Request body validation failed",
			wantDetails: []ValidationIssue{{Path: "mode", Message: "must be solo-entry or dual-entry"}},
		},
		{
			name:        "mongo duplicate key",
			err:         mongo.CommandError{Code: 11000, Message: "dup"},
			wantStatus:  http.StatusConflict,
			wantCode:    "DUPLICATE_KEY",
			wantMessage: "Duplicate key error",
			wantDetails: "A document with this data already exists",
		},
		{
			name:        "mongo document validation",
			err:         mongo.CommandError{Code: 121, Message: "schema"},
			wantStatus:  http.StatusBadRequest,
			wantCode:    "DOCUMENT_VALIDATION_ERROR",
			wantMessage: "Document validation failed",
		},
		{
			name:        "other mongo error leaks only the numeric code",
			err:         mongo.CommandError{Code: 50, Message: "timeout"},
			wantStatus:  http.StatusInternalServerError,
			wantCode:    "DATABASE_ERROR",
			wantMessage: "Database error",
			wantDetails: int32(50),
		},
		{
			name:        "unknown error is scrubbed",
			err:         errors.New("connection string contains a password"),
			wantStatus:  http.StatusInternalServerError,
			wantCode:    "INTERNAL_ERROR",
			wantMessage: "Internal server error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, body := Classify(tc.err)

			assert.Equal(t, tc.wantStatus, status)
			assert.Equal(t, tc.wantCode, body.Error.Code)
			assert.Equal(t, tc.wantMessage, body.Error.Message)
			assert.Equal(t, tc.wantMessage, body.Message, "message must be duplicated")
			assert.False(t, body.Success)
			if tc.wantDetails != nil {
				assert.Equal(t, tc.wantDetails, body.Error.Details)
			}
		})
	}
}

func TestClassifyNeverLeaksInternalErrorText(t *testing.T) {
	_, body := Classify(errors.New("mongodb+srv://user:hunter2@cluster.example.com"))

	serialised, err := json.Marshal(body)
	require.NoError(t, err)
	assert.NotContains(t, string(serialised), "hunter2")
	assert.NotContains(t, string(serialised), "mongodb+srv")
}

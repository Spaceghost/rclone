package gofakes3

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteConditions(t *testing.T) {
	for _, tc := range []struct {
		name, match, none, etag string
		exists                  bool
		want                    error
	}{
		{"Unconditional", "", "", "", false, nil},
		{"Create", "", "*", "", false, nil},
		{"Exists", "", "*", "old", true, ErrPreconditionFailed},
		{"Match", "old", "", "old", true, nil},
		{"Stale", "old", "", "new", true, ErrPreconditionFailed},
		{"Missing", "old", "", "", false, ErrPreconditionFailed},
		{"NoHash", "old", "", "", true, ErrNotImplemented},
		{"AnyExisting", "*", "", "", true, nil},
		{"AnyMissing", "*", "", "", false, ErrPreconditionFailed},
		{"InvalidNone", "", "etag", "etag", true, ErrInvalidArgument},
		{"Both", "old", "*", "old", true, ErrPreconditionFailed},
		{"CaseSensitive", "OLD", "", "old", true, ErrPreconditionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &WriteConditions{IfMatch: tc.match, IfNoneMatch: tc.none}
			assert.ErrorIs(t, c.Check(tc.exists, tc.etag), tc.want)
		})
	}
	var c *WriteConditions
	assert.NoError(t, c.Check(false, ""))
}

func TestParseWriteConditions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		h       http.Header
		want    *WriteConditions
		invalid bool
	}{
		{"Absent", nil, nil, false},
		{"Create", http.Header{"If-None-Match": {"*"}}, &WriteConditions{IfNoneMatch: "*"}, false},
		{"Quoted", http.Header{"If-Match": {`"etag"`}}, &WriteConditions{IfMatch: "etag"}, false},
		{"Bare", http.Header{"If-Match": {"etag"}}, &WriteConditions{IfMatch: "etag"}, false},
		{"Spaces", http.Header{"If-Match": {" \t\"etag\" \t"}}, &WriteConditions{IfMatch: "etag"}, false},
		{"Wildcard", http.Header{"If-Match": {"*"}}, &WriteConditions{IfMatch: "*"}, false},
		{"Empty", http.Header{"If-Match": {""}}, nil, true},
		{"QuotedWildcard", http.Header{"If-Match": {`"*"`}}, nil, true},
		{"EmptyQuoted", http.Header{"If-Match": {`""`}}, nil, true},
		{"Weak", http.Header{"If-Match": {`W/"etag"`}}, nil, true},
		{"List", http.Header{"If-Match": {`"a", "b"`}}, nil, true},
		{"Duplicate", http.Header{"If-Match": {"a", "b"}}, nil, true},
		{"NoneTag", http.Header{"If-None-Match": {`"etag"`}}, nil, true},
		{"NoneEmpty", http.Header{"If-None-Match": {""}}, nil, true},
		{"NoneDuplicate", http.Header{"If-None-Match": {"*", "*"}}, nil, true},
		{"Both", http.Header{"If-Match": {`"etag"`}, "If-None-Match": {"*"}}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWriteConditions(tc.h)
			if tc.invalid {
				require.True(t, HasErrorCode(err, ErrInvalidArgument))
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestPreconditionFailedResponse(t *testing.T) {
	g := New(nil)
	r := httptest.NewRecorder()
	g.httpError(r, httptest.NewRequest(http.MethodPut, "/bucket/key", nil), ErrPreconditionFailed)
	assert.Equal(t, http.StatusPreconditionFailed, r.Code)
	assert.Contains(t, r.Body.String(), "<Code>PreconditionFailed</Code>")
}

func TestConditionalRequestConflictResponse(t *testing.T) {
	g := New(nil)
	r := httptest.NewRecorder()
	g.httpError(r, httptest.NewRequest(http.MethodPut, "/bucket/key", nil), ErrConditionalRequestConflict)
	assert.Equal(t, http.StatusConflict, r.Code)
	assert.Contains(t, r.Body.String(), "<Code>ConditionalRequestConflict</Code>")
}

func TestConditionalBackendRequired(t *testing.T) {
	g := New(nil)
	_, err := g.putObjectWithConditions(context.Background(), "bucket", "key", nil, nil, 0, &WriteConditions{IfNoneMatch: "*"})
	require.ErrorIs(t, err, ErrNotImplemented)
}

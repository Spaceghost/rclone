package s3

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitEntityTags(t *testing.T) {
	for _, test := range []struct {
		name, value string
		want        []string
	}{
		{"Empty", "", nil},
		{"Whitespace", " \t", nil},
		{"Wildcard", " \t*\t ", []string{"*"}},
		{"Strong", `"etag"`, []string{`"etag"`}},
		{"Weak", `W/"etag"`, []string{`W/"etag"`}},
		{"EmptyTag", `""`, []string{`""`}},
		{"List", "\t\"one\" , W/\"two\" ", []string{`"one"`, `W/"two"`}},
		{"Comma", `"one,two", "three"`, []string{`"one,two"`, `"three"`}},
		{"Backslash", `"one\two"`, []string{`"one\two"`}},
		{"ObsText", "\"\x80\xff\"", []string{"\"\x80\xff\""}},
		{"Unquoted", "etag", []string{`"etag"`}},
		{"UnquotedMultipart", "0123456789abcdef-2", []string{`"0123456789abcdef-2"`}},
		{"UnquotedList", "one,two", nil},
		{"UnquotedSpace", "one two", nil},
		{"UnquotedControl", "one\x00two", nil},
		{"UnquotedDEL", "one\x7f", nil},
		{"UnquotedObsText", "one\xff", nil},
		{"UnquotedWildcard", "one*", nil},
		{"Unterminated", `"etag`, nil},
		{"SpaceInTag", `"one two"`, nil},
		{"Control", "\"one\x00two\"", nil},
		{"DEL", "\"one\x7ftwo\"", nil},
		{"CRLF", "\"one\"\r\n", nil},
		{"MissingComma", `"one" "two"`, nil},
		{"TrailingComma", `"one",`, nil},
		{"LeadingComma", `,"one"`, nil},
		{"WildcardList", `*, "one"`, nil},
		{"WeakWildcard", "W/*", nil},
		{"LowercaseWeak", `w/"one"`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, splitEntityTags(test.value))
		})
	}
}

func TestConditionsFromRequest(t *testing.T) {
	for _, test := range []struct {
		name        string
		header      http.Header
		match, none string
		invalid     bool
	}{
		{"Absent", http.Header{}, "", "", false},
		{"Match", http.Header{"If-Match": {`"one"`}}, `"one"`, "", false},
		{"Repeated", http.Header{"If-Match": {`"one"`, `"two"`}}, `"one","two"`, "", false},
		{"None", http.Header{"If-None-Match": {"*"}}, "", "*", false},
		{"Both", http.Header{"If-Match": {`"one"`}, "If-None-Match": {"*"}}, `"one"`, "*", false},
		{"EmptyMatch", http.Header{"If-Match": {""}}, "", "", true},
		{"EmptyNone", http.Header{"If-None-Match": {""}}, "", "", true},
		{"Unquoted", http.Header{"If-Match": {"one"}}, "one", "", false},
		{"UnquotedList", http.Header{"If-Match": {"one", "two"}}, "one,two", "", true},
		{"Unterminated", http.Header{"If-None-Match": {`"one`}}, "", `"one`, true},
		{"RepeatedWildcard", http.Header{"If-Match": {"*", "*"}}, "*,*", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/bucket/key", nil)
			r.Header = test.header
			got := conditionsFromRequest(r)
			if test.name == "Absent" {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, test.match, got.ifMatch)
			assert.Equal(t, test.none, got.ifNoneMatch)
			assert.Equal(t, test.invalid, got.invalid)
		})
	}
}

func FuzzEntityTags(f *testing.F) {
	for _, value := range []string{"", "*", `""`, `"one"`, `W/"one"`, `"one,two", W/"three"`, `"unterminated`, "\"\xff\"", "\x00", "0123456789abcdef-2"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		tags := splitEntityTags(value)
		if len(tags) == 0 {
			return
		}
		assert.Equal(t, tags, splitEntityTags(strings.Join(tags, ", ")))
		assert.Empty(t, splitEntityTags(value+"\x00"))
		for _, tag := range tags {
			if tag == "*" {
				require.Len(t, tags, 1)
				assert.True(t, ifMatchHolds(value, true, `"other"`))
				assert.False(t, ifNoneMatchHolds(value, true, `"other"`))
				continue
			}
			strong := strings.TrimPrefix(tag, "W/")
			assert.False(t, ifNoneMatchHolds(value, true, strong))
			if tag == strong {
				assert.True(t, ifMatchHolds(value, true, strong))
			} else {
				assert.False(t, ifMatchHolds(tag, true, strong))
			}
		}
	})
}

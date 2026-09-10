package s3

import (
	"context"
	"net/http"
	"strings"

	"github.com/rclone/gofakes3"
)

const (
	errPreconditionFailed         gofakes3.ErrorCode = "PreconditionFailed"
	errConditionalRequestConflict gofakes3.ErrorCode = "ConditionalRequestConflict"
)

type writeConditionsKey struct{}

type writeConditions struct {
	ifMatch     string
	ifNoneMatch string
	failed      bool
	conflicted  bool
	invalid     bool
	guard       *xfsCommitGuard
}

func conditionsFromRequest(r *http.Request) *writeConditions {
	matches := r.Header.Values("If-Match")
	nonmatches := r.Header.Values("If-None-Match")
	if len(matches) == 0 && len(nonmatches) == 0 {
		return nil
	}
	c := &writeConditions{
		ifMatch:     strings.Join(matches, ","),
		ifNoneMatch: strings.Join(nonmatches, ","),
	}
	c.invalid = (len(matches) != 0 && len(splitEntityTags(c.ifMatch)) == 0) ||
		(len(nonmatches) != 0 && len(splitEntityTags(c.ifNoneMatch)) == 0)
	return c
}

func conditionsFromContext(ctx context.Context) *writeConditions {
	conditions, _ := ctx.Value(writeConditionsKey{}).(*writeConditions)
	return conditions
}

func (c *writeConditions) fail() error {
	c.failed = true
	return errPreconditionFailed
}

func (c *writeConditions) conflict() error {
	c.conflicted = true
	return errConditionalRequestConflict
}

// splitEntityTags accepts quoted lists and single unquoted tags used by S3 clients.
func splitEntityTags(value string) []string {
	value = strings.Trim(value, " \t")
	if value == "*" {
		return []string{"*"}
	}
	if value != "" && value[0] != '"' && !strings.HasPrefix(value, "W/") {
		for i := range len(value) {
			c := value[i]
			if c <= 0x20 || c >= 0x7f || c == '"' || c == ',' || c == '*' {
				return nil
			}
		}
		return []string{`"` + value + `"`}
	}
	var tags []string
	for value != "" {
		end := 0
		if strings.HasPrefix(value, "W/") {
			end = 2
		}
		if len(value) <= end || value[end] != '"' {
			return nil
		}
		end++
		for end < len(value) && value[end] != '"' {
			if value[end] < 0x21 || value[end] == 0x7f {
				return nil
			}
			end++
		}
		if end == len(value) {
			return nil
		}
		end++
		tags = append(tags, value[:end])
		value = strings.TrimLeft(value[end:], " \t")
		if value == "" {
			return tags
		}
		if value[0] != ',' {
			return nil
		}
		value = strings.TrimLeft(value[1:], " \t")
		if value == "" {
			return nil
		}
	}
	return tags
}

func ifMatchHolds(value string, exists bool, etag string) bool {
	if value == "" {
		return true
	}
	if !exists {
		return false
	}
	for _, candidate := range splitEntityTags(value) {
		if candidate == "*" {
			return true
		}
		if strings.HasPrefix(candidate, "W/") {
			continue
		}
		if candidate == etag {
			return true
		}
	}
	return false
}

func ifNoneMatchHolds(value string, exists bool, etag string) bool {
	if value == "" || !exists {
		return true
	}
	for _, candidate := range splitEntityTags(value) {
		if candidate == "*" {
			return false
		}
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return false
		}
	}
	return true
}

// gofakes3 serialises these error codes but does not map their HTTP status yet.
type conditionalResponseWriter struct {
	http.ResponseWriter
	conditions *writeConditions
}

func (w conditionalResponseWriter) WriteHeader(status int) {
	if status == http.StatusInternalServerError {
		switch {
		case w.conditions.conflicted:
			status = http.StatusConflict
		case w.conditions.failed:
			status = http.StatusPreconditionFailed
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w conditionalResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func conditionalWriteMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			if conditions := conditionsFromRequest(r); conditions != nil {
				ctx := context.WithValue(r.Context(), writeConditionsKey{}, conditions)
				r = r.WithContext(ctx)
				w = conditionalResponseWriter{ResponseWriter: w, conditions: conditions}
			}
		}
		next.ServeHTTP(w, r)
	})
}

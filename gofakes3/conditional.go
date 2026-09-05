package gofakes3

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// WriteConditions are preconditions on the destination of a write.
// A zero value imposes no conditions. IfMatch is a single ETag, without
// quotes, or "*". IfNoneMatch is either empty or "*".
type WriteConditions struct {
	// IfMatch requires a matching ETag, or an existing object for "*".
	IfMatch string
	// IfNoneMatch requires an absent object for "*".
	IfNoneMatch string
}

// Check returns an S3 error if the destination does not satisfy the conditions.
// The backend must exclude competing mutations until the write is committed.
// An unavailable ETag must not be passed as an empty string for an existing
// object when IfMatch specifies an ETag.
func (c *WriteConditions) Check(exists bool, etag string) error {
	if c == nil {
		return nil
	}
	if c.IfNoneMatch != "" && c.IfNoneMatch != "*" {
		return ErrInvalidArgument
	}
	if c.IfNoneMatch == "*" && exists {
		return ErrPreconditionFailed
	}
	if c.IfMatch != "" {
		if !exists {
			return ErrPreconditionFailed
		}
		if c.IfMatch != "*" {
			if etag == "" {
				return ErrNotImplemented
			}
			if c.IfMatch != etag {
				return ErrPreconditionFailed
			}
		}
	}
	return nil
}

func parseWriteConditions(h http.Header) (*WriteConditions, error) {
	var c WriteConditions
	for name, dst := range map[string]*string{"If-Match": &c.IfMatch, "If-None-Match": &c.IfNoneMatch} {
		values := h.Values(name)
		if len(values) == 0 {
			continue
		}
		if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			return nil, ErrorInvalidArgument(name, strings.Join(values, ","), "Expected a single write condition")
		}
		*dst = strings.TrimSpace(values[0])
	}
	if c.IfMatch == "" && c.IfNoneMatch == "" {
		return nil, nil
	}
	if c.IfMatch != "" && c.IfNoneMatch != "" {
		return nil, ErrorInvalidArgument("If-Match", c.IfMatch, "If-Match and If-None-Match cannot be combined")
	}
	if c.IfNoneMatch != "" && c.IfNoneMatch != "*" {
		return nil, ErrorInvalidArgument("If-None-Match", c.IfNoneMatch, "Expected *")
	}
	if c.IfMatch != "" && c.IfMatch != "*" {
		etag := c.IfMatch
		if strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) && len(etag) >= 2 {
			etag = etag[1 : len(etag)-1]
		}
		if etag == "" || etag == "*" || strings.ContainsAny(etag, "\" ,\t\r\n") || strings.HasPrefix(etag, "W/") {
			return nil, ErrorInvalidArgument("If-Match", c.IfMatch, "Expected a single strong ETag")
		}
		c.IfMatch = etag
	}
	return &c, nil
}

// ConditionalPutObjectBackend supports atomic conditional object uploads.
// Backends must serialize the condition check and write with all mutations
// of the destination, including unconditional writes and deletes.
type ConditionalPutObjectBackend interface {
	PutObjectWithConditions(ctx context.Context, bucketName, objectName string, meta map[string]string, input io.Reader, size int64, conditions *WriteConditions) (PutObjectResult, error)
}

// ConditionalCopyObjectBackend supports atomic conditional object copies.
// Conditions apply to the destination, not the source.
type ConditionalCopyObjectBackend interface {
	CopyObjectWithConditions(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string, meta map[string]string, conditions *WriteConditions) (CopyObjectResult, error)
}

// ConditionalMultipartBackend supports atomic conditional multipart completion.
// Conditions apply at completion, not when the upload is initiated.
type ConditionalMultipartBackend interface {
	CompleteMultipartUploadWithConditions(ctx context.Context, bucketName, objectName string, uploadID UploadID, input *CompleteMultipartUploadRequest, conditions *WriteConditions) (VersionID, string, error)
}

func (g *GoFakeS3) putObjectWithConditions(ctx context.Context, bucket, key string, meta map[string]string, input io.Reader, size int64, conditions *WriteConditions) (PutObjectResult, error) {
	if conditions == nil {
		return g.storage.PutObject(ctx, bucket, key, meta, input, size)
	}
	if b, ok := g.storage.(ConditionalPutObjectBackend); ok {
		return b.PutObjectWithConditions(ctx, bucket, key, meta, input, size, conditions)
	}
	return PutObjectResult{}, ErrNotImplemented
}

// Package backend defines the protocol-neutral boundary for upstream object I/O.
package backend

import (
	"context"
	"io"
	"time"
)

type ObjectInfo struct {
	Path         string
	Size         int64
	ETag         string
	LastModified time.Time
}

// Reader is the minimum read-side contract. Write and list contracts will be
// added only with conformance tests for HTTP and S3 adapter semantics.
type Reader interface {
	Stat(context.Context, string) (ObjectInfo, error)
	Open(context.Context, string, int64, int64) (io.ReadCloser, ObjectInfo, error)
}

// Redirector is optional. A protocol adapter must proxy if a backend cannot
// issue a safe, bounded redirect URL.
type Redirector interface {
	SignedURL(context.Context, string, time.Duration) (string, error)
}

//go:build !rusthttp || !cgo || !linux

package fshttp

import (
	"context"
	"net/http"
	"os"
	"sync"

	"github.com/rclone/rclone/fs"
)

var rustUnavailable sync.Once

func selectRustHTTPTransport(_ context.Context, _ *http.Transport, _ bool) http.RoundTripper {
	if os.Getenv("RCLONE_EXPERIMENTAL_RUST_HTTP") == "1" {
		rustUnavailable.Do(func() {
			fs.Logf(nil, "Rust HTTP is unavailable in this build; build for Linux with CGO_ENABLED=1 and -tags rusthttp to enable it")
		})
	}
	return nil
}

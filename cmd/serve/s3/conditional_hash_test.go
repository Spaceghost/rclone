package s3

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rclone/gofakes3"
	"github.com/rclone/rclone/fs/hash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConditionalUnavailableETag(t *testing.T) {
	for _, mode := range []struct {
		name string
		code gofakes3.ErrorCode
	}{
		{"Disabled", gofakes3.ErrNotImplemented},
		{"ReadError", gofakes3.ErrInternal},
	} {
		t.Run(mode.name, func(t *testing.T) {
			if mode.name == "ReadError" && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("requires Unix file permissions and an unprivileged process")
			}
			for _, test := range []struct {
				name, match, none string
				needsHash         bool
				code              gofakes3.ErrorCode
			}{
				{"MatchEmpty", `""`, "", true, ""},
				{"MatchTag", `"etag"`, "", true, ""},
				{"NoneMatchEmpty", "", `""`, true, ""},
				{"NoneMatchTag", "", `"etag"`, true, ""},
				{"MatchWildcard", " * ", "", false, ""},
				{"NoneMatchWildcard", "", " * ", false, errPreconditionFailed},
			} {
				t.Run(test.name, func(t *testing.T) {
					b, root := newTestBackend(t)
					name := filepath.Join(root, "bucket", "object.txt")
					b.s.etagHashType = hash.MD5
					if mode.name == "Disabled" {
						b.s.etagHashType = hash.None
					} else {
						require.NoError(t, os.Chmod(name, 0000))
						t.Cleanup(func() { assert.NoError(t, os.Chmod(name, 0600)) })
					}
					obj, err := b.HeadObject(context.Background(), "bucket", "object.txt")
					require.NoError(t, err)
					require.NoError(t, obj.Contents.Close())
					require.Empty(t, obj.Hash, "the fixture must not have an ETag")

					cb := newConditionalBackend(b)
					ctx, _ := conditionalContext(test.match, test.none)
					_, err = cb.PutObject(ctx, "bucket", "object.txt", map[string]string{}, strings.NewReader("replacement"), 11)
					code := test.code
					if test.needsHash {
						code = mode.code
					}
					want := "normal object"
					if code == gofakes3.ErrNone {
						require.NoError(t, err)
						want = "replacement"
					} else {
						assert.True(t, gofakes3.HasErrorCode(err, code), "want %s, got %v", code, err)
					}
					require.NoError(t, os.Chmod(name, 0600))
					got, err := os.ReadFile(name)
					require.NoError(t, err)
					assert.Equal(t, want, string(got))
				})
			}
		})
	}
}

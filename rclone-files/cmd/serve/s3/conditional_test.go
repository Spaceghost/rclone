package s3

import (
	"context"
	"encoding/hex"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/gofakes3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConditionalFailurePreservesExisting(t *testing.T) {
	for _, fm := range failureModes {
		t.Run(fm.name, func(t *testing.T) {
			b, f, bucket := newPutTestBackend(t, "", nil)
			ctx := context.Background()
			_, err := b.PutObject(ctx, bucket, "object", map[string]string{}, strings.NewReader("original"), 8)
			require.NoError(t, err)
			node, err := b.HeadObject(ctx, bucket, "object")
			require.NoError(t, err)
			c := &gofakes3.WriteConditions{IfMatch: hex.EncodeToString(node.Hash)}
			_, err = b.PutObjectWithConditions(ctx, bucket, "object", map[string]string{}, &errorReader{data: []byte("partial"), err: fm.readerErr}, 100, c)
			require.ErrorIs(t, err, fm.wantErr)
			assert.Equal(t, "original", string(readObject(t, f, bucket, "object")))
			requireOnly(t, f, bucket, "object")
			assert.Equal(t, int64(8), node.Size)
		})
	}
}

func TestConditionalMutationLocks(t *testing.T) {
	for _, method := range []string{"Put", "Copy", "SelfCopy", "Delete", "DeleteMulti", "Complete"} {
		t.Run(method, func(t *testing.T) {
			b, _, bucket := newPutTestBackend(t, "", nil)
			ctx := context.Background()
			for _, key := range []string{"object", "source"} {
				_, err := b.PutObject(ctx, bucket, key, map[string]string{}, strings.NewReader("original"), 8)
				require.NoError(t, err)
			}
			var id gofakes3.UploadID
			var parts gofakes3.CompleteMultipartUploadRequest
			if method == "Complete" {
				var err error
				id, err = b.CreateMultipartUpload(ctx, bucket, "object", map[string]string{})
				require.NoError(t, err)
				etag, err := b.UploadPart(ctx, bucket, "object", id, 1, 4, strings.NewReader("part"))
				require.NoError(t, err)
				parts.Parts = append(parts.Parts, gofakes3.CompletedPart{PartNumber: 1, ETag: etag})
			}
			VFS, err := b.s.getVFS(ctx)
			require.NoError(t, err)
			unlock, err := b.lockObject(ctx, VFS, bucket+"/object")
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				var err error
				switch method {
				case "Put":
					_, err = b.PutObject(ctx, bucket, "object", map[string]string{}, strings.NewReader("new"), 3)
				case "Copy":
					_, err = b.CopyObject(ctx, bucket, "source", bucket, "object", map[string]string{})
				case "SelfCopy":
					_, err = b.CopyObject(ctx, bucket, "object", bucket, "object", map[string]string{})
				case "Delete":
					_, err = b.DeleteObject(ctx, bucket, "object")
				case "DeleteMulti":
					_, err = b.DeleteMulti(ctx, bucket, "object")
				case "Complete":
					_, _, err = b.CompleteMultipartUpload(ctx, bucket, "object", id, &parts)
				}
				done <- err
			}()
			waiting := assert.Eventually(t, func() bool {
				b.objectLocksMu.Lock()
				defer b.objectLocksMu.Unlock()
				return b.objectLocks[objectLockKey{VFS, bucket + "/object"}].refs == 2
			}, 5*time.Second, time.Millisecond)
			unlock()
			require.True(t, waiting)
			require.NoError(t, <-done)
			assert.Empty(t, b.objectLocks)
		})
	}
}

func TestConditionalLockCancellation(t *testing.T) {
	b, _, bucket := newPutTestBackend(t, "", nil)
	VFS, err := b.s.getVFS(context.Background())
	require.NoError(t, err)
	unlock, err := b.lockObject(context.Background(), VFS, bucket+"/object")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = b.lockObject(ctx, VFS, bucket+"/object")
	require.ErrorIs(t, err, context.Canceled)
	other, err := b.lockObject(context.Background(), VFS, bucket+"/other")
	require.NoError(t, err)
	other()
	unlock()
	assert.Empty(t, b.objectLocks)
}

func TestConditionalRejectDoesNotReadBody(t *testing.T) {
	b, f, bucket := newPutTestBackend(t, "", nil)
	ctx := context.Background()
	_, err := b.PutObject(ctx, bucket, "object", map[string]string{}, strings.NewReader("original"), 8)
	require.NoError(t, err)
	_, err = b.PutObjectWithConditions(ctx, bucket, "object", map[string]string{}, &errorReader{err: io.ErrUnexpectedEOF}, 100, &gofakes3.WriteConditions{IfNoneMatch: "*"})
	require.ErrorIs(t, err, gofakes3.ErrPreconditionFailed)
	assert.Equal(t, "original", string(readObject(t, f, bucket, "object")))
}

func TestConditionalLockCaseFold(t *testing.T) {
	b, _, bucket := newPutTestBackend(t, "", nil)
	VFS, err := b.s.getVFS(context.Background())
	require.NoError(t, err)
	VFS.Opt.CaseInsensitive = true
	for _, pair := range [][2]string{{"K", "k"}, {"K", "K"}, {"Σ", "ς"}, {"σ", "ς"}} {
		unlock, err := b.lockObject(context.Background(), VFS, bucket+"/"+pair[0])
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		other, err := b.lockObject(ctx, VFS, bucket+"/"+pair[1])
		if other != nil {
			other()
		}
		cancel()
		unlock()
		require.ErrorIs(t, err, context.DeadlineExceeded, "%q and %q", pair[0], pair[1])
	}
	assert.Empty(t, b.objectLocks)
}

func TestConditionalParentCleanup(t *testing.T) {
	b, f, bucket := newPutTestBackend(t, "", nil)
	ctx := context.Background()
	_, err := b.PutObject(ctx, bucket, "parent/child", map[string]string{}, strings.NewReader("child"), 5)
	require.NoError(t, err)
	VFS, err := b.s.getVFS(ctx)
	require.NoError(t, err)
	fp := bucket + "/parent"
	unlock, err := b.lockObject(ctx, VFS, fp)
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(unlock) }
	defer release()
	done := make(chan error, 1)
	go func() { done <- b.deleteObject(ctx, bucket, "parent/child") }()
	require.Eventually(t, func() bool {
		b.objectLocksMu.Lock()
		defer b.objectLocksMu.Unlock()
		return b.objectLocks[objectLockKey{VFS, fp}].refs == 2
	}, 5*time.Second, time.Millisecond)

	// Commit a replacement at the locked parent while deletion waits to
	// clean it up. Cleanup must not remove this file as an empty directory.
	require.NoError(t, VFS.Remove(fp))
	fh, err := VFS.Create(fp)
	require.NoError(t, err)
	_, err = io.WriteString(fh, "replacement")
	require.NoError(t, err)
	require.NoError(t, fh.Close())
	release()
	require.NoError(t, <-done)
	assert.Equal(t, "replacement", string(readObject(t, f, bucket, "parent")))
	assert.Empty(t, b.objectLocks)
}

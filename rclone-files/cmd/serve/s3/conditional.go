package s3

import (
	"context"
	"strings"
	"unicode"

	"github.com/rclone/gofakes3"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/vfs"
)

type objectLockKey struct {
	vfs  *vfs.VFS
	path string
}

type objectLock struct {
	token chan struct{}
	refs  int
}

// lockObject excludes competing mutations of one VFS path. Idle locks are
// discarded so the table grows with concurrent requests, not stored objects.
func (b *s3Backend) lockObject(ctx context.Context, VFS *vfs.VFS, fp string) (func(), error) {
	if VFS.Opt.CaseInsensitive || VFS.Fs().Features().CaseInsensitive {
		fp = strings.Map(func(r rune) rune {
			for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
				if next < r {
					r = next
				}
			}
			return r
		}, fp)
	}
	key := objectLockKey{VFS, fp}
	b.objectLocksMu.Lock()
	if b.objectLocks == nil {
		b.objectLocks = make(map[objectLockKey]*objectLock)
	}
	lock := b.objectLocks[key]
	if lock == nil {
		lock = &objectLock{token: make(chan struct{}, 1)}
		b.objectLocks[key] = lock
	}
	lock.refs++
	b.objectLocksMu.Unlock()

	release := func() {
		b.objectLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(b.objectLocks, key)
		}
		b.objectLocksMu.Unlock()
	}
	select {
	case lock.token <- struct{}{}:
		unlock := func() {
			<-lock.token
			release()
		}
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, err
		}
		return unlock, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// checkWriteConditions must be called with the destination locked.
func (b *s3Backend) checkWriteConditions(VFS *vfs.VFS, fp string, conditions *gofakes3.WriteConditions) error {
	if conditions == nil {
		return nil
	}
	// Without staging, a multipart upload can change the destination before
	// CompleteMultipartUpload supplies its conditions.
	if !operations.CanServerSideMove(VFS.Fs()) {
		return gofakes3.ErrorMessage(gofakes3.ErrNotImplemented, "Conditional writes require server-side move or copy")
	}
	node, err := VFS.Stat(fp)
	if err == vfs.ENOENT {
		return conditions.Check(false, "")
	}
	if err != nil {
		return err
	}
	if !node.IsFile() {
		return gofakes3.ErrPreconditionFailed
	}
	var etag string
	if conditions.IfMatch != "" && conditions.IfMatch != "*" {
		etag = getFileHash(node, b.s.etagHashType)
		if etag == "" {
			return gofakes3.ErrorMessage(gofakes3.ErrNotImplemented, "Conditional writes require an available ETag")
		}
	}
	return conditions.Check(true, etag)
}

var (
	_ gofakes3.ConditionalPutObjectBackend  = (*s3Backend)(nil)
	_ gofakes3.ConditionalCopyObjectBackend = (*s3Backend)(nil)
	_ gofakes3.ConditionalMultipartBackend  = (*s3Backend)(nil)
)

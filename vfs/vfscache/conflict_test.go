package vfscache

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newWritebackConflict(t *testing.T, name string) (r *fstest.Run, c *Cache, remoteContents, localContents string, item *Item) {
	r, c = newItemTestCache(t)
	ci := fs.GetConfig(c.ctx)
	oldInplace := ci.Inplace
	ci.Inplace = true
	t.Cleanup(func() { ci.Inplace = oldInplace })

	remoteContents, obj, item := newFile(t, r, c, name)
	localContents = "changed" + remoteContents[len("changed"):]
	require.NoError(t, item.Open(objectChangedOnUpdate{Object: obj}))
	_, err := item.WriteAt([]byte("changed"), 0)
	require.NoError(t, err)
	require.ErrorIs(t, item.Close(nil), fs.ErrorObjectChanged)
	return r, c, remoteContents, localContents, item
}

type blockingUpdateFs struct {
	fs.Fs
	started chan struct{}
	finish  chan struct{}
}

func (f blockingUpdateFs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	o, err := f.Fs.NewObject(ctx, remote)
	if err != nil {
		return nil, err
	}
	return blockingUpdateObject{Object: o, started: f.started, finish: f.finish}, nil
}

type blockingUpdateObject struct {
	fs.Object
	started chan struct{}
	finish  chan struct{}
}

func (o blockingUpdateObject) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	close(o.started)
	select {
	case <-o.finish:
		return o.Object.Update(ctx, in, src, options...)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCacheConflicts(t *testing.T) {
	_, c, _, _, _ := newWritebackConflict(t, "existing.txt")
	conflicts := c.Conflicts()
	require.Len(t, conflicts, 1)
	assert.Equal(t, "existing.txt", conflicts[0].Name)
	assert.Equal(t, int64(100), conflicts[0].Size)
	assert.False(t, conflicts[0].ModTime.IsZero())
}

func TestCacheResolveConflictKeepRemote(t *testing.T) {
	r, c, remoteContents, _, _ := newWritebackConflict(t, "existing.txt")

	conflictFile, err := c.ResolveConflict(context.Background(), "existing.txt", "keep-remote")
	require.NoError(t, err)
	assert.Empty(t, conflictFile)
	assert.Empty(t, c.Conflicts())
	assertPathNotExist(t, c.toOSPath("existing.txt"))
	assertPathNotExist(t, c.toOSPathMeta("existing.txt"))
	checkObject(t, r, "existing.txt", remoteContents)
}

func TestCacheResolveConflictKeepLocal(t *testing.T) {
	r, c, _, localContents, item := newWritebackConflict(t, "existing.txt")

	conflictFile, err := c.ResolveConflict(context.Background(), "existing.txt", "keep-local")
	require.NoError(t, err)
	assert.Empty(t, conflictFile)
	assert.False(t, item.IsDirty())
	assert.Empty(t, c.Conflicts())
	checkObject(t, r, "existing.txt", localContents)
}

func TestCacheResolveConflictKeepLocalConcurrent(t *testing.T) {
	_, c, _, _, _ := newWritebackConflict(t, "existing.txt")
	started := make(chan struct{})
	finish := make(chan struct{})
	c.fremote = blockingUpdateFs{Fs: c.fremote, started: started, finish: finish}

	done := make(chan error, 1)
	go func() {
		_, err := c.ResolveConflict(context.Background(), "existing.txt", "keep-local")
		done <- err
	}()
	<-started

	_, err := c.ResolveConflict(context.Background(), "existing.txt", "keep-local")
	require.EqualError(t, err, `writeback conflict "existing.txt" not found`)
	close(finish)
	require.NoError(t, <-done)
}

func TestCacheResolveConflictKeepBoth(t *testing.T) {
	r, c, remoteContents, localContents, _ := newWritebackConflict(t, "existing.txt")

	conflictFile, err := c.ResolveConflict(context.Background(), "existing.txt", "keep-both")
	require.NoError(t, err)
	assert.Equal(t, "existing.conflict1.txt", conflictFile)
	require.Eventually(t, func() bool {
		item := c.DirtyItem(conflictFile)
		return item == nil
	}, time.Second, 10*time.Millisecond)
	assert.Empty(t, c.Conflicts())
	checkObject(t, r, "existing.txt", remoteContents)
	checkObject(t, r, conflictFile, localContents)
	assert.Contains(t, avInfos, avInfo{Remote: conflictFile, Size: 100})
}

func TestCacheResolveConflictInvalidAction(t *testing.T) {
	_, c, _, _, _ := newWritebackConflict(t, "existing.txt")
	_, err := c.ResolveConflict(context.Background(), "existing.txt", "merge")
	require.EqualError(t, err, `unknown conflict action "merge"`)
}

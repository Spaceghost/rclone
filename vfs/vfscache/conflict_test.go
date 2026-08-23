package vfscache

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeWritebackConflict(t *testing.T, r *fstest.Run, c *Cache, name string) (remoteContents, localContents string, item *Item) {
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
	return remoteContents, localContents, item
}

func newWritebackConflict(t *testing.T, name string) (r *fstest.Run, c *Cache, remoteContents, localContents string, item *Item) {
	r, c = newItemTestCache(t)
	remoteContents, localContents, item = makeWritebackConflict(t, r, c, name)
	return r, c, remoteContents, localContents, item
}

func newBlockingWritebackConflict(t *testing.T, name string, started, finish chan struct{}) (r *fstest.Run, c *Cache, remoteContents, localContents string, item *Item) {
	opt := vfscommon.Opt
	opt.CachePollInterval = 0
	opt.WriteBack = 0
	opt.HandleCaching = 0

	r = fstest.NewRun(t)
	ctx, cancel := context.WithCancel(context.Background())
	avInfos = nil
	var err error
	c, err = New(ctx, blockingUpdateFs{Fs: r.Fremote, started: started, finish: finish}, &opt, addVirtual)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, c.CleanUp())
		assertPathNotExist(t, c.root)
		cancel()
	})

	remoteContents, localContents, item = makeWritebackConflict(t, r, c, name)
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

func TestConflictName(t *testing.T) {
	assert.Equal(t, "file-workstation.txt", conflictName("file.txt", "workstation", 1))
	assert.Equal(t, "archive.tar-workstation-2.gz", conflictName("archive.tar.gz", "workstation", 2))
	assert.Equal(t, ".profile-workstation", conflictName(".profile", "workstation", 1))
	assert.Equal(t, "file-work-station.txt", conflictName("file.txt", "work/station", 1))
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

	conflictFile, err := c.ResolveConflict(context.Background(), "existing.txt", conflictKeepRemote)
	require.NoError(t, err)
	assert.Empty(t, conflictFile)
	assert.Empty(t, c.Conflicts())
	assertPathNotExist(t, c.toOSPath("existing.txt"))
	assertPathNotExist(t, c.toOSPathMeta("existing.txt"))
	checkObject(t, r, "existing.txt", remoteContents)
}

func TestCacheResolveConflictKeepLocal(t *testing.T) {
	r, c, _, localContents, item := newWritebackConflict(t, "existing.txt")

	conflictFile, err := c.ResolveConflict(context.Background(), "existing.txt", conflictKeepLocal)
	require.NoError(t, err)
	assert.Empty(t, conflictFile)
	assert.False(t, item.IsDirty())
	assert.Empty(t, c.Conflicts())
	checkObject(t, r, "existing.txt", localContents)
}

func TestCacheResolveConflictKeepLocalConcurrent(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	_, c, _, _, _ := newBlockingWritebackConflict(t, "existing.txt", started, finish)

	done := make(chan error, 1)
	go func() {
		_, err := c.ResolveConflict(context.Background(), "existing.txt", conflictKeepLocal)
		done <- err
	}()
	<-started

	_, err := c.ResolveConflict(context.Background(), "existing.txt", conflictKeepLocal)
	require.EqualError(t, err, `writeback conflict "existing.txt" is being resolved`)
	close(finish)
	require.NoError(t, <-done)
}

func TestCacheResolveConflictBlocksOpen(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	_, c, _, _, item := newBlockingWritebackConflict(t, "existing.txt", started, finish)
	remote, err := c.fremote.NewObject(context.Background(), "existing.txt")
	require.NoError(t, err)

	resolved := make(chan error, 1)
	go func() {
		_, err := c.ResolveConflict(context.Background(), "existing.txt", conflictKeepLocal)
		resolved <- err
	}()
	<-started

	opened := make(chan error, 1)
	go func() {
		opened <- item.Open(remote)
	}()
	select {
	case err := <-opened:
		t.Fatalf("Open returned during conflict resolution: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(finish)
	require.NoError(t, <-resolved)
	require.NoError(t, <-opened)
	require.NoError(t, item.Close(nil))
}

func TestCacheResolveConflictKeepBoth(t *testing.T) {
	r, c, remoteContents, localContents, _ := newWritebackConflict(t, "existing.txt")
	hostname, err := os.Hostname()
	require.NoError(t, err)

	conflictFile, err := c.ResolveConflict(context.Background(), "existing.txt", conflictKeepBoth)
	require.NoError(t, err)
	assert.Equal(t, conflictName("existing.txt", hostname, 1), conflictFile)
	assert.Empty(t, c.Conflicts())
	assertPathNotExist(t, c.toOSPath("existing.txt"))
	assertPathNotExist(t, c.toOSPathMeta("existing.txt"))
	checkObject(t, r, "existing.txt", remoteContents)
	checkObject(t, r, conflictFile, localContents)
	assert.Contains(t, avInfos, avInfo{Remote: conflictFile, Size: 100})
}

func TestCacheResolveConflictKeepBothNumbered(t *testing.T) {
	r, c, _, localContents, _ := newWritebackConflict(t, "existing.txt")
	hostname, err := os.Hostname()
	require.NoError(t, err)
	occupied := conflictName("existing.txt", hostname, 1)
	r.WriteObject(context.Background(), occupied, "occupied", time.Now())

	conflictFile, err := c.ResolveConflict(context.Background(), "existing.txt", conflictKeepBoth)
	require.NoError(t, err)
	assert.Equal(t, conflictName("existing.txt", hostname, 2), conflictFile)
	checkObject(t, r, conflictFile, localContents)
}

func TestCacheResolveConflictInvalidAction(t *testing.T) {
	_, c, _, _, _ := newWritebackConflict(t, "existing.txt")
	_, err := c.ResolveConflict(context.Background(), "existing.txt", "merge")
	require.EqualError(t, err, `unknown conflict action "merge"`)
	assert.Len(t, c.Conflicts(), 1)
}

func TestCacheResolveConflictDryRun(t *testing.T) {
	r, c, remoteContents, localContents, _ := newWritebackConflict(t, "existing.txt")
	ctx, ci := fs.AddConfig(context.Background())
	ci.DryRun = true

	_, err := c.ResolveConflict(ctx, "existing.txt", conflictKeepRemote)
	require.EqualError(t, err, "can't resolve writeback conflicts with --dry-run")
	assert.Len(t, c.Conflicts(), 1)
	checkObject(t, r, "existing.txt", remoteContents)

	data, err := os.ReadFile(c.toOSPath("existing.txt"))
	require.NoError(t, err)
	assert.Equal(t, localContents, string(data))
}

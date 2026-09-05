package vfscache

import (
	"context"
	"io"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type objectChangedOnUpdate struct {
	fs.Object
}

func (o objectChangedOnUpdate) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	return fs.ErrorObjectChanged
}

func TestItemObjectChanged(t *testing.T) {
	r, c := newItemTestCache(t)
	ci := fs.GetConfig(c.ctx)
	oldInplace := ci.Inplace
	ci.Inplace = true
	defer func() { ci.Inplace = oldInplace }()

	contents, obj, item := newFile(t, r, c, "existing")
	require.NoError(t, item.Open(objectChangedOnUpdate{Object: obj}))
	_, err := item.WriteAt([]byte("changed"), 0)
	require.NoError(t, err)
	require.ErrorIs(t, item.Close(nil), fs.ErrorObjectChanged)

	item.mu.Lock()
	assert.True(t, item.info.Dirty)
	assert.True(t, item.info.Conflict)
	item.mu.Unlock()
	checkObject(t, r, "existing", contents)

	reloaded := newItem(c, "existing")
	c.put("existing", reloaded)
	require.NoError(t, reloaded.reload(c.ctx))

	reloaded.mu.Lock()
	assert.True(t, reloaded.info.Dirty)
	assert.True(t, reloaded.info.Conflict)
	reloaded.mu.Unlock()
	checkObject(t, r, "existing", contents)
}

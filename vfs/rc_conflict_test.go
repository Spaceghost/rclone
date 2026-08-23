package vfs

import (
	"testing"

	"github.com/rclone/rclone/fs/rc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRcConflictCalls(t *testing.T) {
	for _, path := range []string{"vfs/conflicts", "vfs/resolve-conflict"} {
		call := rc.Calls.Get(path)
		require.NotNil(t, call)
		assert.False(t, call.NoAuth)
	}
}

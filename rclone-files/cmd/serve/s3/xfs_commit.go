package s3

import (
	"errors"
	"os"
	"syscall"

	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

// tryXFSCommit atomically commits staged into dst when both files are local XFS files.
func tryXFSCommit(VFS *vfs.VFS, stagedPath, dstPath string, length int64) (bool, error) {
	if VFS.Opt.CacheMode >= vfscommon.CacheModeMinimal {
		return false, nil
	}
	if !VFS.Fs().Features().IsLocal {
		return false, nil
	}
	staged, err := os.OpenFile(stagedPath, os.O_RDWR, 0)
	if err != nil {
		return false, nil
	}
	defer staged.Close()
	dst, err := os.OpenFile(dstPath, os.O_RDWR, 0)
	if err != nil {
		return false, nil
	}
	defer dst.Close()

	used, err := local.XFSCommitRange(staged, dst, length)
	if errors.Is(err, syscall.EBUSY) {
		return true, errXFSCommitConflict
	}
	return used, err
}

var errXFSCommitConflict = errors.New("XFS conditional range commit conflict")

package s3

import (
	"errors"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/operations"
	filelib "github.com/rclone/rclone/lib/file"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

type localPathProvider interface {
	LocalPath(remote string) (string, error)
}

func xfsCommitRangeCapability(fsys fs.Fs) (bool, error) {
	local, ok := fsys.(localPathProvider)
	if !ok {
		return false, nil
	}
	root, err := local.LocalPath("")
	if errors.Is(err, fs.ErrorNotImplemented) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return filelib.IsXFS(root)
}

func xfsCommitRangeVFSCapability(v *vfs.VFS) (bool, error) {
	if v.Opt.CacheMode != vfscommon.CacheModeOff || !operations.CanServerSideMove(v.Fs()) {
		return false, nil
	}
	return xfsCommitRangeCapability(v.Fs())
}

func xfsCommitUnavailable(err error) bool {
	return errors.Is(err, filelib.ErrXFSCommitRangeUnsupported)
}

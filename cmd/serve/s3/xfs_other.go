//go:build !linux || mips || mipsle || mips64 || mips64le || ppc64 || ppc64le

package s3

import (
	"github.com/rclone/rclone/fs"
	filelib "github.com/rclone/rclone/lib/file"
)

type xfsCommitGuard struct{}

func startXFSCommitGuard(fsys fs.Fs, remote string) (*xfsCommitGuard, error) {
	return nil, filelib.ErrXFSCommitRangeUnsupported
}

func (g *xfsCommitGuard) commit(fsys fs.Fs, stagingRemote string, dsync bool) error {
	return filelib.ErrXFSCommitRangeUnsupported
}

func (g *xfsCommitGuard) close() error {
	return nil
}

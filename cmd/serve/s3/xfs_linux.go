//go:build linux && !mips && !mipsle && !mips64 && !mips64le && !ppc64 && !ppc64le

package s3

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/rclone/rclone/fs"
	filelib "github.com/rclone/rclone/lib/file"
	"golang.org/x/sys/unix"
)

type xfsCommitGuard struct {
	root   *os.Root
	path   string
	target *os.File
	token  *filelib.XFSCommitToken
}

func startXFSCommitGuard(fsys fs.Fs, remote string) (*xfsCommitGuard, error) {
	local, ok := fsys.(localPathProvider)
	if !ok {
		return nil, filelib.ErrXFSCommitRangeUnsupported
	}
	base, err := local.LocalPath("")
	if err != nil {
		return nil, err
	}
	name, err := local.LocalPath(remote)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(base, name)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	g := &xfsCommitGuard{root: root, path: rel}
	info, err := root.Lstat(rel)
	if err == nil && !info.Mode().IsRegular() {
		err = filelib.ErrXFSCommitRangeUnsupported
	}
	if err == nil {
		g.target, err = root.OpenFile(rel, os.O_RDWR|unix.O_NOFOLLOW, 0)
	}
	if err == nil {
		g.token, err = filelib.StartXFSCommit(g.target)
	}
	if err == nil {
		// A rename replaces one name, whereas an exchange changes every hard link.
		var st unix.Stat_t
		err = unix.Fstat(int(g.target.Fd()), &st)
		if err == nil && (st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1) {
			err = filelib.ErrXFSCommitRangeUnsupported
		}
	}
	if err != nil {
		_ = g.close()
		return nil, err
	}
	return g, nil
}

func (g *xfsCommitGuard) commit(fsys fs.Fs, stagingRemote string, dsync bool) error {
	local := fsys.(localPathProvider)
	base, err := local.LocalPath("")
	if err != nil {
		return err
	}
	name, err := local.LocalPath(stagingRemote)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(base, name)
	if err != nil {
		return err
	}
	// The token protects an inode, not its directory entry.
	current, err := g.root.Lstat(g.path)
	if os.IsNotExist(err) {
		return syscall.EBUSY
	}
	if err != nil {
		return err
	}
	held, err := g.target.Stat()
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(current, held) {
		return syscall.EBUSY
	}
	staging, err := g.root.OpenFile(rel, os.O_RDWR|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = staging.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(int(staging.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return syscall.EINVAL
	}
	return filelib.CommitXFSRange(g.target, staging, g.token, dsync)
}

func (g *xfsCommitGuard) close() error {
	if g == nil {
		return nil
	}
	var err error
	if g.target != nil {
		err = g.target.Close()
		g.target = nil
	}
	if g.root != nil {
		closeErr := g.root.Close()
		g.root = nil
		if err == nil {
			err = closeErr
		}
	}
	return err
}

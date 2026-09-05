//go:build linux

package local

import (
	"errors"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	xfsSuperMagic     = 0x58465342
	xfsIOCStartCommit = 0x80585882
	xfsIOCCommitRange = 0x40585883
)

type xfsCommitRange struct {
	File1FD        int32
	Pad            uint32
	File1Offset    uint64
	File2Offset    uint64
	Length         uint64
	Flags          uint64
	File2Freshness [5]uint64
}

// XFSCommitRange atomically exchanges staged data into dst if dst is unchanged.
func XFSCommitRange(staged, dst *os.File, length int64) (bool, error) {
	if length < 0 {
		return false, syscall.EINVAL
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(dst.Fd()), &stat); err != nil {
		return false, err
	}
	if uint64(stat.Type) != xfsSuperMagic {
		return false, nil
	}

	arg := xfsCommitRange{
		File1FD: int32(staged.Fd()),
		Length:  uint64(length),
	}
	if err := xfsCommitIoctl(dst, xfsIOCStartCommit, &arg); err != nil {
		if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EOPNOTSUPP) {
			return false, nil
		}
		return true, err
	}
	if err := xfsCommitIoctl(dst, xfsIOCCommitRange, &arg); err != nil {
		return true, err
	}
	return true, nil
}

func xfsCommitIoctl(file *os.File, request uintptr, arg *xfsCommitRange) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), request, uintptr(unsafe.Pointer(arg)))
	if errno != 0 {
		return errno
	}
	return nil
}

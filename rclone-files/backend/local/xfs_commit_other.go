//go:build !linux

package local

import "os"

// XFSCommitRange reports that XFS conditional range commit is unavailable.
func XFSCommitRange(staged, dst *os.File, length int64) (bool, error) {
	return false, nil
}

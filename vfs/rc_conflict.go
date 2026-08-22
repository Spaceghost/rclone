package vfs

import (
	"context"
	"errors"
	"fmt"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
)

func init() {
	rc.Add(rc.Call{
		Path:   "vfs/conflicts",
		NoAuth: true,
		Title:  "List VFS writeback conflicts.",
		Help: `
This lists files whose cached local revision could not be written
because the remote file changed.

The returned entries contain the file name, size, and modification
time. Use vfs/resolve-conflict to choose which revision to keep.
` + getVFSHelp,
		Fn: rcConflicts,
	})
	rc.Add(rc.Call{
		Path:  "vfs/resolve-conflict",
		Title: "Resolve a VFS writeback conflict.",
		Help: `
This resolves a writeback conflict previously returned by
vfs/conflicts.

It takes the following parameters:

- file - the conflicted file name
- action - keep-local, keep-remote, or keep-both

keep-local immediately retries the cached local revision against the
current remote revision. If the remote changes again, the write
becomes a new conflict.

keep-remote discards the cached local revision.

keep-both leaves the current remote file in place and queues the
cached local revision under a numbered .conflict name.
` + getVFSHelp,
		Fn: rcResolveConflict,
	})
}

func rcConflicts(ctx context.Context, in rc.Params) (out rc.Params, err error) {
	vfs, err := getVFS(in)
	if err != nil {
		return nil, err
	}
	if vfs.cache == nil {
		return nil, rc.NewErrParamInvalid(errors.New("can't call this unless using the VFS cache"))
	}
	for key, value := range in {
		return nil, fmt.Errorf("invalid parameter: %s=%v", key, value)
	}
	return rc.Params{"conflicts": vfs.cache.Conflicts()}, nil
}

func rcResolveConflict(ctx context.Context, in rc.Params) (out rc.Params, err error) {
	vfs, err := getVFS(in)
	if err != nil {
		return nil, err
	}
	if vfs.cache == nil {
		return nil, rc.NewErrParamInvalid(errors.New("can't call this unless using the VFS cache"))
	}

	file, err := in.GetString("file")
	if err != nil {
		return nil, err
	}
	delete(in, "file")
	action, err := in.GetString("action")
	if err != nil {
		return nil, err
	}
	delete(in, "action")
	for key, value := range in {
		return nil, fmt.Errorf("invalid parameter: %s=%v", key, value)
	}

	conflictFile, err := vfs.cache.ResolveConflict(ctx, file, action)
	if err != nil {
		return nil, err
	}
	vfs.root.ForgetPath(file, fs.EntryObject)

	out = rc.Params{
		"action": action,
		"file":   file,
	}
	if conflictFile != "" {
		out["conflictFile"] = conflictFile
	}
	return out, nil
}

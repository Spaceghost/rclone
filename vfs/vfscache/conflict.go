package vfscache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/vfs/vfscache/writeback"
)

const (
	conflictKeepLocal  = "keep-local"
	conflictKeepRemote = "keep-remote"
	conflictKeepBoth   = "keep-both"
)

// ConflictInfo describes a cached writeback conflict.
type ConflictInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// Conflicts returns the unresolved writeback conflicts.
func (c *Cache) Conflicts() (conflicts []ConflictInfo) {
	c.mu.Lock()
	for _, item := range c.item {
		item.mu.Lock()
		if item.info.Conflict {
			conflicts = append(conflicts, ConflictInfo{
				Name:    item.name,
				Size:    item.info.Size,
				ModTime: item.info.ModTime,
			})
		}
		item.mu.Unlock()
	}
	c.mu.Unlock()

	sort.Slice(conflicts, func(i, j int) bool {
		return conflicts[i].Name < conflicts[j].Name
	})
	return conflicts
}

func (c *Cache) claimConflict(name string) (*Item, writeback.Handle, error) {
	c.mu.Lock()
	item := c.item[name]
	if item != nil {
		item.mu.Lock()
	}
	c.mu.Unlock()
	if item == nil {
		return nil, 0, fmt.Errorf("writeback conflict %q not found", name)
	}
	defer item.mu.Unlock()
	if item.resolving != nil {
		return nil, 0, fmt.Errorf("writeback conflict %q is being resolved", name)
	}
	if !item.info.Conflict {
		return nil, 0, fmt.Errorf("writeback conflict %q not found", name)
	}
	if item.opens != 0 {
		return nil, 0, fmt.Errorf("writeback conflict %q is in use", name)
	}

	item.resolving = make(chan struct{})
	item.info.Conflict = false
	id := item.writeBackID
	item.writeBackID = 0
	return item, id, nil
}

func (item *Item) finishConflictResolution() {
	item.mu.Lock()
	close(item.resolving)
	item.resolving = nil
	item.mu.Unlock()
}

func (item *Item) restoreConflict(err error) error {
	item.mu.Lock()
	item.info.Conflict = true
	item.modified = false
	saveErr := item._save()
	item.mu.Unlock()
	if saveErr != nil {
		return fmt.Errorf("%w (failed to restore conflict state: %v)", err, saveErr)
	}
	return err
}

func conflictName(name, hostname string, n int) string {
	hostname = strings.ReplaceAll(hostname, "/", "-")
	dir, leaf := path.Split(name)
	ext := path.Ext(leaf)
	if ext == leaf {
		ext = ""
	}
	base := strings.TrimSuffix(leaf, ext)
	suffix := "-" + hostname
	if n > 1 {
		suffix += fmt.Sprintf("-%d", n)
	}
	return path.Join(dir, base+suffix+ext)
}

func (c *Cache) nextConflictName(ctx context.Context, name string) (string, error) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "rclone"
	}
	for n := 1; ; n++ {
		candidate := conflictName(name, hostname, n)
		c.mu.Lock()
		_, cached := c.item[candidate]
		c.mu.Unlock()
		if cached {
			continue
		}
		_, err := c.fremote.NewObject(ctx, candidate)
		if err == nil {
			continue
		}
		if !errors.Is(err, fs.ErrorObjectNotFound) {
			return "", fmt.Errorf("failed to check conflict file %q: %w", candidate, err)
		}
		return candidate, nil
	}
}

func (c *Cache) keepLocalConflict(ctx context.Context, name string, item *Item) error {
	remote, err := c.fremote.NewObject(ctx, name)
	if errors.Is(err, fs.ErrorObjectNotFound) {
		remote = nil
	} else if err != nil {
		return item.restoreConflict(fmt.Errorf("failed to read current remote file %q: %w", name, err))
	}

	item.mu.Lock()
	oldObject := item.o
	oldFingerprint := item.info.Fingerprint
	item.o = remote
	item.info.Fingerprint = ""
	item._updateFingerprint()
	item.modified = true
	err = item._store(ctx, nil)
	if err == nil {
		item.modified = false
		item.mu.Unlock()
		return nil
	}
	item.o = oldObject
	item.info.Fingerprint = oldFingerprint
	item.info.Conflict = true
	item.modified = false
	saveErr := item._save()
	item.mu.Unlock()
	if saveErr != nil {
		return fmt.Errorf("%w (failed to restore conflict state: %v)", err, saveErr)
	}
	return err
}

func (c *Cache) keepBothConflict(ctx context.Context, name string, item *Item) (string, error) {
	newName, err := c.nextConflictName(ctx, name)
	if err != nil {
		return "", item.restoreConflict(err)
	}
	cacheObject, err := c.fcache.NewObject(ctx, name)
	if err != nil {
		return "", item.restoreConflict(fmt.Errorf("failed to find cached file %q: %w", name, err))
	}
	remoteObject, err := operations.Copy(ctx, c.fremote, nil, newName, cacheObject)
	if err != nil {
		return "", item.restoreConflict(fmt.Errorf("failed to write conflict copy %q: %w", newName, err))
	}

	c.Remove(name)
	if c.avFn != nil {
		if err := c.avFn(newName, remoteObject.Size(), false); err != nil {
			fs.Errorf(newName, "vfs cache: failed to add conflict copy to VFS: %v", err)
		}
	}
	return newName, nil
}

// ResolveConflict resolves a cached writeback conflict.
func (c *Cache) ResolveConflict(ctx context.Context, name, action string) (conflictFile string, err error) {
	name = clean(name)
	if name == "" {
		return "", errors.New("conflict file name is empty")
	}
	switch action {
	case conflictKeepLocal, conflictKeepRemote, conflictKeepBoth:
	default:
		return "", fmt.Errorf("unknown conflict action %q", action)
	}
	if fs.GetConfig(ctx).DryRun {
		return "", errors.New("can't resolve writeback conflicts with --dry-run")
	}

	item, id, err := c.claimConflict(name)
	if err != nil {
		return "", err
	}
	defer item.finishConflictResolution()
	c.writeback.Remove(id)

	switch action {
	case conflictKeepLocal:
		return "", c.keepLocalConflict(ctx, name, item)
	case conflictKeepRemote:
		c.Remove(name)
		return "", nil
	default:
		return c.keepBothConflict(ctx, name, item)
	}
}

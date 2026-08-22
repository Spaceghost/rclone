package vfscache

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
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

func (c *Cache) conflictItem(name string) (*Item, error) {
	name = clean(name)
	c.mu.Lock()
	item := c.item[name]
	c.mu.Unlock()
	if item == nil {
		return nil, fmt.Errorf("writeback conflict %q not found", name)
	}

	item.mu.Lock()
	defer item.mu.Unlock()
	if !item.info.Conflict {
		return nil, fmt.Errorf("writeback conflict %q not found", name)
	}
	if item.opens != 0 {
		return nil, fmt.Errorf("writeback conflict %q is in use", name)
	}
	return item, nil
}

func (c *Cache) hasConflictItem(name string, item *Item) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.item[name] == item
}

func conflictName(name string, n int) string {
	dir, leaf := path.Split(name)
	ext := path.Ext(leaf)
	if ext == leaf {
		ext = ""
	}
	base := strings.TrimSuffix(leaf, ext)
	return path.Join(dir, fmt.Sprintf("%s.conflict%d%s", base, n, ext))
}

func (c *Cache) nextConflictName(ctx context.Context, name string) (string, error) {
	for n := 1; ; n++ {
		candidate := conflictName(name, n)
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

func (c *Cache) stopConflictWriteback(item *Item) {
	item.mu.Lock()
	id := item.writeBackID
	item.writeBackID = 0
	item.mu.Unlock()
	c.writeback.Remove(id)
}

func (c *Cache) queueConflictWriteback(item *Item) {
	item.mu.Lock()
	name := item.name
	size := item.info.Size
	item.mu.Unlock()

	id := c.writeback.Add(0, name, size, true, func(ctx context.Context) error {
		return item.store(ctx, nil)
	})
	item.mu.Lock()
	item.writeBackID = id
	item.mu.Unlock()
}

func (c *Cache) keepLocalConflict(ctx context.Context, name string, item *Item) error {
	remote, err := c.fremote.NewObject(ctx, name)
	if errors.Is(err, fs.ErrorObjectNotFound) {
		remote = nil
	} else if err != nil {
		return fmt.Errorf("failed to read current remote file %q: %w", name, err)
	}

	c.stopConflictWriteback(item)
	if !c.hasConflictItem(name, item) {
		return fmt.Errorf("writeback conflict %q not found", name)
	}

	item.mu.Lock()
	defer item.mu.Unlock()
	if !item.info.Conflict || item.opens != 0 {
		return fmt.Errorf("writeback conflict %q changed while resolving", name)
	}

	oldObject := item.o
	oldFingerprint := item.info.Fingerprint
	item.o = remote
	item.info.Fingerprint = ""
	item._updateFingerprint()
	item.info.Conflict = false // prevent a second resolution while _store unlocks item.mu
	item.modified = true

	err = item._store(ctx, nil)
	if err == nil {
		return nil
	}

	// The false conflict state above was deliberately not saved, so a
	// process exit during the upload still reloads this as unresolved.
	item.o = oldObject
	item.info.Fingerprint = oldFingerprint
	item.info.Conflict = true
	if saveErr := item._save(); saveErr != nil {
		return fmt.Errorf("%w (failed to restore conflict state: %v)", err, saveErr)
	}
	return err
}

func (c *Cache) keepBothConflict(ctx context.Context, name string, item *Item) (string, error) {
	newName, err := c.nextConflictName(ctx, name)
	if err != nil {
		return "", err
	}

	c.stopConflictWriteback(item)
	if !c.hasConflictItem(name, item) {
		return "", fmt.Errorf("writeback conflict %q not found", name)
	}
	if err := c.Rename(name, newName, nil); err != nil {
		return "", err
	}

	item.mu.Lock()
	item.info.Fingerprint = ""
	item.info.Conflict = false
	item.modified = true
	size := item.info.Size
	err = item._save()
	item.mu.Unlock()
	if err != nil {
		item.mu.Lock()
		item.info.Conflict = true
		_ = item._save()
		item.mu.Unlock()
		return "", fmt.Errorf("failed to save conflict copy %q: %w", newName, err)
	}

	if c.avFn != nil {
		if err := c.avFn(newName, size, false); err != nil {
			item.mu.Lock()
			item.info.Conflict = true
			_ = item._save()
			item.mu.Unlock()
			return "", fmt.Errorf("failed to add conflict copy %q to VFS: %w", newName, err)
		}
	}

	c.queueConflictWriteback(item)
	return newName, nil
}

// ResolveConflict resolves a cached writeback conflict.
func (c *Cache) ResolveConflict(ctx context.Context, name, action string) (conflictName string, err error) {
	name = clean(name)
	if name == "" {
		return "", errors.New("conflict file name is empty")
	}
	item, err := c.conflictItem(name)
	if err != nil {
		return "", err
	}

	switch action {
	case conflictKeepLocal:
		return "", c.keepLocalConflict(ctx, name, item)
	case conflictKeepRemote:
		c.stopConflictWriteback(item)
		if !c.hasConflictItem(name, item) {
			return "", fmt.Errorf("writeback conflict %q not found", name)
		}
		c.Remove(name)
		return "", nil
	case conflictKeepBoth:
		return c.keepBothConflict(ctx, name, item)
	default:
		return "", fmt.Errorf("unknown conflict action %q", action)
	}
}

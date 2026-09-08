// Package kopiarepo owns in-process Kopia connections and disposable local state.
package kopiarepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/atexit"
	"github.com/rclone/rclone/lib/backup/kopiablob"
)

const (
	defaultContentCacheBytes  = 256 << 20
	defaultMetadataCacheBytes = 64 << 20
)

type failure struct{ err error }

// Repository owns a Kopia connection, not the underlying rclone filesystem.
// Temporary connection metadata contains no repository password. A caller may
// provide a persistent cache directory; that cache is not needed for recovery.
type Repository struct {
	repo.Repository
	dir       string
	cancel    context.CancelCauseFunc
	fatal     atomic.Pointer[failure]
	atExit    atexit.FnHandle
	closeOnce sync.Once
	closeErr  error
}

// Open connects to a standard sharded Kopia repository. It never initializes one.
// cacheDirectory may be empty to keep all local state disposable.
func Open(ctx context.Context, storage kopiablob.Options, password, cacheDirectory string) (*Repository, error) {
	if password == "" {
		return nil, errors.New("backup repository password is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "rclone-kopia-")
	if err != nil {
		return nil, fmt.Errorf("create private Kopia connection directory: %w", err)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	r := &Repository{dir: dir, cancel: cancel}
	fail := func(err error) (*Repository, error) {
		cancel(err)
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if cacheDirectory == "" {
		cacheDirectory = filepath.Join(dir, "cache")
	}
	cacheDirectory, err = filepath.Abs(cacheDirectory)
	if err != nil {
		return fail(err)
	}
	ci := blob.ConnectionInfo{Type: kopiablob.Type, Config: &storage}
	cfg := repo.LocalConfig{
		Storage: &ci,
		// A cache directory with zero capacity is not equivalent to a disabled
		// cache in Kopia. Configure both caches together so metadata reads have
		// a backing cache; the engine's sweep policy enforces these targets.
		Caching: &content.CachingOptions{
			CacheDirectory:              cacheDirectory,
			ContentCacheSizeBytes:       defaultContentCacheBytes,
			MetadataCacheSizeBytes:      defaultMetadataCacheBytes,
			ContentCacheSizeLimitBytes:  2 * defaultContentCacheBytes,
			MetadataCacheSizeLimitBytes: 2 * defaultMetadataCacheBytes,
		},
		ClientOptions: repo.ClientOptions{ReadOnly: storage.ReadOnly, EnableActions: false},
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return fail(err)
	}
	configPath := filepath.Join(dir, "connection.json")
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		return fail(err)
	}
	r.Repository, err = repo.Open(ctx, configPath, password, &repo.Options{
		DisableRepositoryLog: true,
		DoNotWaitForUpgrade:  true,
		// Kopia's CLI default calls os.Exit. An embedded engine must not kill
		// an rcd process and unrelated jobs when one repository fails.
		OnFatalError: func(err error) {
			r.fatal.Store(&failure{err: err})
			cancel(err)
		},
	})
	if err != nil {
		return fail(fmt.Errorf("open Kopia repository: %w", err))
	}
	// Some CLI commands construct filesystems without the fs cache. Register
	// with the existing rclone exit machinery as well as supporting Close.
	// The callback never accesses atExit, which is assigned after registration.
	r.atExit = atexit.Register(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.close(cleanup); err != nil {
			fs.Errorf(nil, "Close Kopia repository: %v", err)
		}
	})
	return r, nil
}

// Err reports a fatal engine error without terminating the hosting process.
func (r *Repository) Err() error {
	if f := r.fatal.Load(); f != nil {
		return fmt.Errorf("kopia repository failed: %w", f.err)
	}
	return nil
}

// Close releases engine resources and removes only connection-owned temporary
// files. A caller-supplied persistent cache is never removed.
func (r *Repository) Close(ctx context.Context) error {
	atexit.Unregister(r.atExit)
	return r.close(ctx)
}

func (r *Repository) close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.closeErr = r.Repository.Close(ctx)
		r.cancel(r.closeErr)
		r.closeErr = errors.Join(r.closeErr, os.RemoveAll(r.dir))
	})
	return r.closeErr
}

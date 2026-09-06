// Package kopiablob adapts rclone filesystems to Kopia's sharded blob storage.
// The repository layout is shared with Kopia's filesystem and rclone providers.
package kopiablob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/sharded"
	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

// Type is the process-local Kopia provider registration name, not a new format.
const Type = "rclone-native"

// Options identifies storage without copying rclone credentials into Kopia config.
// Writes are initially supported only for a direct local backend. Other backends
// require independent atomicity and consistency qualification before enabling them.
type Options struct {
	Remote   string `json:"remote"`
	ReadOnly bool   `json:"readOnly"`
}

// Storage implements Kopia's blob contract over an rclone filesystem.
// It does not own the underlying Fs and never shuts down shared rclone connections.
type Storage struct {
	sharded.Storage
	blob.DefaultProviderImplementation
	f   fs.Fs
	opt Options
}

func init() {
	blob.AddSupportedStorage(Type, Options{ReadOnly: true}, New)
}

// New resolves an rclone remote and constructs a storage connection.
func New(ctx context.Context, opt *Options, isCreate bool) (blob.Storage, error) {
	if opt == nil || opt.Remote == "" {
		return nil, errors.New("backup storage remote is required")
	}
	f, err := fs.NewFs(ctx, opt.Remote)
	if err != nil {
		return nil, fmt.Errorf("open backup storage: %w", err)
	}
	return FromFs(f, *opt, isCreate)
}

// FromFs adapts an existing filesystem. isCreate chooses Kopia's default sharding
// for new repositories; existing .shards parameters always take precedence.
func FromFs(f fs.Fs, opt Options, isCreate bool) (*Storage, error) {
	if f == nil {
		return nil, errors.New("nil backup storage filesystem")
	}
	if isCreate && opt.ReadOnly {
		return nil, os.ErrPermission
	}
	if !opt.ReadOnly {
		// A server-side Move capability alone does not imply atomic replacement.
		// Qualify the concrete local implementation rather than assuming that
		// every remote with Move or !PartialUploads is safe repository storage.
		if _, ok := f.(*local.Fs); !ok || f.Features().Move == nil {
			return nil, errors.New("backup writes currently require a direct local backend; remote storage is read-only until qualified")
		}
	}
	s := &Storage{f: f, opt: opt}
	s.Storage = sharded.New(s, ".", sharded.Options{}, isCreate)
	return s, nil
}

// ConnectionInfo returns a credential-free reference to rclone configuration.
func (s *Storage) ConnectionInfo() blob.ConnectionInfo {
	o := s.opt
	return blob.ConnectionInfo{Type: Type, Config: &o}
}

// DisplayName deliberately omits the remote's potentially sensitive root/config.
func (s *Storage) DisplayName() string { return "rclone backup storage" }

// IsReadOnly reports whether storage mutations are prohibited.
func (s *Storage) IsReadOnly() bool { return s.opt.ReadOnly }

// FlushCaches invalidates backend directory caches without introducing a VFS.
func (s *Storage) FlushCaches(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if flush := s.f.Features().DirCacheFlush; flush != nil {
		flush()
	}
	return nil
}

func checkID(id blob.ID) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(string(id), "/\\\x00") {
		return fmt.Errorf("invalid backup blob ID %q", id)
	}
	return nil
}

// GetBlob returns exactly the requested bytes, or an error with an empty output.
func (s *Storage) GetBlob(ctx context.Context, id blob.ID, offset, length int64, output blob.OutputBuffer) error {
	output.Reset()
	if err := checkID(id); err != nil {
		return err
	}
	return s.Storage.GetBlob(ctx, id, offset, length, output)
}

// GetMetadata retrieves a blob's size and timestamp.
func (s *Storage) GetMetadata(ctx context.Context, id blob.ID) (blob.Metadata, error) {
	if err := checkID(id); err != nil {
		return blob.Metadata{}, err
	}
	return s.Storage.GetMetadata(ctx, id)
}

// PutBlob publishes a complete blob, never a partially uploaded final object.
func (s *Storage) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, opt blob.PutOptions) error {
	if s.opt.ReadOnly {
		return os.ErrPermission
	}
	if err := checkID(id); err != nil {
		return err
	}
	return s.Storage.PutBlob(ctx, id, data, opt)
}

// DeleteBlob removes a blob idempotently. Retention remains an engine operation.
func (s *Storage) DeleteBlob(ctx context.Context, id blob.ID) error {
	if s.opt.ReadOnly {
		return os.ErrPermission
	}
	if err := checkID(id); err != nil {
		return err
	}
	return s.Storage.DeleteBlob(ctx, id)
}

func remotePath(p string) (string, error) {
	if strings.ContainsAny(p, "\\\x00") || path.IsAbs(p) {
		return "", fmt.Errorf("invalid backup storage path %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("invalid backup storage path %q", p)
		}
	}
	p = path.Clean(p)
	if p == "." {
		return "", nil
	}
	return p, nil
}

func mapError(err error) error {
	if errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound) || errors.Is(err, os.ErrNotExist) {
		return blob.ErrBlobNotFound
	}
	return err
}

// GetBlobFromPath implements the range-read side of sharded.Impl.
// Kopia defines length < 0 as the whole blob, irrespective of offset.
func (s *Storage) GetBlobFromPath(ctx context.Context, _, p string, offset, length int64, output blob.OutputBuffer) (err error) {
	output.Reset()
	defer func() {
		if err != nil {
			output.Reset()
		}
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	p, err = remotePath(p)
	if err != nil {
		return err
	}
	if length < 0 {
		offset = 0
	}
	if offset < 0 {
		return blob.ErrInvalidRange
	}
	o, err := s.f.NewObject(ctx, p)
	if err != nil {
		return mapError(err)
	}
	size := o.Size()
	if size < 0 {
		return errors.New("backup storage returned unknown blob size")
	}
	if length < 0 {
		length = size
	}
	// Subtraction avoids overflow for adversarial offset/length values.
	if offset > size || length > size-offset {
		return blob.ErrInvalidRange
	}
	if length == 0 {
		return nil
	}
	r, err := o.Open(ctx, &fs.RangeOption{Start: offset, End: offset + length - 1})
	if err != nil {
		return mapError(err)
	}
	_, copyErr := io.CopyN(output, r, length)
	closeErr := r.Close()
	if copyErr != nil {
		if errors.Is(copyErr, io.EOF) || errors.Is(copyErr, io.ErrUnexpectedEOF) {
			return blob.ErrInvalidRange
		}
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return blob.EnsureLengthExactly(output.Length(), length)
}

// GetMetadataFromPath implements sharded.Impl.
func (s *Storage) GetMetadataFromPath(ctx context.Context, _, p string) (blob.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return blob.Metadata{}, err
	}
	p, err := remotePath(p)
	if err != nil {
		return blob.Metadata{}, err
	}
	o, err := s.f.NewObject(ctx, p)
	if err != nil {
		return blob.Metadata{}, mapError(err)
	}
	return blob.Metadata{Length: o.Size(), Timestamp: o.ModTime(ctx)}, nil
}

// PutBlobInPath stages local writes under a name ignored by Kopia's sharded
// listing, then atomically renames them. Unsupported guarantees fail before IO.
func (s *Storage) PutBlobInPath(ctx context.Context, _, p string, data blob.Bytes, opt blob.PutOptions) error {
	if s.opt.ReadOnly {
		return os.ErrPermission
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if opt.DoNotRecreate || opt.HasRetentionOptions() {
		return blob.ErrUnsupportedPutBlobOption
	}
	p, err := remotePath(p)
	if err != nil {
		return err
	}
	if p == "" {
		return errors.New("cannot write backup storage root")
	}
	tmp := p + "." + uuid.NewString() + ".tmp"
	modTime := opt.SetModTime
	if modTime.IsZero() {
		modTime = time.Now()
	}
	r := data.Reader()
	o, putErr := s.f.Put(ctx, r, object.NewStaticObjectInfo(tmp, modTime, int64(data.Length()), true, nil, s.f))
	closeErr := r.Close()
	// Always attempt cleanup with a bounded, uncancelled context. Do not touch
	// the final path on failure: it may contain the previous committed blob.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if staged, e := s.f.NewObject(cleanup, tmp); e == nil {
			if e = staged.Remove(cleanup); e != nil {
				fs.Debugf(s.f, "backup staging cleanup failed: %v", e)
			}
		}
	}()
	if putErr != nil {
		return putErr
	}
	if closeErr != nil {
		return closeErr
	}
	if o == nil || o.Size() != int64(data.Length()) {
		return errors.New("backup staging upload has unexpected size")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	published, err := s.f.Features().Move(ctx, o, p)
	if err != nil {
		return err
	}
	if opt.GetModTime != nil {
		*opt.GetModTime = published.ModTime(ctx)
	}
	return nil
}

// DeleteBlobInPath implements sharded.Impl without removing directories.
func (s *Storage) DeleteBlobInPath(ctx context.Context, _, p string) error {
	if s.opt.ReadOnly {
		return os.ErrPermission
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := remotePath(p)
	if err != nil {
		return err
	}
	o, err := s.f.NewObject(ctx, p)
	if errors.Is(mapError(err), blob.ErrBlobNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	err = o.Remove(ctx)
	if errors.Is(mapError(err), blob.ErrBlobNotFound) {
		return nil
	}
	return err
}

type fileInfo struct {
	name string
	size int64
	mod  time.Time
	dir  bool
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) ModTime() time.Time { return i.mod }
func (i fileInfo) IsDir() bool        { return i.dir }
func (i fileInfo) Sys() any           { return nil }
func (i fileInfo) Mode() os.FileMode {
	if i.dir {
		return os.ModeDir | 0o700
	}
	return 0o600
}

// ReadDir supplies immediate children to Kopia's existing sharded walker.
// Missing directories are empty; other listing errors must never hide blobs.
func (s *Storage) ReadDir(ctx context.Context, p string) ([]os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := remotePath(p)
	if err != nil {
		return nil, err
	}
	entries, err := s.f.List(ctx, p)
	if errors.Is(err, fs.ErrorDirNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]os.FileInfo, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := path.Base(e.Remote())
		if name == "." || name == ".." || seen[name] {
			return nil, fmt.Errorf("ambiguous backup storage entry %q", name)
		}
		seen[name] = true
		_, isDir := e.(fs.Directory)
		result = append(result, fileInfo{name: name, size: e.Size(), mod: e.ModTime(ctx), dir: isDir})
	}
	return result, nil
}

var _ blob.Storage = (*Storage)(nil)
var _ sharded.Impl = (*Storage)(nil)

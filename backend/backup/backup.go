// Package backup exposes Kopia snapshots as a read-only rclone filesystem.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	kfs "github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/backup/kopiablob"
	"github.com/rclone/rclone/lib/backup/kopiarepo"
	"golang.org/x/sync/errgroup"
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "backup",
		Description: "Kopia snapshot reader (experimental)",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:     "remote",
			Help:     "Remote or local path containing an existing Kopia filesystem/rclone repository.\n\nReads use the native in-process adapter. Flat native-cloud Kopia repositories are not supported yet.",
			Required: true,
		}, {
			Name: "password", Help: "Kopia repository password.",
			IsPassword: true, Required: true,
		}, {
			Name:     "source",
			Help:     "Optional exact source identifier (user@host:path).\n\nUse 'rclone backend sources remote:' to list identifiers. Without a selection, latest is available only when a single source exists.",
			Advanced: true,
		}, {
			Name:     "cache_dir",
			Help:     "Optional local Kopia cache directory.\n\nBy default the cache is temporary and deleted on shutdown. A persistent cache improves subsequent opens but is never required for recovery. Do not place it inside the repository.",
			Advanced: true,
		}},
	})
}

// Options defines the backup remote configuration.
type Options struct {
	Remote   string `config:"remote"`
	Password string `config:"password"`
	Source   string `config:"source"`
	CacheDir string `config:"cache_dir"`
}

type openChainKey struct{}

// Fs is an immutable catalogue of snapshots for one connection. A new connection
// is required to see newly published snapshots; latest never changes mid-restore.
type Fs struct {
	name       string
	root       string
	opt        Options
	repository *kopiarepo.Repository
	features   *fs.Features
	complete   map[string]*snapshot.Manifest
	incomplete map[string]*snapshot.Manifest
	latest     *snapshot.Manifest
	sources    []string
	mu         sync.RWMutex
	closed     bool
	closeErr   error
}

// NewFs opens an existing repository without making any storage mutations.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	var opt Options
	if err := configstruct.Set(m, &opt); err != nil {
		return nil, err
	}
	if opt.Remote == "" || opt.Password == "" {
		return nil, errors.New("backup storage remote and repository password are required")
	}
	chain, _ := ctx.Value(openChainKey{}).([]string)
	for _, previous := range chain {
		if previous == name {
			return nil, errors.New("recursive backup remote configuration")
		}
	}
	if len(chain) >= 16 || strings.HasPrefix(opt.Remote, name+":") {
		return nil, errors.New("recursive backup remote configuration")
	}
	chain = append(append([]string(nil), chain...), name)
	ctx = context.WithValue(ctx, openChainKey{}, chain)
	root, err := cleanPath(root)
	if err != nil {
		return nil, err
	}
	password, err := obscure.Reveal(opt.Password)
	if err != nil {
		return nil, fmt.Errorf("decode backup password (use rclone config): %w", err)
	}
	r, err := kopiarepo.Open(ctx, kopiablob.Options{Remote: opt.Remote, ReadOnly: true}, password, opt.CacheDir)
	if err != nil {
		return nil, err
	}
	// Retain neither the revealed password nor its obscured configuration value.
	opt.Password = ""
	f := &Fs{name: name, root: root, opt: opt, repository: r}
	f.features = (&fs.Features{CanHaveEmptyDirectories: true}).Fill(ctx, f)
	fail := func(err error) (fs.Fs, error) {
		_ = f.Shutdown(context.WithoutCancel(ctx))
		return nil, err
	}
	if err := f.loadCatalogue(ctx); err != nil {
		return fail(err)
	}
	if !isContainer(root) {
		entry, err := f.resolve(ctx, root)
		if err != nil {
			return fail(err)
		}
		defer entry.Close()
		if !entry.IsDir() {
			if _, ok := entry.(kfs.File); !ok {
				return fail(fs.ErrorNotAFile)
			}
			f.root = path.Dir(root)
			if f.root == "." {
				f.root = ""
			}
			return f, fs.ErrorIsFile
		}
	}
	return f, nil
}

func sourceName(m *snapshot.Manifest) string {
	return fmt.Sprintf("%s@%s:%s", m.Source.UserName, m.Source.Host, m.Source.Path)
}

func isIncomplete(m *snapshot.Manifest) bool {
	if m.IncompleteReason != "" {
		return true
	}
	if m.RootEntry != nil && m.RootEntry.DirSummary != nil {
		s := m.RootEntry.DirSummary
		return s.IncompleteReason != "" || s.FatalErrorCount != 0 || s.IgnoredErrorCount != 0
	}
	return false
}

func (f *Fs) loadCatalogue(ctx context.Context) error {
	ids, err := snapshot.ListSnapshotManifests(ctx, f.repository, nil, nil)
	if err != nil {
		return err
	}
	manifests := make([]*snapshot.Manifest, len(ids))
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for i, id := range ids {
		group.Go(func() error {
			var err error
			manifests[i], err = snapshot.LoadSnapshot(ctx, f.repository, id)
			return err
		})
	}
	// Do not use LoadSnapshots: that helper logs and drops unreadable manifests.
	// A corrupt catalogue must not silently turn into a successful older backup.
	if err := group.Wait(); err != nil {
		return fmt.Errorf("read snapshot catalogue: %w", err)
	}
	f.complete = make(map[string]*snapshot.Manifest)
	f.incomplete = make(map[string]*snapshot.Manifest)
	sources := map[string]bool{}
	for _, m := range manifests {
		if m.RootEntry == nil {
			return fmt.Errorf("snapshot %s has no root entry", m.ID)
		}
		source := sourceName(m)
		if f.opt.Source != "" && source != f.opt.Source {
			continue
		}
		sources[source] = true
		if isIncomplete(m) {
			f.incomplete[string(m.ID)] = m
			continue
		}
		f.complete[string(m.ID)] = m
		if f.latest == nil || m.EndTime.ToTime().After(f.latest.EndTime.ToTime()) ||
			(m.EndTime == f.latest.EndTime && m.ID > f.latest.ID) {
			f.latest = m
		}
	}
	for source := range sources {
		f.sources = append(f.sources, source)
	}
	sort.Strings(f.sources)
	if len(f.sources) > 1 {
		f.latest = nil
	}
	return nil
}

func cleanPath(p string) (string, error) {
	if strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return "", errors.New("invalid snapshot path")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", errors.New("snapshot path cannot contain '..'")
		}
	}
	p = path.Clean(p)
	if p == "." {
		return "", nil
	}
	return p, nil
}

func isContainer(p string) bool { return p == "" || p == "snapshots" || p == "incomplete" }

func (f *Fs) health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.closed {
		return os.ErrClosed
	}
	return f.repository.Err()
}

func (f *Fs) resolve(ctx context.Context, p string) (kfs.Entry, error) {
	parts := strings.Split(p, "/")
	var m *snapshot.Manifest
	var rest []string
	switch parts[0] {
	case "latest":
		if len(f.sources) > 1 {
			return nil, errors.New("latest is ambiguous: select source in config or use snapshots/<id>")
		}
		m, rest = f.latest, parts[1:]
	case "snapshots", "incomplete":
		if len(parts) < 2 {
			return nil, fs.ErrorIsDir
		}
		if parts[0] == "snapshots" {
			m = f.complete[parts[1]]
		} else {
			m = f.incomplete[parts[1]]
		}
		rest = parts[2:]
	default:
		return nil, fs.ErrorObjectNotFound
	}
	if m == nil {
		return nil, fs.ErrorObjectNotFound
	}
	entry := snapshotfs.EntryFromDirEntry(f.repository, m.RootEntry)
	for _, name := range rest {
		dir, ok := entry.(kfs.Directory)
		if !ok {
			entry.Close()
			return nil, fs.ErrorObjectNotFound
		}
		next, err := dir.Child(ctx, name)
		entry.Close()
		if errors.Is(err, kfs.ErrEntryNotFound) {
			return nil, fs.ErrorObjectNotFound
		}
		if err != nil {
			return nil, err
		}
		entry = next
	}
	if failed, ok := entry.(kfs.ErrorEntry); ok {
		entry.Close()
		return nil, failed.ErrorInfo()
	}
	return entry, nil
}

// Name returns the configured remote name.
func (f *Fs) Name() string { return f.name }

// Root returns the snapshot view root, not the repository storage root.
func (f *Fs) Root() string { return f.root }

// String describes this remote without disclosing storage credentials.
func (f *Fs) String() string { return "Kopia snapshots " + f.name + ":" + f.root }

// Precision returns the timestamp precision stored in snapshot metadata.
func (f *Fs) Precision() time.Duration { return time.Nanosecond }

// Hashes does not misrepresent Kopia chunk IDs as whole-file hashes.
func (f *Fs) Hashes() hash.Set { return hash.NewHashSet() }

// Features returns the read-only backend capabilities.
func (f *Fs) Features() *fs.Features { return f.features }

func (f *Fs) dirEntry(remote string, entry kfs.Entry) (fs.DirEntry, error) {
	if entry.IsDir() {
		return fs.NewDir(remote, entry.ModTime()), nil
	}
	if file, ok := entry.(kfs.File); ok && entry.Mode().IsRegular() {
		return &Object{f: f, remote: remote, entry: file}, nil
	}
	// Do not silently omit links or materialize them as unrelated regular files.
	return nil, fmt.Errorf("%s: unsupported snapshot entry type %s; use Kopia restore for full metadata/link fidelity", remote, entry.Mode())
}

// List lists virtual snapshot directories or the children of one snapshot path.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if err := f.health(ctx); err != nil {
		return nil, err
	}
	dir, err := cleanPath(dir)
	if err != nil {
		return nil, err
	}
	full := path.Join(f.root, dir)
	if full == "." {
		full = ""
	}
	var result fs.DirEntries
	if full == "" {
		result = append(result, fs.NewDir("snapshots", time.Time{}), fs.NewDir("incomplete", time.Time{}))
		if f.latest != nil {
			e, err := f.dirEntry("latest", snapshotfs.EntryFromDirEntry(f.repository, f.latest.RootEntry))
			if err != nil {
				return nil, err
			}
			result = append(result, e)
		}
		return result, nil
	}
	if full == "snapshots" || full == "incomplete" {
		manifests := f.complete
		if full == "incomplete" {
			manifests = f.incomplete
		}
		for id, m := range manifests {
			e, err := f.dirEntry(path.Join(dir, id), snapshotfs.EntryFromDirEntry(f.repository, m.RootEntry))
			if err != nil {
				return nil, err
			}
			result = append(result, e)
		}
		sort.Slice(result, func(i, j int) bool { return result[i].Remote() < result[j].Remote() })
		return result, nil
	}
	entry, err := f.resolve(ctx, full)
	if errors.Is(err, fs.ErrorObjectNotFound) {
		return nil, fs.ErrorDirNotFound
	}
	if err != nil {
		return nil, err
	}
	defer entry.Close()
	folder, ok := entry.(kfs.Directory)
	if !ok {
		return nil, fs.ErrorIsFile
	}
	children, err := kfs.GetAllEntries(ctx, folder)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(children))
	for _, child := range children {
		name := child.Name()
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || seen[name] {
			return nil, errors.New("snapshot directory has invalid or duplicate names")
		}
		seen[name] = true
		e, err := f.dirEntry(path.Join(dir, name), child)
		if err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Remote() < result[j].Remote() })
	return result, nil
}

// NewObject resolves a regular file in an immutable snapshot.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if err := f.health(ctx); err != nil {
		return nil, err
	}
	remote, err := cleanPath(remote)
	if err != nil {
		return nil, err
	}
	full := path.Join(f.root, remote)
	if isContainer(full) {
		return nil, fs.ErrorIsDir
	}
	entry, err := f.resolve(ctx, full)
	if err != nil {
		return nil, err
	}
	if entry.IsDir() {
		entry.Close()
		return nil, fs.ErrorIsDir
	}
	file, ok := entry.(kfs.File)
	if !ok {
		entry.Close()
		return nil, fs.ErrorNotAFile
	}
	return &Object{f: f, remote: remote, entry: file}, nil
}

// Put rejects ordinary writes; publishing a snapshot is a separate transaction.
func (f *Fs) Put(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) (fs.Object, error) {
	return nil, fs.ErrorPermissionDenied
}

// Mkdir rejects mutations of snapshot history.
func (f *Fs) Mkdir(context.Context, string) error { return fs.ErrorPermissionDenied }

// Rmdir rejects mutations of snapshot history.
func (f *Fs) Rmdir(context.Context, string) error { return fs.ErrorPermissionDenied }

// Command exposes source discovery without a separate Kopia executable.
func (f *Fs) Command(ctx context.Context, name string, args []string, opt map[string]string) (any, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if err := f.health(ctx); err != nil {
		return nil, err
	}
	if len(args) != 0 || len(opt) != 0 {
		return nil, errors.New("backup backend commands take no arguments or options")
	}
	if name == "sources" {
		return append([]string{}, f.sources...), nil
	}
	return nil, fs.ErrorCommandNotFound
}

// Shutdown waits for open file readers before releasing repository resources.
func (f *Fs) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		f.closeErr = f.repository.Close(ctx)
	}
	return f.closeErr
}

// Object is a regular file backed by Kopia's authenticated, seekable reader.
type Object struct {
	f      *Fs
	remote string
	entry  kfs.File
}

// Fs returns the parent filesystem.
func (o *Object) Fs() fs.Info { return o.f }

// Remote returns the path relative to the view root.
func (o *Object) Remote() string { return o.remote }

// String returns the file's relative path.
func (o *Object) String() string { return o.remote }

// ModTime returns the snapshot timestamp, not the backing pack's timestamp.
func (o *Object) ModTime(context.Context) time.Time { return o.entry.ModTime() }

// Size returns the logical file length.
func (o *Object) Size() int64 { return o.entry.Size() }

// Storable reports whether the object represents a regular file.
func (o *Object) Storable() bool { return true }

// Hash does not expose keyed content IDs as portable file hashes.
func (o *Object) Hash(context.Context, hash.Type) (string, error) { return "", hash.ErrUnsupported }

// SetModTime rejects mutations of snapshot history.
func (o *Object) SetModTime(context.Context, time.Time) error { return fs.ErrorPermissionDenied }

// Remove rejects mutations of snapshot history.
func (o *Object) Remove(context.Context) error { return fs.ErrorPermissionDenied }

// Update rejects mutations of snapshot history.
func (o *Object) Update(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) error {
	return fs.ErrorPermissionDenied
}

type snapshotReader struct {
	io.Reader
	closer io.Closer
	unlock func()
	once   sync.Once
	err    error
}

func (r *snapshotReader) Close() error {
	r.once.Do(func() {
		r.err = r.closer.Close()
		r.unlock()
	})
	return r.err
}

// Open reads only the requested logical file range; Kopia locates and verifies
// the backing chunks. The repository stays open until the returned reader closes.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	o.f.mu.RLock()
	fail := func(err error) (io.ReadCloser, error) { o.f.mu.RUnlock(); return nil, err }
	if err := o.f.health(ctx); err != nil {
		return fail(err)
	}
	offset, limit := int64(0), int64(-1)
	for _, option := range options {
		switch x := option.(type) {
		case *fs.RangeOption:
			offset, limit = x.Decode(o.Size())
		case *fs.SeekOption:
			offset = x.Offset
		default:
			if option.Mandatory() {
				return fail(fmt.Errorf("unsupported snapshot read option: %v", option))
			}
		}
	}
	if offset < 0 || offset > o.Size() || limit < -1 {
		return fail(errors.New("invalid snapshot file range"))
	}
	r, err := o.entry.Open(ctx)
	if err != nil {
		return fail(err)
	}
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		_ = r.Close()
		return fail(err)
	}
	var reader io.Reader = r
	if limit >= 0 {
		reader = io.LimitReader(r, limit)
	}
	return &snapshotReader{Reader: reader, closer: r, unlock: o.f.mu.RUnlock}, nil
}

var _ fs.Fs = (*Fs)(nil)
var _ fs.Object = (*Object)(nil)
var _ fs.Shutdowner = (*Fs)(nil)
var _ fs.Commander = (*Fs)(nil)

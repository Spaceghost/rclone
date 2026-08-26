// Package projection implements a manifest-driven overlay of rclone remotes.
package projection

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Spaceghost/rclone-projection-vfs/internal/manifest"
	"github.com/Spaceghost/rclone-projection-vfs/internal/model"
	"github.com/Spaceghost/rclone-projection-vfs/internal/resolver"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "projection",
		Description: "Manifest-driven projection of other rclone remotes",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:     "manifest",
			Help:     "Path to the standalone CUE projection manifest.",
			Required: true,
		}},
	})
}

type Options struct {
	Manifest string `config:"manifest"`
}

type Fs struct {
	name      string
	root      string
	opt       Options
	manifest  model.Manifest
	resolver  *resolver.Static
	upstreams map[string]*upstream
	features  *fs.Features
	hashes    hash.Set
	precision time.Duration
	when      time.Time
}

type Object struct {
	fs.Object
	fs     *Fs
	remote string
}

type upstream struct {
	fs fs.Fs
}

func NewFs(ctx context.Context, name, root string, mapper configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(mapper, opt); err != nil {
		return nil, err
	}
	if opt.Manifest == "" {
		return nil, fmt.Errorf("projection manifest is required")
	}
	isDir := strings.HasSuffix(root, "/")
	root = strings.Trim(root, "/")
	if root != "" && path.Clean(root) != root {
		return nil, fmt.Errorf("projection root must be clean")
	}
	m, err := manifest.LoadFile(opt.Manifest)
	if err != nil {
		return nil, fmt.Errorf("load projection manifest: %w", err)
	}
	staticResolver, err := resolver.NewStatic(m)
	if err != nil {
		return nil, err
	}
	f := &Fs{
		name:      name,
		root:      root,
		opt:       *opt,
		manifest:  m,
		resolver:  staticResolver,
		upstreams: make(map[string]*upstream, len(m.Upstreams)),
		when:      time.Now(),
	}
	first := true
	for alias, spec := range m.Upstreams {
		if strings.HasPrefix(spec.Remote, name+":") {
			return nil, fmt.Errorf("upstream %q points projection remote at itself", alias)
		}
		upstreamFs, upstreamErr := cache.Get(ctx, spec.Remote)
		if upstreamErr != nil {
			return nil, fmt.Errorf("create upstream %q from %q: %w", alias, spec.Remote, upstreamErr)
		}
		u := &upstream{fs: upstreamFs}
		cache.PinUntilFinalized(upstreamFs, u)
		f.upstreams[alias] = u
		if first {
			f.hashes = upstreamFs.Hashes()
			f.precision = upstreamFs.Precision()
			first = false
		} else {
			f.hashes = f.hashes.Overlap(upstreamFs.Hashes())
			if upstreamFs.Precision() > f.precision {
				f.precision = upstreamFs.Precision()
			}
		}
	}
	f.features = (&fs.Features{Overlay: true}).Fill(ctx, f)
	if f.root != "" && !isDir {
		_, objectErr := f.NewObject(ctx, "")
		switch {
		case objectErr == nil:
			f.root = path.Dir(f.root)
			if f.root == "." {
				f.root = ""
			}
			return f, fs.ErrorIsFile
		case errors.Is(objectErr, fs.ErrorObjectNotFound), errors.Is(objectErr, fs.ErrorIsDir):
		default:
			return nil, objectErr
		}
	}
	return f, nil
}

func (f *Fs) Name() string                        { return f.name }
func (f *Fs) Root() string                        { return f.root }
func (f *Fs) String() string                      { return fmt.Sprintf("projection root %q", f.root) }
func (f *Fs) Features() *fs.Features              { return f.features }
func (f *Fs) Precision() time.Duration            { return f.precision }
func (f *Fs) Hashes() hash.Set                    { return f.hashes }
func (f *Fs) Mkdir(context.Context, string) error { return fs.ErrorPermissionDenied }
func (f *Fs) Rmdir(context.Context, string) error { return fs.ErrorPermissionDenied }

func (f *Fs) Put(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) (fs.Object, error) {
	return nil, fs.ErrorPermissionDenied
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	resolution, err := f.resolver.Resolve(ctx, virtualPath(f.root, remote))
	if err != nil {
		return nil, fs.ErrorObjectNotFound
	}
	upstream := f.upstreams[resolution.Target.Upstream]
	object, err := upstream.fs.NewObject(ctx, resolution.Target.Path)
	if err != nil {
		return nil, err
	}
	return &Object{Object: object, fs: f, remote: remote}, nil
}

func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	absDir := join(f.root, dir)
	entries := make(map[string]fs.DirEntry)
	dirExists := absDir == ""

	if route, ok := f.prefixRoute(absDir); ok {
		dirExists = true
		if err := f.listUpstream(ctx, route, absDir, entries); err != nil && err != fs.ErrorDirNotFound {
			return nil, err
		}
	}

	for _, route := range f.manifest.Routes {
		routePath := strings.Trim(route.Match.Path, "/")
		remainder, ok := childRemainder(absDir, routePath)
		if !ok {
			continue
		}
		dirExists = true
		child, nested := splitFirst(remainder)
		visibleAbs := join(absDir, child)
		visibleRemote, ok := stripRoot(f.root, visibleAbs)
		if !ok {
			continue
		}
		if nested || route.Match.Kind == "prefix" {
			entries[child] = fs.NewDir(visibleRemote, f.when)
			continue
		}
		upstream := f.upstreams[route.Target.Upstream]
		object, err := upstream.fs.NewObject(ctx, route.Target.Path)
		if err == fs.ErrorObjectNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries[child] = &Object{Object: object, fs: f, remote: visibleRemote}
	}

	if !dirExists {
		return nil, fs.ErrorDirNotFound
	}
	result := make(fs.DirEntries, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Remote() < result[j].Remote() })
	return result, nil
}

func (f *Fs) prefixRoute(absDir string) (model.Route, bool) {
	virtualDir := "/" + absDir
	if absDir == "" {
		virtualDir = "/"
	}
	var selected model.Route
	found := false
	for _, route := range f.manifest.Routes {
		if route.Match.Kind != "prefix" {
			continue
		}
		prefixRoot := strings.TrimSuffix(route.Match.Path, "/")
		if prefixRoot == "" {
			prefixRoot = "/"
		}
		if virtualDir == prefixRoot || strings.HasPrefix(virtualDir+"/", route.Match.Path) {
			if !found || len(route.Match.Path) > len(selected.Match.Path) {
				selected, found = route, true
			}
		}
	}
	return selected, found
}

func (f *Fs) listUpstream(ctx context.Context, route model.Route, absDir string, out map[string]fs.DirEntry) error {
	routeRoot := strings.Trim(route.Match.Path, "/")
	relativeDir := strings.TrimPrefix(strings.TrimPrefix(absDir, routeRoot), "/")
	targetDir := join(route.Target.Path, relativeDir)
	upstream := f.upstreams[route.Target.Upstream]
	entries, err := upstream.fs.List(ctx, targetDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		relative, ok := trimPathPrefix(entry.Remote(), route.Target.Path)
		if !ok {
			continue
		}
		visibleAbs := join(routeRoot, relative)
		visibleRemote, ok := stripRoot(f.root, visibleAbs)
		if !ok {
			continue
		}
		key := path.Base(visibleRemote)
		if object, ok := entry.(fs.Object); ok {
			out[key] = &Object{Object: object, fs: f, remote: visibleRemote}
		} else {
			out[key] = fs.NewDir(visibleRemote, entry.ModTime(ctx))
		}
	}
	return nil
}

func (o *Object) Fs() fs.Info                                 { return o.fs }
func (o *Object) Remote() string                              { return o.remote }
func (o *Object) String() string                              { return o.remote }
func (o *Object) SetModTime(context.Context, time.Time) error { return fs.ErrorPermissionDenied }
func (o *Object) Update(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) error {
	return fs.ErrorPermissionDenied
}
func (o *Object) Remove(context.Context) error { return fs.ErrorPermissionDenied }

func virtualPath(root, remote string) string {
	joined := join(root, remote)
	if joined == "" {
		return "/"
	}
	return "/" + joined
}

func join(parts ...string) string {
	joined := path.Join(parts...)
	if joined == "." {
		return ""
	}
	return strings.TrimPrefix(joined, "/")
}

func childRemainder(parent, candidate string) (string, bool) {
	if parent == "" {
		return candidate, candidate != ""
	}
	prefix := parent + "/"
	if !strings.HasPrefix(candidate, prefix) {
		return "", false
	}
	return strings.TrimPrefix(candidate, prefix), true
}

func splitFirst(value string) (string, bool) {
	if index := strings.IndexByte(value, '/'); index >= 0 {
		return value[:index], true
	}
	return value, false
}

func stripRoot(root, value string) (string, bool) {
	if root == "" {
		return value, true
	}
	if value == root {
		return "", true
	}
	prefix := root + "/"
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	return strings.TrimPrefix(value, prefix), true
}

func trimPathPrefix(value, prefix string) (string, bool) {
	value = strings.Trim(value, "/")
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return value, true
	}
	if value == prefix {
		return "", true
	}
	if !strings.HasPrefix(value, prefix+"/") {
		return "", false
	}
	return strings.TrimPrefix(value, prefix+"/"), true
}

var (
	_ fs.Fs     = (*Fs)(nil)
	_ fs.Object = (*Object)(nil)
)

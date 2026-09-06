package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	kfs "github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/upload"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/lib/backup/kopiablob"
	"github.com/rclone/rclone/lib/backup/kopiarepo"
)

const fixturePassword = "test-only-kopia-repository-password"

type fixture struct {
	ctx     context.Context
	dir     string
	source  string
	storage blob.Storage
	r       *kopiarepo.Repository
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := kopiablob.New(ctx, &kopiablob.Options{Remote: dir}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Initialize(ctx, st, nil, fixturePassword); err != nil {
		t.Fatal(err)
	}
	r, err := kopiarepo.Open(ctx, kopiablob.Options{Remote: dir}, fixturePassword, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return &fixture{ctx: ctx, dir: dir, source: t.TempDir(), storage: st, r: r}
}

func (x *fixture) upload(t *testing.T, parents ...*snapshot.Manifest) *snapshot.Manifest {
	t.Helper()
	ctx, w, err := x.r.NewWriter(x.ctx, repo.WriteSessionOptions{Purpose: "rclone integration test"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := w.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	dir, err := localfs.Directory(x.source)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	m, err := upload.NewUploader(w).Upload(ctx, dir, policy.BuildTree(nil, policy.DefaultPolicy), snapshot.SourceInfo{
		Host: "test-host", UserName: "test-user", Path: x.source,
	}, parents...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.SaveSnapshot(ctx, w, m); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	return m
}

func (x *fixture) save(t *testing.T, m *snapshot.Manifest) *snapshot.Manifest {
	t.Helper()
	ctx, w, err := x.r.NewWriter(x.ctx, repo.WriteSessionOptions{Purpose: "snapshot catalogue test"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close(ctx) }()
	m = m.Clone()
	if _, err := snapshot.SaveSnapshot(ctx, w, m); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	return m
}

func (x *fixture) config(t *testing.T) configmap.Simple {
	t.Helper()
	password, err := obscure.Obscure(fixturePassword)
	if err != nil {
		t.Fatal(err)
	}
	return configmap.Simple{"remote": x.dir, "password": password}
}

func (x *fixture) open(t *testing.T, root string) *Fs {
	t.Helper()
	f, err := NewFs(x.ctx, "vault", root, x.config(t))
	if err != nil {
		t.Fatal(err)
	}
	result := f.(*Fs)
	t.Cleanup(func() {
		if err := result.Shutdown(x.ctx); err != nil {
			t.Error(err)
		}
	})
	return result
}

func readObject(t *testing.T, f fs.Fs, name string, options ...fs.OpenOption) []byte {
	t.Helper()
	o, err := f.NewObject(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := o.Open(context.Background(), options...)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read=%v close=%v", readErr, closeErr)
	}
	return data
}

func writeFixtureFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func packIDs(t *testing.T, st blob.Storage) []string {
	t.Helper()
	var ids []string
	if err := st.ListBlobs(context.Background(), "p", func(m blob.Metadata) error {
		ids = append(ids, string(m.BlobID))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	return ids
}

func childID(t *testing.T, r repo.Repository, m *snapshot.Manifest, name string) object.ID {
	t.Helper()
	root := snapshotfs.EntryFromDirEntry(r, m.RootEntry)
	defer root.Close()
	child, err := root.(kfs.Directory).Child(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	return child.(object.HasObjectID).ObjectID()
}

func TestSnapshotRoundTrip(t *testing.T) {
	x := newFixture(t)
	duplicate := bytes.Repeat([]byte("shared content across paths and snapshots\n"), 32768)
	writeFixtureFile(t, x.source, "a.bin", duplicate)
	writeFixtureFile(t, x.source, "b.bin", duplicate)
	writeFixtureFile(t, x.source, "sub/report.txt", []byte("version-one"))
	first := x.upload(t)
	if childID(t, x.r, first, "a.bin") != childID(t, x.r, first, "b.bin") {
		t.Fatal("identical files in the first snapshot did not share their content object")
	}
	packs := packIDs(t, x.storage)
	unchanged := x.upload(t, first)
	if unchanged.RootObjectID() != first.RootObjectID() || unchanged.Stats.NonCachedFiles != 0 {
		t.Fatalf("unchanged backup did not reuse the prior tree: %+v", unchanged.Stats)
	}
	if !reflect.DeepEqual(packs, packIDs(t, x.storage)) {
		t.Fatal("unchanged backup wrote new data packs")
	}

	frozen := x.open(t, "latest")
	if got := string(readObject(t, frozen, "sub/report.txt")); got != "version-one" {
		t.Fatalf("initial restore: %q", got)
	}
	if got := string(readObject(t, frozen, "sub/report.txt", &fs.RangeOption{Start: 2, End: 6})); got != "rsion" {
		t.Fatalf("range restore: %q", got)
	}
	if got := string(readObject(t, frozen, "sub/report.txt", &fs.SeekOption{Offset: 8})); got != "one" {
		t.Fatalf("seek restore: %q", got)
	}
	if got := string(readObject(t, frozen, "sub/report.txt", &fs.RangeOption{Start: -1, End: 3})); got != "one" {
		t.Fatalf("suffix restore: %q", got)
	}
	entries, err := frozen.List(x.ctx, "sub")
	if err != nil || len(entries) != 1 || entries[0].Remote() != "sub/report.txt" {
		t.Fatalf("nested listing: %v %v", entries, err)
	}

	// Exercise the normal rclone copy path rather than only the new Object API.
	destination, err := fs.NewFs(x.ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src, err := frozen.NewObject(x.ctx, "sub/report.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operations.Copy(x.ctx, destination, nil, "restored.txt", src); err != nil {
		t.Fatal(err)
	}
	if got := string(readObject(t, destination, "restored.txt")); got != "version-one" {
		t.Fatalf("rclone copy restored %q", got)
	}

	writeFixtureFile(t, x.source, "sub/report.txt", []byte("version-two-expanded"))
	second := x.upload(t, unchanged)
	if childID(t, x.r, second, "a.bin") != childID(t, x.r, first, "a.bin") {
		t.Fatal("unchanged file did not reuse content across snapshots")
	}
	if got := string(readObject(t, frozen, "sub/report.txt")); got != "version-one" {
		t.Fatalf("latest changed during an existing connection: %q", got)
	}
	fresh := x.open(t, "latest")
	if got := string(readObject(t, fresh, "sub/report.txt")); got != "version-two-expanded" {
		t.Fatalf("new connection did not observe new snapshot: %q", got)
	}
	old := x.open(t, "snapshots/"+string(first.ID))
	if got := string(readObject(t, old, "sub/report.txt")); got != "version-one" {
		t.Fatalf("historical snapshot changed: %q", got)
	}

	// Open the same encrypted repository using only stock Kopia filesystem and
	// repository code, with a fresh config/cache and without the rclone adapter.
	stock, err := filesystem.New(x.ctx, &filesystem.Options{Path: x.dir}, false)
	if err != nil {
		t.Fatal(err)
	}
	ci := stock.ConnectionInfo()
	config, err := json.Marshal(repo.LocalConfig{Storage: &ci, ClientOptions: repo.ClientOptions{ReadOnly: true}})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "stock-kopia.json")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := repo.Open(x.ctx, configPath, fixturePassword, &repo.Options{
		DisableRepositoryLog: true, OnFatalError: func(err error) { t.Error(err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recovered.Close(x.ctx) }()
	loaded, err := snapshot.LoadSnapshot(x.ctx, recovered, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := recovered.OpenObject(x.ctx, childID(t, recovered, loaded, "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(data, duplicate) {
		t.Fatalf("independent stock Kopia recovery failed: %v", err)
	}
}

func TestCatalogueAndReadOnly(t *testing.T) {
	x := newFixture(t)
	writeFixtureFile(t, x.source, "file.txt", []byte("complete file"))
	m := x.upload(t)
	partial := m.Clone()
	partial.IncompleteReason = "test interruption"
	partial = x.save(t, partial)
	f := x.open(t, "")
	if f.latest == nil || f.latest.ID != m.ID || len(f.incomplete) != 1 {
		t.Fatal("incomplete snapshot selected as latest")
	}
	if got := string(readObject(t, f, "incomplete/"+string(partial.ID)+"/file.txt")); got != "complete file" {
		t.Fatalf("selective incomplete restore: %q", got)
	}
	if _, err := f.NewObject(x.ctx, "snapshots/"+string(partial.ID)+"/file.txt"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("incomplete snapshot exposed as complete: %v", err)
	}
	if _, err := f.Put(x.ctx, strings.NewReader("bad"), nil); !errors.Is(err, fs.ErrorPermissionDenied) {
		t.Fatalf("Put: %v", err)
	}
	if err := f.Mkdir(x.ctx, "bad"); !errors.Is(err, fs.ErrorPermissionDenied) {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := f.Rmdir(x.ctx, "snapshots"); !errors.Is(err, fs.ErrorPermissionDenied) {
		t.Fatalf("Rmdir: %v", err)
	}
	o, err := f.NewObject(x.ctx, "latest/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Remove(x.ctx); !errors.Is(err, fs.ErrorPermissionDenied) {
		t.Fatalf("Remove: %v", err)
	}
	if err := o.Update(x.ctx, strings.NewReader("bad"), nil); !errors.Is(err, fs.ErrorPermissionDenied) {
		t.Fatalf("Update: %v", err)
	}
	fileRoot, err := NewFs(x.ctx, "vault", "latest/file.txt", x.config(t))
	if !errors.Is(err, fs.ErrorIsFile) || fileRoot.Root() != "latest" {
		t.Fatalf("file-root convention: %v %v", fileRoot, err)
	}
	_ = fileRoot.(*Fs).Shutdown(x.ctx)
	cancelled, cancel := context.WithCancel(x.ctx)
	cancel()
	if _, err := f.List(cancelled, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled listing: %v", err)
	}
	if err := f.Shutdown(x.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Open(x.ctx); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read after shutdown: %v", err)
	}

	other := m.Clone()
	other.Source.Host = "another-host"
	x.save(t, other)
	ambiguous := x.open(t, "")
	if ambiguous.latest != nil || len(ambiguous.sources) != 2 {
		t.Fatal("latest silently selected among unrelated sources")
	}
	if _, err := ambiguous.NewObject(x.ctx, "latest/file.txt"); err == nil {
		t.Fatal("ambiguous latest did not fail")
	}
	cfg := x.config(t)
	cfg["source"] = sourceName(m)
	selected, err := NewFs(x.ctx, "vault", "latest", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(readObject(t, selected, "file.txt")); got != "complete file" {
		t.Fatalf("source selection: %q", got)
	}
	_ = selected.(*Fs).Shutdown(x.ctx)
	cfg["password"], _ = obscure.Obscure("wrong-password")
	if _, err := NewFs(x.ctx, "vault", "", cfg); err == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestPathValidationAndRecursion(t *testing.T) {
	for _, path := range []string{"../outside", "x/../../outside", "/absolute", "a\\b", "a\x00b"} {
		if _, err := cleanPath(path); err == nil {
			t.Errorf("accepted invalid snapshot path %q", path)
		}
	}
	if _, err := NewFs(context.Background(), "vault", "", configmap.Simple{"remote": "vault:", "password": "unused"}); err == nil {
		t.Fatal("self-referencing repository accepted")
	}
}

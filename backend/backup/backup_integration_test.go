package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rfs "github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fs/sync"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/fstest/runs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func repositoryFiles(t *testing.T, root string) map[string][sha256.Size]byte {
	t.Helper()
	files := make(map[string][sha256.Size]byte)
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			t.Fatalf("unexpected non-regular fixture file %q", name)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = sha256.Sum256(data)
		return nil
	})
	require.NoError(t, err)
	return files
}

func TestIntegrationProfile(t *testing.T) {
	config, err := runs.NewConfig("testdata/integration.yaml")
	require.NoError(t, err)
	config.FilterBackendsByBackends([]string{"backup"})
	jobs := config.MakeRuns()
	require.Len(t, jobs, 1)
	job := jobs[0]
	assert.Equal(t, "backend/backup", job.Path)
	assert.Empty(t, job.Remote, "the suite provisions its own repository")
	assert.Empty(t, job.Ignore, "no integration failures may be ignored")
	assert.Empty(t, job.Env)
	assert.True(t, job.NoBinary)
	assert.True(t, job.NoRetries)
}

// TestIntegration exercises the read-only backend through rclone's registered
// filesystem and directory-copy paths, using only disposable local storage.
func TestIntegration(t *testing.T) {
	require.Empty(t, *fstest.RemoteName, "use the self-contained testdata/integration.yaml profile, not an existing remote")
	t.Setenv("RCLONE_CONFIG", filepath.Join(t.TempDir(), "rclone.conf"))
	fstest.Initialise()
	ctx := context.Background()
	x := newFixture(t)
	modTime := time.Unix(978307200, 0).UTC()
	files := map[string][]byte{
		"root.txt":        []byte("root file\n"),
		"nested/世界.txt":   []byte("Unicode filename and contents: 世界\n"),
		"nested/zero.bin": {},
		"nested/data.bin": bytes.Repeat([]byte("0123456789abcdef"), 65536),
	}
	for name, data := range files {
		writeFixtureFile(t, x.source, name, data)
		require.NoError(t, os.Chtimes(filepath.Join(x.source, name), modTime, modTime))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(x.source, "nested", "empty"), 0o700))
	complete := x.upload(t)
	partial := complete.Clone()
	partial.IncompleteReason = "integration fixture interruption"
	partial = x.save(t, partial)

	t.Setenv("RCLONE_CONFIG_BACKUPINTEGRATION_TYPE", "backup")
	for name, value := range x.config(t) {
		t.Setenv("RCLONE_CONFIG_BACKUPINTEGRATION_"+strings.ToUpper(name), value)
	}
	t.Setenv("RCLONE_CONFIG_BACKUPINTEGRATION_SOURCE", "")
	t.Setenv("RCLONE_CONFIG_BACKUPINTEGRATION_CACHE_DIR", "")
	before := repositoryFiles(t, x.dir)
	require.NotEmpty(t, before)
	// Registered before readers so their shutdown is included in this check.
	t.Cleanup(func() {
		assert.Equal(t, before, repositoryFiles(t, x.dir), "restore or rejected mutation changed repository files")
	})
	open := func(t *testing.T, root string) rfs.Fs {
		t.Helper()
		f, err := rfs.NewFs(ctx, "BackupIntegration:"+root)
		require.NoError(t, err)
		require.NotNil(t, f.Features().Shutdown)
		t.Cleanup(func() { assert.NoError(t, f.Features().Shutdown(ctx)) })
		return f
	}

	for _, test := range []struct {
		name, root, prefix string
	}{
		{"Latest", "latest", ""},
		{"Snapshot", "snapshots/" + string(complete.ID), ""},
		{"Incomplete", "incomplete/" + string(partial.ID), ""},
		{"Subdirectory", "latest/nested", "nested/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := open(t, test.root)
			expected := make(map[string][sha256.Size]byte)
			for name, data := range files {
				if !strings.HasPrefix(name, test.prefix) {
					continue
				}
				name = strings.TrimPrefix(name, test.prefix)
				expected[name] = sha256.Sum256(data)
				o, err := source.NewObject(ctx, name)
				require.NoError(t, err)
				item := fstest.NewItem(name, string(data), modTime)
				item.Check(t, o, source.Precision())
			}
			destination := t.TempDir()
			target, err := rfs.NewFs(ctx, destination)
			require.NoError(t, err)
			require.NoError(t, sync.CopyDir(ctx, target, source, true))
			assert.Equal(t, expected, repositoryFiles(t, destination))
			empty := strings.TrimPrefix("nested/empty", test.prefix)
			info, err := os.Stat(filepath.Join(destination, empty))
			require.NoError(t, err)
			assert.True(t, info.IsDir(), "empty directory was not restored")

			name := strings.TrimPrefix("nested/data.bin", test.prefix)
			data := files["nested/data.bin"]
			for _, read := range []struct {
				name   string
				option rfs.OpenOption
				want   []byte
			}{
				{"Range", &rfs.RangeOption{Start: 5, End: 20}, data[5:21]},
				{"Suffix", &rfs.RangeOption{Start: -1, End: 9}, data[len(data)-9:]},
				{"PastEnd", &rfs.RangeOption{Start: int64(len(data) - 3), End: int64(len(data) + 20)}, data[len(data)-3:]},
				{"SeekEOF", &rfs.SeekOption{Offset: int64(len(data))}, []byte{}},
			} {
				t.Run(read.name, func(t *testing.T) {
					assert.Equal(t, read.want, readObject(t, source, name, read.option))
				})
			}
		})
	}

	t.Run("RejectMutations", func(t *testing.T) {
		f := open(t, "latest")
		o, err := f.NewObject(ctx, "root.txt")
		require.NoError(t, err)
		info := object.NewStaticObjectInfo("root.txt", modTime, 3, true, nil, f)
		for _, test := range []struct {
			name   string
			mutate func() error
		}{
			{"Put", func() error { _, err := f.Put(ctx, strings.NewReader("bad"), info); return err }},
			{"Mkdir", func() error { return f.Mkdir(ctx, "new-directory") }},
			{"Rmdir", func() error { return f.Rmdir(ctx, "nested/empty") }},
			{"Update", func() error { return o.Update(ctx, strings.NewReader("bad"), info) }},
			{"Remove", func() error { return o.Remove(ctx) }},
			{"SetModTime", func() error { return o.SetModTime(ctx, modTime.Add(time.Hour)) }},
		} {
			t.Run(test.name, func(t *testing.T) {
				err := test.mutate()
				assert.True(t, errors.Is(err, rfs.ErrorPermissionDenied), "expected permission denied, got %v", err)
				assert.Equal(t, files["root.txt"], readObject(t, f, "root.txt"))
			})
		}
	})
}

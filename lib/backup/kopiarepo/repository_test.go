package kopiarepo

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kopia/kopia/repo"
	"github.com/rclone/rclone/lib/backup/kopiablob"
)

func TestConnectionLifecycle(t *testing.T) {
	ctx := context.Background()
	password := "test-only-secret-not-stored-in-connection-config"
	dir := t.TempDir()
	st, err := kopiablob.New(ctx, &kopiablob.Options{Remote: dir}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Initialize(ctx, st, nil, password); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "persistent-cache")
	r, err := Open(ctx, kopiablob.Options{Remote: dir, ReadOnly: true}, password, cache)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(ctx) })
	config, err := os.ReadFile(filepath.Join(r.dir, "connection.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(config, []byte(password)) {
		t.Fatal("repository password persisted in connection file")
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	// A marker belongs to the caller, not to the temporary connection.
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(cache, "caller-owned-marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := r.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(r.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary connection leaked after close: %v", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatalf("caller-owned cache was removed: %q %v", data, err)
	}
	// A cache-free connection must still open the repository.
	fresh, err := Open(ctx, kopiablob.Options{Remote: dir, ReadOnly: true}, password, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFailedOpenCleansPrivateState(t *testing.T) {
	temp := t.TempDir()
	for _, variable := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(variable, temp)
	}
	missing := filepath.Join(t.TempDir(), "missing-repository")
	if _, err := Open(context.Background(), kopiablob.Options{Remote: missing, ReadOnly: true}, "password", ""); err == nil {
		t.Fatal("opened nonexistent repository")
	}
	entries, err := os.ReadDir(temp)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) >= len("rclone-kopia-") && entry.Name()[:len("rclone-kopia-")] == "rclone-kopia-" {
			t.Fatalf("failed connection leaked %s", entry.Name())
		}
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only failed open mutated storage: %v", err)
	}
}

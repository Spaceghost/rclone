package projection

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	fsobject "github.com/rclone/rclone/fs/object"
)

func TestPathHelpers(t *testing.T) {
	tests := []struct {
		root, remote, virtual string
	}{
		{"", "docs/readme.txt", "/docs/readme.txt"},
		{"docs", "readme.txt", "/docs/readme.txt"},
		{"", "", "/"},
	}
	for _, test := range tests {
		if got := virtualPath(test.root, test.remote); got != test.virtual {
			t.Errorf("virtualPath(%q, %q) = %q", test.root, test.remote, got)
		}
	}
}

func TestChildRemainder(t *testing.T) {
	if got, ok := childRemainder("docs", "docs/guides/start.pdf"); !ok || got != "guides/start.pdf" {
		t.Fatalf("childRemainder = %q, %v", got, ok)
	}
	if _, ok := childRemainder("doc", "docs/file"); ok {
		t.Fatal("component prefix must not match")
	}
}

func TestTrimPathPrefix(t *testing.T) {
	if got, ok := trimPathPrefix("published/guides/start.pdf", "published"); !ok || got != "guides/start.pdf" {
		t.Fatalf("trimPathPrefix = %q, %v", got, ok)
	}
	if _, ok := trimPathPrefix("publication/file", "published"); ok {
		t.Fatal("component prefix must not match")
	}
}

func TestBackendListsReadsAndDeniesWrites(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	upstreamRoot := filepath.Join(root, "upstream")
	if err := os.MkdirAll(filepath.Join(upstreamRoot, "manuals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upstreamRoot, "README.md"), []byte("projected readme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upstreamRoot, "manuals", "start.txt"), []byte("start here\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	remote := ":local:" + filepath.ToSlash(upstreamRoot)
	source := fmt.Sprintf(`package projection
manifest: {
  version: "v1alpha1"
  upstreams: {
    exact: remote: %q
    tree: remote: %q
  }
  routes: [{
    name: "readme"
    match: {kind: "exact", path: "/docs/readme.txt"}
    target: {upstream: "exact", path: "README.md"}
    access: "read-only"
  }, {
    name: "manuals"
    match: {kind: "prefix", path: "/manuals/"}
    target: {upstream: "tree", path: "manuals"}
    access: "read-only"
  }]
}`, remote, remote)
	manifestPath := filepath.Join(root, "projection.cue")
	if err := os.WriteFile(manifestPath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	backend, err := NewFs(ctx, "projected", "", configmap.Simple{"manifest": manifestPath})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := backend.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Remote() != "docs" || entries[1].Remote() != "manuals" {
		t.Fatalf("unexpected root entries: %v", entries)
	}

	object, err := backend.NewObject(ctx, "docs/readme.txt")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := object.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if string(data) != "projected readme\n" {
		t.Fatalf("unexpected data %q", data)
	}

	entries, err = backend.List(ctx, "manuals")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Remote() != "manuals/start.txt" {
		t.Fatalf("unexpected prefix entries: %v", entries)
	}

	src := fsobject.NewStaticObjectInfo("manuals/new.txt", time.Now(), 3, true, nil, backend)
	if _, err := backend.Put(ctx, bytes.NewReader([]byte("new")), src); !errors.Is(err, fs.ErrorPermissionDenied) {
		t.Fatalf("Put error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(upstreamRoot, "manuals", "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write reached upstream: %v", err)
	}
}

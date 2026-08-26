package resolver

import (
	"context"
	"errors"
	"testing"

	"github.com/Spaceghost/rclone-projection-vfs/internal/model"
)

func TestStaticResolverUsesExactThenLongestPrefix(t *testing.T) {
	m := model.Manifest{
		Version: "v1alpha1",
		Upstreams: map[string]model.Upstream{
			"archive": {Remote: "archive:"},
		},
		Routes: []model.Route{
			route("fallback", "prefix", "/", "root"),
			route("docs", "prefix", "/docs/", "published"),
			route("readme", "exact", "/docs/readme.txt", "special/readme.txt"),
		},
	}
	r, err := NewStatic(m)
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		route  string
		target string
	}{
		"/docs/readme.txt": {"readme", "special/readme.txt"},
		"/docs/guide.pdf":  {"docs", "published/guide.pdf"},
		"/other.bin":       {"fallback", "root/other.bin"},
	}
	for input, expected := range tests {
		got, err := r.Resolve(context.Background(), input)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", input, err)
		}
		if got.RouteName != expected.route || got.Target.Path != expected.target {
			t.Errorf("Resolve(%q) = route %q target %q", input, got.RouteName, got.Target.Path)
		}
	}
}

func TestStaticResolverRejectsUncleanPath(t *testing.T) {
	m := model.Manifest{
		Version:   "v1alpha1",
		Upstreams: map[string]model.Upstream{"archive": {Remote: "archive:"}},
		Routes:    []model.Route{route("all", "prefix", "/", "root")},
	}
	r, err := NewStatic(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), "/docs/../secret"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func route(name, kind, matchPath, targetPath string) model.Route {
	return model.Route{
		Name:   name,
		Match:  model.Match{Kind: kind, Path: matchPath},
		Target: model.Target{Upstream: "archive", Path: targetPath},
		Access: "read-only",
	}
}

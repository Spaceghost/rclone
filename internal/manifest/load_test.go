package manifest

import "testing"

func TestLoad(t *testing.T) {
	source := []byte(`package projection
manifest: {
  version: "v1alpha1"
  upstreams: docs: remote: "docs-source:"
  routes: [{
    name: "docs"
    match: {kind: "prefix", path: "/docs/"}
    target: {upstream: "docs", path: "public"}
    access: "read-only"
  }]
}`)

	m, err := Load(source, "test.cue")
	if err != nil {
		t.Fatal(err)
	}
	if m.Routes[0].Name != "docs" {
		t.Fatalf("unexpected route: %#v", m.Routes[0])
	}
}

func TestLoadRejectsTraversal(t *testing.T) {
	source := []byte(`package projection
manifest: {
  version: "v1alpha1"
  upstreams: docs: remote: "archive:"
  routes: [{
    name: "escape"
    match: {kind: "exact", path: "/secret"}
    target: {upstream: "docs", path: "../secret"}
    access: "read-only"
  }]
}`)

	if _, err := Load(source, "test.cue"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestLoadRejectsFileDirectoryCollision(t *testing.T) {
	source := []byte(`package projection
manifest: {
  version: "v1alpha1"
  upstreams: docs: remote: "archive:"
  routes: [{
    name: "file"
    match: {kind: "exact", path: "/docs"}
    target: {upstream: "docs", path: "README.md"}
    access: "read-only"
  }, {
    name: "child"
    match: {kind: "exact", path: "/docs/child.txt"}
    target: {upstream: "docs", path: "child.txt"}
    access: "read-only"
  }]
}`)

	if _, err := Load(source, "test.cue"); err == nil {
		t.Fatal("expected file/directory collision to be rejected")
	}
}

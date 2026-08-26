package manifest

import "testing"

func TestLoad(t *testing.T) {
	source := []byte(`package projection
manifest: {
  version: "v1alpha1"
  upstreams: docs: {kind: "http", baseURL: "https://example.test/objects/"}
  routes: [{
    name: "docs"
    match: {kind: "prefix", path: "/docs/"}
    target: {upstream: "docs", path: "public"}
    delivery: mode: "proxy"
    cache: {mode: "read-through", ttl: "5m"}
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
  upstreams: docs: {kind: "rclone", remote: "archive"}
  routes: [{
    name: "escape"
    match: {kind: "exact", path: "/secret"}
    target: {upstream: "docs", path: "../secret"}
    delivery: mode: "proxy"
    cache: mode: "disabled"
  }]
}`)

	if _, err := Load(source, "test.cue"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

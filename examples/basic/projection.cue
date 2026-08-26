package projection

// A standalone static manifest. Credentials are intentionally absent; adapters
// will obtain them from their configured credential providers.
manifest: {
	version: "v1alpha1"

	upstreams: {
		docs: {
			kind:    "http"
			baseURL: "https://example.invalid/objects/"
		}
		archive: {
			kind:   "rclone"
			remote: "archive"
		}
	}

	routes: [{
		name: "readme"
		match: {
			kind: "exact"
			path: "/docs/readme.txt"
		}
		target: {
			upstream: "docs"
			path:     "readme.txt"
		}
		delivery: mode: "redirect"
		cache: mode: "disabled"
	}, {
		name: "public-archive"
		match: {
			kind: "prefix"
			path: "/public/"
		}
		target: {
			upstream: "archive"
			path:     "published"
		}
		delivery: mode: "proxy"
		cache: {
			mode: "read-through"
			ttl:  "15m"
		}
	}]
}

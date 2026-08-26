package projection

// A standalone static manifest. Each upstream is another configured rclone
// remote, so credentials and transport behavior remain owned by rclone.
manifest: {
	version: "v1alpha1"

	upstreams: {
		docs: {
			remote: ":local:"
		}
		archive: {
			remote: ":local:"
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
			path:     "README.md"
		}
		access: "read-only"
	}, {
		name: "public-archive"
		match: {
			kind: "prefix"
			path: "/public/"
		}
		target: {
			upstream: "archive"
			path:     "docs"
		}
		access: "read-only"
	}]
}

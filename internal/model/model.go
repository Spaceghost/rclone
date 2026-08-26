package model

// Manifest is the versioned, protocol-neutral projection configuration.
type Manifest struct {
	Version   string              `json:"version"`
	Upstreams map[string]Upstream `json:"upstreams"`
	Routes    []Route             `json:"routes"`
}

type Upstream struct {
	Remote string `json:"remote"`
}

type Route struct {
	Name   string `json:"name"`
	Match  Match  `json:"match"`
	Target Target `json:"target"`
	Access string `json:"access"`
}

type Match struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type Target struct {
	Upstream string `json:"upstream"`
	Path     string `json:"path"`
}

// Resolution is the stable hand-off between the core resolver and protocol
// operations. The projection backend executes this plan through another
// configured rclone remote; the resolver never performs I/O.
type Resolution struct {
	RouteName string   `json:"routeName"`
	Path      string   `json:"path"`
	Target    Target   `json:"target"`
	Upstream  Upstream `json:"upstream"`
	Access    string   `json:"access"`
}

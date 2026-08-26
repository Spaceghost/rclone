package model

// Manifest is the versioned, protocol-neutral projection configuration.
type Manifest struct {
	Version   string              `json:"version"`
	Upstreams map[string]Upstream `json:"upstreams"`
	Routes    []Route             `json:"routes"`
}

type Upstream struct {
	Kind     string `json:"kind"`
	Remote   string `json:"remote,omitempty"`
	BaseURL  string `json:"baseURL,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
}

type Route struct {
	Name     string   `json:"name"`
	Match    Match    `json:"match"`
	Target   Target   `json:"target"`
	Delivery Delivery `json:"delivery"`
	Cache    Cache    `json:"cache"`
}

type Match struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type Target struct {
	Upstream string `json:"upstream"`
	Path     string `json:"path"`
}

type Delivery struct {
	Mode string `json:"mode"`
}

type Cache struct {
	Mode string `json:"mode"`
	TTL  string `json:"ttl,omitempty"`
}

// Resolution is the stable hand-off between the core resolver and protocol
// adapters. Backends execute this plan; the resolver never performs I/O.
type Resolution struct {
	RouteName string   `json:"routeName"`
	Path      string   `json:"path"`
	Target    Target   `json:"target"`
	Upstream  Upstream `json:"upstream"`
	Delivery  Delivery `json:"delivery"`
	Cache     Cache    `json:"cache"`
}

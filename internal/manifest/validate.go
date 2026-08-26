package manifest

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Spaceghost/rclone-projection-vfs/internal/model"
)

var supportedUpstreams = map[string]bool{"rclone": true, "http": true, "s3": true}
var supportedDelivery = map[string]bool{"proxy": true, "redirect": true}
var supportedCache = map[string]bool{
	"disabled": true, "read-through": true, "write-through": true, "write-back": true,
}

func Validate(m model.Manifest) error {
	if m.Version != "v1alpha1" {
		return fmt.Errorf("manifest version must be v1alpha1")
	}
	if len(m.Upstreams) == 0 {
		return fmt.Errorf("at least one upstream is required")
	}
	for name, upstream := range m.Upstreams {
		if name == "" {
			return fmt.Errorf("upstream name cannot be empty")
		}
		if !supportedUpstreams[upstream.Kind] {
			return fmt.Errorf("upstream %q has unsupported kind %q", name, upstream.Kind)
		}
		if upstream.Kind == "http" {
			parsed, err := url.Parse(upstream.BaseURL)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				return fmt.Errorf("HTTP upstream %q requires an absolute baseURL", name)
			}
		}
	}

	names := make(map[string]bool, len(m.Routes))
	exactPaths := make(map[string]bool)
	prefixPaths := make(map[string]bool)
	for index, route := range m.Routes {
		label := fmt.Sprintf("route[%d]", index)
		if route.Name == "" {
			return fmt.Errorf("%s requires a name", label)
		}
		if names[route.Name] {
			return fmt.Errorf("duplicate route name %q", route.Name)
		}
		names[route.Name] = true

		if route.Match.Path == "" || !strings.HasPrefix(route.Match.Path, "/") {
			return fmt.Errorf("route %q match path must be a clean absolute path", route.Name)
		}
		switch route.Match.Kind {
		case "exact":
			if path.Clean(route.Match.Path) != route.Match.Path {
				return fmt.Errorf("route %q exact path must be clean", route.Name)
			}
			if exactPaths[route.Match.Path] {
				return fmt.Errorf("duplicate exact path %q", route.Match.Path)
			}
			exactPaths[route.Match.Path] = true
		case "prefix":
			if route.Match.Path != "/" && !strings.HasSuffix(route.Match.Path, "/") {
				return fmt.Errorf("route %q prefix path must end in /", route.Name)
			}
			if route.Match.Path != "/" && path.Clean(strings.TrimSuffix(route.Match.Path, "/")) != strings.TrimSuffix(route.Match.Path, "/") {
				return fmt.Errorf("route %q prefix path must be clean", route.Name)
			}
			if prefixPaths[route.Match.Path] {
				return fmt.Errorf("duplicate prefix path %q", route.Match.Path)
			}
			prefixPaths[route.Match.Path] = true
		default:
			return fmt.Errorf("route %q has unsupported match kind %q", route.Name, route.Match.Kind)
		}

		if _, ok := m.Upstreams[route.Target.Upstream]; !ok {
			return fmt.Errorf("route %q references unknown upstream %q", route.Name, route.Target.Upstream)
		}
		if err := validateTargetPath(route.Target.Path); err != nil {
			return fmt.Errorf("route %q target path: %w", route.Name, err)
		}
		if !supportedDelivery[route.Delivery.Mode] {
			return fmt.Errorf("route %q has unsupported delivery mode %q", route.Name, route.Delivery.Mode)
		}
		if !supportedCache[route.Cache.Mode] {
			return fmt.Errorf("route %q has unsupported cache mode %q", route.Name, route.Cache.Mode)
		}
		if route.Cache.TTL != "" {
			if _, err := time.ParseDuration(route.Cache.TTL); err != nil {
				return fmt.Errorf("route %q has invalid cache ttl: %w", route.Name, err)
			}
		}
	}
	return nil
}

func validateTargetPath(value string) error {
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "\\") {
		return fmt.Errorf("must be relative")
	}
	cleaned := path.Clean(strings.ReplaceAll(value, "\\", "/"))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("must not escape the upstream root")
	}
	return nil
}

package resolver

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"

	"github.com/Spaceghost/rclone-projection-vfs/internal/manifest"
	"github.com/Spaceghost/rclone-projection-vfs/internal/model"
)

var ErrNotFound = errors.New("projection path not found")

type Resolver interface {
	Resolve(context.Context, string) (model.Resolution, error)
}

type Static struct {
	manifest model.Manifest
	exact    map[string]model.Route
	prefixes []model.Route
}

func NewStatic(m model.Manifest) (*Static, error) {
	if err := manifest.Validate(m); err != nil {
		return nil, err
	}
	r := &Static{manifest: m, exact: make(map[string]model.Route)}
	for _, route := range m.Routes {
		if route.Match.Kind == "exact" {
			r.exact[route.Match.Path] = route
		} else {
			r.prefixes = append(r.prefixes, route)
		}
	}
	sort.Slice(r.prefixes, func(i, j int) bool {
		return len(r.prefixes[i].Match.Path) > len(r.prefixes[j].Match.Path)
	})
	return r, nil
}

func (r *Static) Resolve(_ context.Context, projectionPath string) (model.Resolution, error) {
	cleaned := path.Clean(projectionPath)
	if !strings.HasPrefix(projectionPath, "/") || cleaned != projectionPath {
		return model.Resolution{}, ErrNotFound
	}
	if route, ok := r.exact[projectionPath]; ok {
		return r.resolution(route, projectionPath, ""), nil
	}
	for _, route := range r.prefixes {
		if strings.HasPrefix(projectionPath, route.Match.Path) {
			suffix := strings.TrimPrefix(projectionPath, route.Match.Path)
			return r.resolution(route, projectionPath, suffix), nil
		}
	}
	return model.Resolution{}, ErrNotFound
}

func (r *Static) resolution(route model.Route, projectionPath, suffix string) model.Resolution {
	target := route.Target
	if suffix != "" {
		target.Path = path.Join(target.Path, suffix)
	}
	return model.Resolution{
		RouteName: route.Name,
		Path:      projectionPath,
		Target:    target,
		Upstream:  r.manifest.Upstreams[target.Upstream],
		Delivery:  route.Delivery,
		Cache:     route.Cache,
	}
}

package manifest

import (
	"fmt"
	"os"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	"github.com/Spaceghost/rclone-projection-vfs/internal/model"
)

// LoadFile compiles a standalone CUE file and decodes its concrete `manifest`
// value. Tree discovery and embedded manifests are intentionally a later phase.
func LoadFile(name string) (model.Manifest, error) {
	source, err := os.ReadFile(name)
	if err != nil {
		return model.Manifest{}, err
	}
	return Load(source, name)
}

// Load compiles a manifest from CUE source.
func Load(source []byte, filename string) (model.Manifest, error) {
	ctx := cuecontext.New()
	value := ctx.CompileBytes(source, cue.Filename(filename))
	if err := value.Err(); err != nil {
		return model.Manifest{}, fmt.Errorf("compile CUE: %w", err)
	}

	manifestValue := value.LookupPath(cue.ParsePath("manifest"))
	if !manifestValue.Exists() {
		return model.Manifest{}, fmt.Errorf("CUE value %q is required", "manifest")
	}
	if err := manifestValue.Validate(); err != nil {
		return model.Manifest{}, fmt.Errorf("validate CUE manifest: %w", err)
	}

	var decoded model.Manifest
	if err := manifestValue.Decode(&decoded); err != nil {
		return model.Manifest{}, fmt.Errorf("decode CUE manifest: %w", err)
	}
	if err := Validate(decoded); err != nil {
		return model.Manifest{}, err
	}
	return decoded, nil
}

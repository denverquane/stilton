// Package mesh loads STL, OBJ, and 3MF files into fauxgl meshes for rendering.
package mesh

import (
	"fmt"

	"github.com/fogleman/fauxgl"
)

// Load reads a model file of the given format ("stl", "obj", "3mf").
func Load(path, format string) (m *fauxgl.Mesh, err error) {
	// fauxgl's parsers index without bounds checks and panic on malformed files.
	defer func() {
		if r := recover(); r != nil {
			m, err = nil, fmt.Errorf("malformed %s: %v", format, r)
		}
	}()
	switch format {
	case "stl":
		m, err = LoadSTL(path)
	case "obj":
		m, err = fauxgl.LoadOBJ(path)
	case "3mf":
		m, err = Load3MF(path)
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
	if err != nil {
		return nil, err
	}
	if len(m.Triangles) == 0 {
		return nil, fmt.Errorf("mesh has no triangles")
	}
	return m, nil
}

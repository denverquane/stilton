// Package mesh loads STL, OBJ, and 3MF files into fauxgl meshes for rendering.
package mesh

import (
	"errors"
	"fmt"

	"github.com/fogleman/fauxgl"
)

// DefaultColor is used for models (and parts of models) with no color of
// their own.
var DefaultColor = fauxgl.HexColor("#e8a33d")

// Plate selectors for multi-plate slicer projects (see LoadModel). Positive
// values are plate numbers.
const (
	AllPlates  = 0
	PlateFirst = -1
)

// ErrNoPlate means the requested plate isn't in the file.
var ErrNoPlate = errors.New("no such plate")

// Loaded is a loaded model.
type Loaded struct {
	Mesh *fauxgl.Mesh
	// Colored meshes carry their colors in their vertices. Only 3MFs that use
	// two or more colors are colored.
	Colored bool
	// Plates lists a multi-plate slicer project's plate numbers, and Plate
	// is the one loaded (0 for all, or for files without plates).
	Plates []int
	Plate  int
}

// Load reads a model file of the given format ("stl", "obj", "3mf"), every
// plate of it.
func Load(path, format string) (*fauxgl.Mesh, error) {
	l, err := LoadModel(path, format, AllPlates)
	return l.Mesh, err
}

// LoadPreview loads a model the way it should look in a thumbnail: only the
// first plate of a multi-plate slicer project.
func LoadPreview(path, format string) (Loaded, error) {
	return LoadModel(path, format, PlateFirst)
}

// LoadModel reads a model file, and for multi-plate 3MFs, one plate of it
// (or AllPlates).
func LoadModel(path, format string, plate int) (l Loaded, err error) {
	// fauxgl's parsers index without bounds checks and panic on malformed files.
	defer func() {
		if r := recover(); r != nil {
			l, err = Loaded{}, fmt.Errorf("malformed %s: %v", format, r)
		}
	}()
	switch format {
	case "stl":
		l.Mesh, err = LoadSTL(path)
	case "obj":
		l.Mesh, err = fauxgl.LoadOBJ(path)
	case "3mf":
		l, err = Load3MF(path, plate)
	default:
		return Loaded{}, fmt.Errorf("unsupported format %q", format)
	}
	if err == nil && format != "3mf" && plate > 0 {
		err = ErrNoPlate
	}
	if err != nil {
		return Loaded{}, err
	}
	if len(l.Mesh.Triangles) == 0 {
		return Loaded{}, fmt.Errorf("mesh has no triangles")
	}
	return l, nil
}

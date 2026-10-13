package mesh

import (
	"testing"

	"github.com/fogleman/fauxgl"
)

func TestUpMatrix(t *testing.T) {
	for up, axis := range map[string]fauxgl.Vector{
		"":   fauxgl.V(0, 0, 1),
		"+z": fauxgl.V(0, 0, 1),
		"-z": fauxgl.V(0, 0, -1),
		"+y": fauxgl.V(0, 1, 0),
		"-y": fauxgl.V(0, -1, 0),
		"+x": fauxgl.V(1, 0, 0),
		"-x": fauxgl.V(-1, 0, 0),
	} {
		m := UpMatrix(up)
		if got := m.MulPosition(axis); got != fauxgl.V(0, 0, 1) {
			t.Errorf("%q: %v maps to %v, want +Z", up, axis, got)
		}
		if d := m.Determinant(); d != 1 {
			t.Errorf("%q: determinant %v, want 1 (a rotation, not a mirror)", up, d)
		}
	}
}

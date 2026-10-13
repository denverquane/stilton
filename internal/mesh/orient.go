package mesh

import "github.com/fogleman/fauxgl"

// UpMatrix rotates a model whose up axis is up ("+y", "-x", ...) so that it
// points up along +Z, the convention for print files. "" and "+z" need no
// rotation. The web viewer has the same table; keep them in step.
func UpMatrix(up string) fauxgl.Matrix {
	m := fauxgl.Identity()
	switch up {
	case "-z":
		m.X11, m.X22 = -1, -1
	case "+y":
		m.X11, m.X12, m.X21, m.X22 = 0, -1, 1, 0
	case "-y":
		m.X11, m.X12, m.X21, m.X22 = 0, 1, -1, 0
	case "+x":
		m.X00, m.X02, m.X20, m.X22 = 0, -1, 1, 0
	case "-x":
		m.X00, m.X02, m.X20, m.X22 = 0, 1, -1, 0
	}
	return m
}

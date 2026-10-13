package mesh

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fogleman/fauxgl"
)

// A unit square from two triangles sharing an edge.
func square(c1, c2 fauxgl.Color) *fauxgl.Mesh {
	a, b, c, d := fauxgl.V(0, 0, 0), fauxgl.V(1, 0, 0), fauxgl.V(1, 1, 0), fauxgl.V(0, 1, 0)
	t1 := fauxgl.NewTriangleForPoints(a, b, c)
	t2 := fauxgl.NewTriangleForPoints(a, c, d)
	t1.V1.Color, t1.V2.Color, t1.V3.Color = c1, c1, c1
	t2.V1.Color, t2.V2.Color, t2.V3.Color = c2, c2, c2
	return fauxgl.NewTriangleMesh([]*fauxgl.Triangle{t1, t2})
}

func ply(t *testing.T, m *fauxgl.Mesh, colored bool) (header string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteBinaryPLY(&buf, m, colored); err != nil {
		t.Fatal(err)
	}
	h, b, ok := bytes.Cut(buf.Bytes(), []byte("end_header\n"))
	if !ok {
		t.Fatal("no end_header")
	}
	return string(h), b
}

func TestWriteBinaryPLY(t *testing.T) {
	red, blue := parseColor("#FF0000"), parseColor("#0000FF")

	// One color: the shared edge's two vertices are merged, 4 in all.
	h, body := ply(t, square(red, red), true)
	if !strings.Contains(h, "element vertex 4\n") || !strings.Contains(h, "property uchar red") {
		t.Errorf("one-color header:\n%s", h)
	}
	if want := 4*15 + 2*13; len(body) != want {
		t.Errorf("one-color body %d bytes, want %d", len(body), want)
	}
	// Colors pass through DisplayColor: pure red becomes a lifted red.
	if r := body[12]; r != 255 || body[13] == 0 {
		t.Errorf("first vertex color %v, want lifted red", body[12:15])
	}

	// Two colors: the shared edge is split so the boundary stays sharp.
	h, _ = ply(t, square(red, blue), true)
	if !strings.Contains(h, "element vertex 6\n") {
		t.Errorf("two-color header:\n%s", h)
	}

	// Uncolored: no color properties, 12-byte vertices.
	h, body = ply(t, square(red, blue), false)
	if strings.Contains(h, "red") || !strings.Contains(h, "element vertex 4\n") {
		t.Errorf("uncolored header:\n%s", h)
	}
	if want := 4*12 + 2*13; len(body) != want {
		t.Errorf("uncolored body %d bytes, want %d", len(body), want)
	}
}

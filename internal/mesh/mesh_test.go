package mesh

import (
	"archive/zip"
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/fogleman/fauxgl"
)

func write3MF(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.3mf")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

const testRels = `<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Target="/3D/3dmodel.model" Id="rel0" Type="http://schemas.microsoft.com/3dmanufacturing/2013/01/3dmodel"/>
</Relationships>`

// TestLoad3MFTransforms checks matrix layout and composition order using a
// component (rotate 90° about Z, then translate +10 X) stored in a separate
// part via p:path, inside a build item that rotates another 90° about Z.
func TestLoad3MFTransforms(t *testing.T) {
	path := write3MF(t, map[string]string{
		"_rels/.rels": testRels,
		"3D/3dmodel.model": `<?xml version="1.0" encoding="UTF-8"?>
<model unit="millimeter" xmlns="http://schemas.microsoft.com/3dmanufacturing/core/2015/02"
       xmlns:p="http://schemas.microsoft.com/3dmanufacturing/production/2015/06">
  <resources>
    <object id="2" type="model">
      <components>
        <component p:path="/3D/Objects/part.model" objectid="1" transform="0 1 0 -1 0 0 0 0 1 10 0 0"/>
      </components>
    </object>
  </resources>
  <build><item objectid="2" transform="0 1 0 -1 0 0 0 0 1 0 0 0"/></build>
</model>`,
		"3D/Objects/part.model": `<?xml version="1.0" encoding="UTF-8"?>
<model unit="millimeter" xmlns="http://schemas.microsoft.com/3dmanufacturing/core/2015/02">
  <resources>
    <object id="1" type="model">
      <mesh>
        <vertices><vertex x="2" y="0" z="0"/><vertex x="0" y="0" z="0"/><vertex x="0" y="0" z="1"/></vertices>
        <triangles><triangle v1="0" v2="1" v3="2"/></triangles>
      </mesh>
    </object>
  </resources>
</model>`,
	})

	m, err := Load(path, "3mf")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Triangles) != 1 {
		t.Fatalf("got %d triangles, want 1", len(m.Triangles))
	}
	// Component: (2,0,0) -> rotate (0,2,0) -> translate (10,2,0); item: rotate -> (-2,10,0).
	box := m.BoundingBox()
	wantMin, wantMax := fauxgl.V(-2, 10, 0), fauxgl.V(0, 10, 1)
	if !near(box.Min, wantMin) || !near(box.Max, wantMax) {
		t.Errorf("bounding box = %v..%v, want %v..%v", box.Min, box.Max, wantMin, wantMax)
	}
}

func TestLoad3MFBadIndex(t *testing.T) {
	path := write3MF(t, map[string]string{
		"_rels/.rels": testRels,
		"3D/3dmodel.model": `<model xmlns="http://schemas.microsoft.com/3dmanufacturing/core/2015/02">
  <resources><object id="1"><mesh>
    <vertices><vertex x="0" y="0" z="0"/></vertices>
    <triangles><triangle v1="0" v2="1" v3="2"/></triangles>
  </mesh></object></resources>
  <build><item objectid="1"/></build>
</model>`,
	})
	if _, err := Load(path, "3mf"); err == nil {
		t.Fatal("expected an error for out-of-range triangle indices")
	}
}

func TestLoadMalformedSTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.stl")
	os.WriteFile(path, []byte("solid x\nvertex 1 2 3\nvertex 4 5 6\nendsolid\n"), 0o644)
	if _, err := Load(path, "stl"); err == nil {
		t.Fatal("expected an error for a truncated ASCII STL")
	}
}

func near(a, b fauxgl.Vector) bool {
	const eps = 1e-9
	return math.Abs(a.X-b.X) < eps && math.Abs(a.Y-b.Y) < eps && math.Abs(a.Z-b.Z) < eps
}

func TestLoadPaddedBinarySTL(t *testing.T) {
	var buf bytes.Buffer
	tri := fauxgl.NewTriangleForPoints(fauxgl.V(0, 0, 0), fauxgl.V(1, 0, 0), fauxgl.V(0, 1, 0))
	if err := WriteBinarySTL(&buf, fauxgl.NewTriangleMesh([]*fauxgl.Triangle{tri, tri})); err != nil {
		t.Fatal(err)
	}
	buf.Write(make([]byte, 604)) // trailing padding, as Meshmixer writes

	path := filepath.Join(t.TempDir(), "padded.stl")
	os.WriteFile(path, buf.Bytes(), 0o644)
	m, err := Load(path, "stl")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Triangles) != 2 {
		t.Fatalf("got %d triangles, want 2", len(m.Triangles))
	}
}

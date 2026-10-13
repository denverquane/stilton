package mesh

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/fogleman/fauxgl"
)

const (
	rel3DModel   = "http://schemas.microsoft.com/3dmanufacturing/2013/01/3dmodel"
	relThumbnail = "http://schemas.openxmlformats.org/package/2006/relationships/metadata/thumbnail"

	maxComponentDepth = 16
)

type xmlRelationships struct {
	Rels []struct {
		Target string `xml:"Target,attr"`
		Type   string `xml:"Type,attr"`
	} `xml:"Relationship"`
}

type xmlModel struct {
	Objects []xmlObject `xml:"resources>object"`
	Items   []struct {
		ObjectID  int    `xml:"objectid,attr"`
		Transform string `xml:"transform,attr"`
		Path      string `xml:"http://schemas.microsoft.com/3dmanufacturing/production/2015/06 path,attr"`
	} `xml:"build>item"`
}

type xmlObject struct {
	ID       int `xml:"id,attr"`
	Vertices []struct {
		X float64 `xml:"x,attr"`
		Y float64 `xml:"y,attr"`
		Z float64 `xml:"z,attr"`
	} `xml:"mesh>vertices>vertex"`
	Triangles []struct {
		V1 int `xml:"v1,attr"`
		V2 int `xml:"v2,attr"`
		V3 int `xml:"v3,attr"`
	} `xml:"mesh>triangles>triangle"`
	Components []struct {
		ObjectID  int    `xml:"objectid,attr"`
		Transform string `xml:"transform,attr"`
		Path      string `xml:"http://schemas.microsoft.com/3dmanufacturing/production/2015/06 path,attr"`
	} `xml:"components>component"`
}

// threeMF lazily parses the model parts of a 3MF package. Geometry may be
// split across parts via the production extension's p:path (Bambu Studio and
// Orca do this), so parts are loaded on demand and cached.
type threeMF struct {
	zr    *zip.ReadCloser
	parts map[string]map[int]*xmlObject
}

// Load3MF reads every build item of a 3MF file into a single mesh, with
// component and item transforms applied.
func Load3MF(filename string) (*fauxgl.Mesh, error) {
	zr, err := zip.OpenReader(filename)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	pkg := &threeMF{zr: zr, parts: map[string]map[int]*xmlObject{}}

	root := "3D/3dmodel.model"
	if rels, err := pkg.rels(); err == nil {
		for _, r := range rels.Rels {
			if r.Type == rel3DModel {
				root = partName(r.Target)
				break
			}
		}
	}

	f, err := pkg.zr.Open(root)
	if err != nil {
		return nil, err
	}
	var model xmlModel
	err = xml.NewDecoder(f).Decode(&model)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", root, err)
	}
	pkg.parts[root] = indexObjects(model.Objects)

	var tris []*fauxgl.Triangle
	for _, item := range model.Items {
		t, err := parseTransform(item.Transform)
		if err != nil {
			return nil, err
		}
		part := root
		if item.Path != "" {
			part = partName(item.Path)
		}
		if tris, err = pkg.appendObject(tris, part, item.ObjectID, t, 0); err != nil {
			return nil, err
		}
	}
	return fauxgl.NewTriangleMesh(tris), nil
}

func (p *threeMF) appendObject(tris []*fauxgl.Triangle, part string, id int, m fauxgl.Matrix, depth int) ([]*fauxgl.Triangle, error) {
	if depth > maxComponentDepth {
		return nil, fmt.Errorf("components nested deeper than %d", maxComponentDepth)
	}
	objects, err := p.part(part)
	if err != nil {
		return nil, err
	}
	obj := objects[id]
	if obj == nil {
		return nil, fmt.Errorf("%s: object %d not found", part, id)
	}

	n := len(obj.Vertices)
	for _, t := range obj.Triangles {
		if t.V1 < 0 || t.V2 < 0 || t.V3 < 0 || t.V1 >= n || t.V2 >= n || t.V3 >= n {
			return nil, fmt.Errorf("%s: object %d: triangle index out of range", part, id)
		}
		v := func(i int) fauxgl.Vector {
			return m.MulPosition(fauxgl.V(obj.Vertices[i].X, obj.Vertices[i].Y, obj.Vertices[i].Z))
		}
		tris = append(tris, fauxgl.NewTriangleForPoints(v(t.V1), v(t.V2), v(t.V3)))
	}

	for _, c := range obj.Components {
		ct, err := parseTransform(c.Transform)
		if err != nil {
			return nil, err
		}
		cpart := part
		if c.Path != "" {
			cpart = partName(c.Path)
		}
		// The component's transform applies first, then its parent's.
		if tris, err = p.appendObject(tris, cpart, c.ObjectID, m.Mul(ct), depth+1); err != nil {
			return nil, err
		}
	}
	return tris, nil
}

func (p *threeMF) part(name string) (map[int]*xmlObject, error) {
	if objs, ok := p.parts[name]; ok {
		return objs, nil
	}
	f, err := p.zr.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var model xmlModel
	if err := xml.NewDecoder(f).Decode(&model); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	objs := indexObjects(model.Objects)
	p.parts[name] = objs
	return objs, nil
}

func (p *threeMF) rels() (*xmlRelationships, error) {
	f, err := p.zr.Open("_rels/.rels")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rels xmlRelationships
	if err := xml.NewDecoder(f).Decode(&rels); err != nil {
		return nil, err
	}
	return &rels, nil
}

func indexObjects(objs []xmlObject) map[int]*xmlObject {
	m := make(map[int]*xmlObject, len(objs))
	for i := range objs {
		m[objs[i].ID] = &objs[i]
	}
	return m
}

// partName converts an OPC part reference ("/3D/3dmodel.model") to a zip entry name.
func partName(target string) string {
	return strings.TrimPrefix(path.Clean("/"+target), "/")
}

// parseTransform converts a 3MF transform attribute to a fauxgl matrix.
//
// 3MF stores a 4x3 matrix for row vectors ("m00 m01 m02 m10 ... m32", with
// x' = x*m00 + y*m10 + z*m20 + m30). fauxgl multiplies column vectors, so the
// values are transposed into place.
func parseTransform(s string) (fauxgl.Matrix, error) {
	if strings.TrimSpace(s) == "" {
		return fauxgl.Identity(), nil
	}
	fields := strings.Fields(s)
	if len(fields) != 12 {
		return fauxgl.Matrix{}, fmt.Errorf("transform %q: want 12 values, got %d", s, len(fields))
	}
	var v [12]float64
	for i, f := range fields {
		var err error
		if v[i], err = strconv.ParseFloat(f, 64); err != nil {
			return fauxgl.Matrix{}, fmt.Errorf("transform %q: %w", s, err)
		}
	}
	return fauxgl.Matrix{
		X00: v[0], X01: v[3], X02: v[6], X03: v[9],
		X10: v[1], X11: v[4], X12: v[7], X13: v[10],
		X20: v[2], X21: v[5], X22: v[8], X23: v[11],
		X33: 1,
	}, nil
}

// EmbeddedThumbnail returns the preview image a slicer saved inside a 3MF, if
// any. The package thumbnail relationship is checked first, then the paths
// Bambu Studio / Orca / PrusaSlicer use.
func EmbeddedThumbnail(filename string) (image.Image, bool) {
	zr, err := zip.OpenReader(filename)
	if err != nil {
		return nil, false
	}
	defer zr.Close()
	pkg := &threeMF{zr: zr}

	candidates := []string{}
	if rels, err := pkg.rels(); err == nil {
		for _, r := range rels.Rels {
			if r.Type == relThumbnail {
				candidates = append(candidates, partName(r.Target))
			}
		}
	}
	candidates = append(candidates, "Metadata/plate_1.png", "Metadata/thumbnail.png")

	for _, name := range candidates {
		f, err := zr.Open(name)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, 32<<20))
		f.Close()
		if err != nil {
			continue
		}
		if img, _, err := image.Decode(bytes.NewReader(data)); err == nil {
			return img, true
		}
	}
	return nil, false
}

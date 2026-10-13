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

// Untagged namespaces match any namespace, so the materials extension's
// m:colorgroup and PrusaSlicer's slic3rpe:mmu_segmentation decode without
// spelling out their URLs.
type xmlModel struct {
	Objects       []xmlObject `xml:"resources>object"`
	BaseMaterials []struct {
		ID    int `xml:"id,attr"`
		Bases []struct {
			Color string `xml:"displaycolor,attr"`
		} `xml:"base"`
	} `xml:"resources>basematerials"`
	ColorGroups []struct {
		ID     int `xml:"id,attr"`
		Colors []struct {
			Color string `xml:"color,attr"`
		} `xml:"color"`
	} `xml:"resources>colorgroup"`
	Items []xmlItem `xml:"build>item"`
}

type xmlItem struct {
	ObjectID  int    `xml:"objectid,attr"`
	Transform string `xml:"transform,attr"`
	Path      string `xml:"http://schemas.microsoft.com/3dmanufacturing/production/2015/06 path,attr"`
}

type xmlObject struct {
	ID int `xml:"id,attr"`
	// Default material: a basematerials or colorgroup id, and an index into it.
	PID      string `xml:"pid,attr"`
	PIndex   string `xml:"pindex,attr"`
	Vertices []struct {
		X float64 `xml:"x,attr"`
		Y float64 `xml:"y,attr"`
		Z float64 `xml:"z,attr"`
	} `xml:"mesh>vertices>vertex"`
	Triangles  []xmlTriangle `xml:"mesh>triangles>triangle"`
	Components []struct {
		ObjectID  int    `xml:"objectid,attr"`
		Transform string `xml:"transform,attr"`
		Path      string `xml:"http://schemas.microsoft.com/3dmanufacturing/production/2015/06 path,attr"`
	} `xml:"components>component"`
}

type xmlTriangle struct {
	V1 int `xml:"v1,attr"`
	V2 int `xml:"v2,attr"`
	V3 int `xml:"v3,attr"`
	// Material per vertex (p1..p3) from a resource (pid); p2/p3 default to p1.
	PID string `xml:"pid,attr"`
	P1  string `xml:"p1,attr"`
	P2  string `xml:"p2,attr"`
	P3  string `xml:"p3,attr"`
	// Multi-material painting: Bambu Studio/Orca and PrusaSlicer.
	PaintColor string `xml:"paint_color,attr"`
	MMU        string `xml:"mmu_segmentation,attr"`
}

// modelPart is a parsed .model file: its objects and color resources.
type modelPart struct {
	objects map[int]*xmlObject
	colors  map[int][]fauxgl.Color // basematerials and colorgroups by id
}

// threeMF lazily parses the model parts of a 3MF package. Geometry may be
// split across parts via the production extension's p:path (Bambu Studio and
// Orca do this), so parts are loaded on demand and cached.
type threeMF struct {
	zr    *zip.ReadCloser
	parts map[string]*modelPart
	slots *slicerColors // nil unless the file came from a multi-material slicer
}

// Load3MF reads every build item of a 3MF file into a single mesh, with
// component and item transforms applied. When the file assigns two or more
// colors (3MF materials, or a slicer's filament assignments and painting),
// every vertex carries its color and the mesh is reported as colored;
// otherwise vertex colors are left unset.
//
// For multi-plate slicer projects, plate selects which plate to load: a plate
// number, PlateFirst, or AllPlates. The returned Loaded lists the plates.
func Load3MF(filename string, plate int) (Loaded, error) {
	zr, err := zip.OpenReader(filename)
	if err != nil {
		return Loaded{}, err
	}
	defer zr.Close()
	pkg := &threeMF{zr: zr, parts: map[string]*modelPart{}}
	pkg.slots = readSlicerColors(zr)
	plates := pkg.slots.plateNumbers()
	var onPlate map[int]bool // nil: every item
	if len(plates) > 0 {
		switch plate {
		case AllPlates:
		case PlateFirst:
			plate = plates[0]
			onPlate = pkg.slots.plates[plate]
		default:
			if onPlate = pkg.slots.plates[plate]; onPlate == nil {
				return Loaded{}, ErrNoPlate
			}
		}
	} else if plate > 0 {
		return Loaded{}, ErrNoPlate
	}

	root := "3D/3dmodel.model"
	if rels, err := pkg.rels(); err == nil {
		for _, r := range rels.Rels {
			if r.Type == rel3DModel {
				root = partName(r.Target)
				break
			}
		}
	}
	model, err := pkg.decode(root)
	if err != nil {
		return Loaded{}, err
	}

	b := &meshBuilder{}
	for _, item := range model.items {
		t, err := parseTransform(item.Transform)
		if err != nil {
			return Loaded{}, err
		}
		part := root
		if item.Path != "" {
			part = partName(item.Path)
		}
		if onPlate != nil && !onPlate[item.ObjectID] {
			continue
		}
		slot := slotRef{object: item.ObjectID}
		if err := pkg.appendObject(b, part, item.ObjectID, t, slot, 0); err != nil {
			return Loaded{}, err
		}
	}
	m, colored := b.mesh(pkg.slots.multiColor())
	return Loaded{Mesh: m, Colored: colored, Plates: plates, Plate: max(plate, 0)}, nil
}

// slotRef identifies where a slicer's filament assignment for some geometry
// lives: the build item's object, and the part (component) within it.
type slotRef struct {
	object, part int
	hasPart      bool
}

func (p *threeMF) appendObject(b *meshBuilder, part string, id int, m fauxgl.Matrix, slot slotRef, depth int) error {
	if depth > maxComponentDepth {
		return fmt.Errorf("components nested deeper than %d", maxComponentDepth)
	}
	mp, err := p.part(part)
	if err != nil {
		return err
	}
	obj := mp.objects[id]
	if obj == nil {
		return fmt.Errorf("%s: object %d not found", part, id)
	}

	n := len(obj.Vertices)
	for i, t := range obj.Triangles {
		if t.V1 < 0 || t.V2 < 0 || t.V3 < 0 || t.V1 >= n || t.V2 >= n || t.V3 >= n {
			return fmt.Errorf("%s: object %d: triangle index out of range", part, id)
		}
		v := func(i int) fauxgl.Vector {
			return m.MulPosition(fauxgl.V(obj.Vertices[i].X, obj.Vertices[i].Y, obj.Vertices[i].Z))
		}
		if !p.slots.trianglePrintable(slot, i) {
			continue
		}
		tri := fauxgl.NewTriangleForPoints(v(t.V1), v(t.V2), v(t.V3))
		colors, ok := p.triangleColors(mp, obj, t, i, slot)
		b.add(tri, colors, ok)
	}

	for _, c := range obj.Components {
		ct, err := parseTransform(c.Transform)
		if err != nil {
			return err
		}
		cpart := part
		if c.Path != "" {
			cpart = partName(c.Path)
		}
		cslot := slot
		if depth == 0 {
			cslot.part, cslot.hasPart = c.ObjectID, true
		}
		// Slicer modifiers, negative volumes and support blockers are
		// components too, but aren't part of the printed shape.
		if !p.slots.printable(cslot) {
			continue
		}
		// The component's transform applies first, then its parent's.
		if err := p.appendObject(b, cpart, c.ObjectID, m.Mul(ct), cslot, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// decodedModel is what a .model file contributes beyond its objects.
type decodedModel struct {
	items []xmlItem
}

func (p *threeMF) decode(name string) (decodedModel, error) {
	f, err := p.zr.Open(name)
	if err != nil {
		return decodedModel{}, err
	}
	defer f.Close()
	var model xmlModel
	if err := xml.NewDecoder(f).Decode(&model); err != nil {
		return decodedModel{}, fmt.Errorf("%s: %w", name, err)
	}
	mp := &modelPart{objects: map[int]*xmlObject{}, colors: map[int][]fauxgl.Color{}}
	for i := range model.Objects {
		mp.objects[model.Objects[i].ID] = &model.Objects[i]
	}
	for _, bm := range model.BaseMaterials {
		for _, base := range bm.Bases {
			mp.colors[bm.ID] = append(mp.colors[bm.ID], parseColor(base.Color))
		}
	}
	for _, cg := range model.ColorGroups {
		for _, c := range cg.Colors {
			mp.colors[cg.ID] = append(mp.colors[cg.ID], parseColor(c.Color))
		}
	}
	p.parts[name] = mp
	return decodedModel{items: model.Items}, nil
}

func (p *threeMF) part(name string) (*modelPart, error) {
	if mp, ok := p.parts[name]; ok {
		return mp, nil
	}
	if _, err := p.decode(name); err != nil {
		return nil, err
	}
	return p.parts[name], nil
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

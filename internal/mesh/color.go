package mesh

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"encoding/xml"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/fogleman/fauxgl"
)

// Colors in 3MFs come from three places, in order of precedence:
//
//  1. Multi-material painting: Bambu Studio/Orca's paint_color and
//     PrusaSlicer's mmu_segmentation, per triangle.
//  2. The filament (extruder) a slicer assigns to each part or object.
//     Bambu/Orca keep this in Metadata/model_settings.config and the filament
//     colors in Metadata/project_settings.config; PrusaSlicer uses
//     Metadata/Slic3r_PE_model.config and Metadata/Slic3r_PE.config.
//  3. Standard 3MF materials: <basematerials> and the materials extension's
//     <colorgroup>, referenced by objects and triangles.

// fallbackPalette colors filament slots when a project doesn't list colors.
var fallbackPalette = []string{"#e8a33d", "#4f9ddc", "#e05d5d", "#59b96b", "#b77ad6", "#e0d14f", "#5fc9c4", "#d98c5f"}

// slicerColors is a slicer's filament assignments for one 3MF.
type slicerColors struct {
	filaments []fauxgl.Color
	objects   map[int]int     // object id -> extruder (1-based; 0 = unset)
	parts     map[[2]int]int  // (object id, part id) -> extruder
	subtypes  map[[2]int]bool // (object id, part id) -> printable
	volumes   map[int][]prusaVolume
	// plates holds the objects on each plate of a slicer project, by plate
	// number (which needn't be contiguous). Empty if unknown.
	plates map[int]map[int]bool
}

// prusaVolume is a range of an object's triangles that PrusaSlicer treats as
// one volume (part, modifier, ...).
type prusaVolume struct {
	first, last int
	extruder    int
	printable   bool
}

type xmlKV struct {
	Key   string `xml:"key,attr"`
	Value string `xml:"value,attr"`
}

func extruderOf(md []xmlKV) int {
	for _, kv := range md {
		if kv.Key == "extruder" {
			n, _ := strconv.Atoi(kv.Value)
			return max(n, 0)
		}
	}
	return 0
}

func metadata(md []xmlKV, key string) string {
	for _, kv := range md {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

// readSlicerColors reads Bambu/Orca or PrusaSlicer filament assignments, or
// returns nil if the file has none.
func readSlicerColors(zr *zip.ReadCloser) *slicerColors {
	sc := &slicerColors{
		plates:   map[int]map[int]bool{},
		objects:  map[int]int{},
		parts:    map[[2]int]int{},
		subtypes: map[[2]int]bool{},
		volumes:  map[int][]prusaVolume{},
	}
	found := false

	var bambu struct {
		Objects []struct {
			ID       int     `xml:"id,attr"`
			Metadata []xmlKV `xml:"metadata"`
			Parts    []struct {
				ID       int     `xml:"id,attr"`
				Subtype  string  `xml:"subtype,attr"`
				Metadata []xmlKV `xml:"metadata"`
			} `xml:"part"`
		} `xml:"object"`
		Plates []struct {
			Metadata  []xmlKV `xml:"metadata"`
			Instances []struct {
				Metadata []xmlKV `xml:"metadata"`
			} `xml:"model_instance"`
		} `xml:"plate"`
	}
	if readXML(zr, "Metadata/model_settings.config", &bambu) {
		found = true
		for _, o := range bambu.Objects {
			sc.objects[o.ID] = extruderOf(o.Metadata)
			for _, p := range o.Parts {
				key := [2]int{o.ID, p.ID}
				sc.parts[key] = extruderOf(p.Metadata)
				sc.subtypes[key] = p.Subtype == "" || p.Subtype == "normal_part"
			}
		}
		for _, plate := range bambu.Plates {
			n, err := strconv.Atoi(metadata(plate.Metadata, "plater_id"))
			if err != nil || len(plate.Instances) == 0 {
				continue
			}
			objects := map[int]bool{}
			for _, inst := range plate.Instances {
				if id, err := strconv.Atoi(metadata(inst.Metadata, "object_id")); err == nil {
					objects[id] = true
				}
			}
			sc.plates[n] = objects
		}
		var project struct {
			FilamentColour []string `json:"filament_colour"`
		}
		if data, ok := readFile(zr, "Metadata/project_settings.config"); ok && json.Unmarshal(data, &project) == nil {
			for _, c := range project.FilamentColour {
				sc.filaments = append(sc.filaments, parseColor(c))
			}
		}
	}

	var prusa struct {
		Objects []struct {
			ID       int     `xml:"id,attr"`
			Metadata []xmlKV `xml:"metadata"`
			Volumes  []struct {
				First    int     `xml:"firstid,attr"`
				Last     int     `xml:"lastid,attr"`
				Metadata []xmlKV `xml:"metadata"`
			} `xml:"volume"`
		} `xml:"object"`
	}
	if !found && readXML(zr, "Metadata/Slic3r_PE_model.config", &prusa) {
		found = true
		for _, o := range prusa.Objects {
			sc.objects[o.ID] = extruderOf(o.Metadata)
			for _, v := range o.Volumes {
				t := metadata(v.Metadata, "volume_type")
				sc.volumes[o.ID] = append(sc.volumes[o.ID], prusaVolume{
					first: v.First, last: v.Last,
					extruder:  extruderOf(v.Metadata),
					printable: t == "" || t == "ModelPart",
				})
			}
		}
		sc.filaments = prusaFilaments(zr)
	}
	if !found {
		return nil
	}
	return sc
}

// prusaFilaments reads "; filament_colour = #RRGGBB;#..." from
// Slic3r_PE.config, preferring per-extruder colors where set.
func prusaFilaments(zr *zip.ReadCloser) []fauxgl.Color {
	data, ok := readFile(zr, "Metadata/Slic3r_PE.config")
	if !ok {
		return nil
	}
	lists := map[string][]string{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimPrefix(sc.Text(), "; "), " = ")
		if ok && (key == "filament_colour" || key == "extruder_colour") {
			lists[key] = strings.Split(value, ";")
		}
	}
	var out []fauxgl.Color
	for i, c := range lists["filament_colour"] {
		color := parseColor(c)
		if i < len(lists["extruder_colour"]) {
			if ec := parseColor(lists["extruder_colour"][i]); ec.A > 0 {
				color = ec
			}
		}
		out = append(out, color)
	}
	return out
}

// multiColor reports whether the project assigns two or more different
// filament colors to its objects and parts, on any plate. Then every plate is
// shown in its colors, even one whose parts happen to share a filament.
func (sc *slicerColors) multiColor() bool {
	if sc == nil {
		return false
	}
	seen := map[fauxgl.Color]bool{}
	for _, e := range sc.objects {
		if e > 0 {
			seen[sc.filament(e)] = true
		}
	}
	for key, e := range sc.parts {
		if e > 0 && sc.subtypes[key] {
			seen[sc.filament(e)] = true
		}
	}
	for _, vs := range sc.volumes {
		for _, v := range vs {
			if v.extruder > 0 && v.printable {
				seen[sc.filament(v.extruder)] = true
			}
		}
	}
	return len(seen) > 1
}

// plateNumbers lists the project's plates in order.
func (sc *slicerColors) plateNumbers() []int {
	if sc == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(sc.plates))
}

// DisplayColor adjusts a model color for display on STLton's dark
// background: dark filaments are lightened so the darkest a color gets is a
// mid-dark grey. Thumbnails and the 3D viewer both use it.
func DisplayColor(c fauxgl.Color) fauxgl.Color {
	const floor = 0.28
	return fauxgl.Color{R: floor + c.R*(1-floor), G: floor + c.G*(1-floor), B: floor + c.B*(1-floor), A: 1}
}

// printable reports whether a component is part of the printed shape (not
// a modifier, negative volume, or support blocker/enforcer).
func (sc *slicerColors) printable(slot slotRef) bool {
	if sc == nil || !slot.hasPart {
		return true
	}
	ok, known := sc.subtypes[[2]int{slot.object, slot.part}]
	return !known || ok
}

// trianglePrintable is printable for PrusaSlicer, whose volumes are ranges
// of one mesh's triangles rather than components.
func (sc *slicerColors) trianglePrintable(slot slotRef, i int) bool {
	if sc == nil || slot.hasPart {
		return true
	}
	if v, ok := sc.volume(slot.object, i); ok {
		return v.printable
	}
	return true
}

func (sc *slicerColors) volume(object, i int) (prusaVolume, bool) {
	for _, v := range sc.volumes[object] {
		if i >= v.first && i <= v.last {
			return v, true
		}
	}
	return prusaVolume{}, false
}

// extruder returns the extruder assigned to a triangle's part or object,
// or 0 if none.
func (sc *slicerColors) extruder(slot slotRef, i int) int {
	if slot.hasPart {
		if e := sc.parts[[2]int{slot.object, slot.part}]; e > 0 {
			return e
		}
	} else if v, ok := sc.volume(slot.object, i); ok && v.extruder > 0 {
		return v.extruder
	}
	return sc.objects[slot.object]
}

// filament is extruder n's color (1-based).
func (sc *slicerColors) filament(n int) fauxgl.Color {
	if n >= 1 && n <= len(sc.filaments) && sc.filaments[n-1].A > 0 {
		return sc.filaments[n-1]
	}
	return parseColor(fallbackPalette[(n-1+len(fallbackPalette))%len(fallbackPalette)])
}

// triangleColors returns a triangle's vertex colors, or false if nothing
// assigns it one.
func (p *threeMF) triangleColors(mp *modelPart, obj *xmlObject, t xmlTriangle, i int, slot slotRef) ([3]fauxgl.Color, bool) {
	same := func(c fauxgl.Color) ([3]fauxgl.Color, bool) { return [3]fauxgl.Color{c, c, c}, true }
	if sc := p.slots; sc != nil {
		code := t.PaintColor
		if code == "" {
			code = t.MMU
		}
		if state := paintState(code); state > 0 {
			return same(sc.filament(state))
		}
		if e := sc.extruder(slot, i); e > 0 {
			return same(sc.filament(e))
		}
	}

	pid, p1 := t.PID, t.P1
	if pid == "" {
		pid, p1 = obj.PID, obj.PIndex
	}
	if pid == "" {
		return [3]fauxgl.Color{}, false
	}
	if t.P1 == "" {
		p1 = obj.PIndex
	}
	id, err := strconv.Atoi(pid)
	colors := mp.colors[id]
	if err != nil || len(colors) == 0 {
		return [3]fauxgl.Color{}, false
	}
	at := func(s, fallback string) (fauxgl.Color, bool) {
		if s == "" {
			s = fallback
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n >= len(colors) || colors[n].A == 0 {
			return fauxgl.Color{}, false
		}
		return colors[n], true
	}
	c1, ok1 := at(p1, "0")
	c2, ok2 := at(t.P2, p1)
	c3, ok3 := at(t.P3, p1)
	if !ok1 || !ok2 || !ok3 {
		return [3]fauxgl.Color{}, false
	}
	return [3]fauxgl.Color{c1, c2, c3}, true
}

// paintState decodes a TriangleSelector paint code (Bambu's paint_color,
// PrusaSlicer's mmu_segmentation) and returns the filament covering most of
// the triangle, or 0 if it's unpainted or the code is malformed.
//
// The code is a hex string read from its last digit backwards, one 4-bit
// node per digit, depth-first. The low two bits are how many sides the
// triangle is split along; 0 means a leaf, whose high two bits are its state
// (3 meaning "3 plus the next digit"). A split node has (sides + 1) children.
// Areas are approximated by giving each child an equal share, which is close
// enough for a thumbnail.
func paintState(code string) int {
	if code == "" {
		return 0
	}
	pos := len(code) - 1
	next := func() (int, bool) {
		if pos < 0 {
			return 0, false
		}
		n, err := strconv.ParseUint(code[pos:pos+1], 16, 8)
		pos--
		return int(n), err == nil
	}
	weights := map[int]float64{}
	var node func(share float64, depth int) bool
	node = func(share float64, depth int) bool {
		if depth > 32 {
			return false
		}
		n, ok := next()
		if !ok {
			return false
		}
		if split := n & 0b11; split > 0 {
			children := split + 1
			for range children {
				if !node(share/float64(children), depth+1) {
					return false
				}
			}
			return true
		}
		state := n >> 2
		if state == 3 {
			extra, ok := next()
			if !ok {
				return false
			}
			state += extra
		}
		weights[state] += share
		return true
	}
	if !node(1, 0) {
		return 0
	}
	best, bestWeight := 0, -1.0
	for state, w := range weights {
		if w > bestWeight || (w == bestWeight && state < best) {
			best, bestWeight = state, w
		}
	}
	return best
}

// meshBuilder collects triangles and their colors.
type meshBuilder struct {
	tris    []*fauxgl.Triangle
	colors  [][3]fauxgl.Color
	colored []bool
}

func (b *meshBuilder) add(t *fauxgl.Triangle, c [3]fauxgl.Color, ok bool) {
	b.tris = append(b.tris, t)
	b.colors = append(b.colors, c)
	b.colored = append(b.colored, ok)
}

// mesh builds the mesh, coloring its vertices only if at least two distinct
// colors are used (or force is set, for one plate of a multi-color project):
// a single-color file is drawn like any other model. Uncolored triangles in
// a colored mesh get DefaultColor.
func (b *meshBuilder) mesh(force bool) (*fauxgl.Mesh, bool) {
	distinct := map[fauxgl.Color]bool{}
	for i, ok := range b.colored {
		if ok {
			for _, c := range b.colors[i] {
				distinct[c] = true
			}
		}
		if len(distinct) > 1 {
			break
		}
	}
	m := fauxgl.NewTriangleMesh(b.tris)
	if len(distinct) < 2 && !(force && len(distinct) > 0) {
		return m, false
	}
	for i, t := range b.tris {
		c := [3]fauxgl.Color{DefaultColor, DefaultColor, DefaultColor}
		if b.colored[i] {
			c = b.colors[i]
		}
		t.V1.Color, t.V2.Color, t.V3.Color = c[0], c[1], c[2]
	}
	return m, true
}

// parseColor parses "#RRGGBB" or "#RRGGBBAA". Unparseable colors have A == 0.
func parseColor(s string) fauxgl.Color {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 && len(s) != 8 {
		return fauxgl.Color{}
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return fauxgl.Color{}
	}
	if len(s) == 6 {
		v = v<<8 | 0xff
	}
	return fauxgl.Color{
		R: float64(v>>24&0xff) / 255,
		G: float64(v>>16&0xff) / 255,
		B: float64(v>>8&0xff) / 255,
		A: 1, // slicer colors are opaque; ignore alpha
	}
}

func readFile(zr *zip.ReadCloser, name string) ([]byte, bool) {
	f, err := zr.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16<<20))
	return data, err == nil
}

func readXML(zr *zip.ReadCloser, name string, v any) bool {
	data, ok := readFile(zr, name)
	return ok && xml.Unmarshal(data, v) == nil
}

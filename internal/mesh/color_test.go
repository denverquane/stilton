package mesh

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fogleman/fauxgl"
)

func TestPaintState(t *testing.T) {
	for code, want := range map[string]int{
		"":   0,
		"4":  1,
		"8":  2,
		"0C": 3, // state 3+: "C" then the next digit adds to 3
		"1C": 4,
		"F2": 0, // truncated: a split with no children
		"Z":  0, // not hex
		// A real code from a painted Bambu file; mostly state 2.
		"80882882882886808828828868088288680886800AAAA2": 2,
		// Split into 2 (low bits 01): children "8" (state 2) and "4" (state 1).
		"481": 1, // equal shares; ties go to the lower state
		// Pathologically deep nesting must not overflow.
		strings.Repeat("1", 200): 0,
	} {
		if got := paintState(code); got != want {
			t.Errorf("paintState(%q) = %d, want %d", code, got, want)
		}
	}
}

func TestParseColor(t *testing.T) {
	if c := parseColor("#FF8000"); c != (fauxgl.Color{R: 1, G: 128.0 / 255, B: 0, A: 1}) {
		t.Errorf("#FF8000 = %v", c)
	}
	if c := parseColor("#00FF00FF"); c.G != 1 || c.A != 1 {
		t.Errorf("#00FF00FF = %v", c)
	}
	for _, bad := range []string{"", "#FFF", "red", "#GGGGGG"} {
		if parseColor(bad).A != 0 {
			t.Errorf("parseColor(%q) should be invalid", bad)
		}
	}
}

const coreModelHead = `<?xml version="1.0" encoding="UTF-8"?>
<model unit="millimeter" xmlns="http://schemas.microsoft.com/3dmanufacturing/core/2015/02"
       xmlns:m="http://schemas.microsoft.com/3dmanufacturing/material/2015/02"
       xmlns:p="http://schemas.microsoft.com/3dmanufacturing/production/2015/06">`

const triMesh = `<mesh><vertices><vertex x="0" y="0" z="0"/><vertex x="1" y="0" z="0"/><vertex x="0" y="1" z="0"/><vertex x="0" y="0" z="1"/></vertices>
<triangles>%s</triangles></mesh>`

func colorsOf(m *fauxgl.Mesh) []fauxgl.Color {
	var out []fauxgl.Color
	for _, t := range m.Triangles {
		out = append(out, t.V1.Color)
	}
	return out
}

func TestLoad3MFMaterials(t *testing.T) {
	path := write3MF(t, map[string]string{
		"_rels/.rels": testRels,
		"3D/3dmodel.model": coreModelHead + `<resources>
  <basematerials id="5"><base name="red" displaycolor="#FF0000"/><base name="blue" displaycolor="#0000FF"/></basematerials>
  <m:colorgroup id="6"><m:color color="#00FF00FF"/></m:colorgroup>
  <object id="1" pid="5" pindex="1">` + strings.ReplaceAll(triMesh, "%s",
			`<triangle v1="0" v2="1" v3="2" pid="5" p1="0"/><triangle v1="0" v2="1" v3="3"/><triangle v1="0" v2="2" v3="3" pid="6" p1="0"/>`) + `</object>
</resources><build><item objectid="1"/></build></model>`,
	})
	l, err := Load3MF(path, PlateFirst)
	if err != nil || !l.Colored {
		t.Fatalf("colored=%v err=%v", l.Colored, err)
	}
	got := colorsOf(l.Mesh)
	want := []fauxgl.Color{parseColor("#FF0000"), parseColor("#0000FF"), parseColor("#00FF00")}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("triangle %d color %v, want %v", i, got[i], want[i])
		}
	}
}

func TestLoad3MFSingleColorIsPlain(t *testing.T) {
	path := write3MF(t, map[string]string{
		"_rels/.rels": testRels,
		"3D/3dmodel.model": coreModelHead + `<resources>
  <basematerials id="5"><base name="grey" displaycolor="#808080"/></basematerials>
  <object id="1" pid="5" pindex="0">` + strings.ReplaceAll(triMesh, "%s", `<triangle v1="0" v2="1" v3="2"/>`) + `</object>
</resources><build><item objectid="1"/></build></model>`,
	})
	if l, err := Load3MF(path, PlateFirst); err != nil || l.Colored {
		t.Errorf("one color should render plain: colored=%v err=%v", l.Colored, err)
	}
}

// A Bambu-style project: two plates; the first holds object 10, whose parts
// are a normal part on filament 2, a modifier (not drawn), and a part left on
// the object's filament 1 with one triangle painted filament 3.
func TestLoad3MFBambu(t *testing.T) {
	part := func(tris string) string {
		return `<object id="%d" type="model">` + strings.ReplaceAll(triMesh, "%s", tris) + `</object>`
	}
	objects := coreModelHead + `<resources>` +
		strings.Replace(part(`<triangle v1="0" v2="1" v3="2"/>`), "%d", "1", 1) +
		strings.Replace(part(`<triangle v1="0" v2="1" v3="3"/>`), "%d", "2", 1) +
		strings.Replace(part(`<triangle v1="0" v2="2" v3="3"/><triangle v1="1" v2="2" v3="3" paint_color="0C"/>`), "%d", "3", 1) +
		strings.Replace(part(`<triangle v1="0" v2="1" v3="2"/>`), "%d", "4", 1) +
		`<object id="10" type="model"><components>
  <component objectid="1"/><component objectid="2"/><component objectid="3"/>
</components></object>
<object id="20" type="model"><components><component objectid="4"/></components></object>
</resources></model>`
	path := write3MF(t, map[string]string{
		"_rels/.rels": testRels,
		"3D/3dmodel.model": coreModelHead + `<resources/><build>
  <item objectid="10" p:path="/3D/Objects/a.model"/>
  <item objectid="20" p:path="/3D/Objects/a.model" transform="1 0 0 0 1 0 0 0 1 500 0 0"/>
</build></model>`,
		"3D/Objects/a.model": objects,
		"Metadata/model_settings.config": `<?xml version="1.0"?><config>
  <object id="10"><metadata key="extruder" value="1"/>
    <part id="1" subtype="normal_part"><metadata key="extruder" value="2"/></part>
    <part id="2" subtype="modifier_part"><metadata key="extruder" value="3"/></part>
    <part id="3" subtype="normal_part"><metadata key="extruder" value="0"/></part>
  </object>
  <object id="20"><metadata key="extruder" value="2"/></object>
  <plate><metadata key="plater_id" value="2"/><model_instance><metadata key="object_id" value="20"/></model_instance></plate>
  <plate><metadata key="plater_id" value="1"/><model_instance><metadata key="object_id" value="10"/></model_instance></plate>
</config>`,
		"Metadata/project_settings.config": `{"filament_colour": ["#FFFFFF", "#000000", "#FF0000"]}`,
	})

	l, err := Load3MF(path, PlateFirst)
	if err != nil || !l.Colored {
		t.Fatalf("colored=%v err=%v", l.Colored, err)
	}
	if !slices.Equal(l.Plates, []int{1, 2}) || l.Plate != 1 {
		t.Errorf("plates %v, loaded %d; want [1 2], 1", l.Plates, l.Plate)
	}
	got := colorsOf(l.Mesh)
	want := []fauxgl.Color{parseColor("#000000"), parseColor("#FFFFFF"), parseColor("#FF0000")}
	if len(got) != len(want) {
		t.Fatalf("got %d triangles, want %d (modifier and plate 2 skipped)", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("triangle %d color %v, want %v", i, got[i], want[i])
		}
	}

	// Other plates, and all of them.
	if p2, err := Load3MF(path, 2); err != nil || len(p2.Mesh.Triangles) != 1 || p2.Plate != 2 {
		t.Errorf("plate 2: %+v (%v), want 1 triangle", p2, err)
	}
	if all, err := Load3MF(path, AllPlates); err != nil || len(all.Mesh.Triangles) != 4 {
		t.Errorf("all plates: %+v (%v), want 4 triangles", all, err)
	}
	// Plate 2's object is all filament 2, but the project uses several
	// colors, so it's shown in its color rather than the default.
	if p2, _ := Load3MF(path, 2); !p2.Colored || colorsOf(p2.Mesh)[0] != parseColor("#000000") {
		t.Errorf("plate 2 colored=%v colors %v, want black", p2.Colored, colorsOf(p2.Mesh))
	}
	if _, err := Load3MF(path, 9); !errors.Is(err, ErrNoPlate) {
		t.Errorf("plate 9: err = %v, want ErrNoPlate", err)
	}
}

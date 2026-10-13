package mesh

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"

	"github.com/fogleman/fauxgl"
)

// LoadSTL reads a binary or ASCII STL. fauxgl only treats a file as binary
// when its size exactly matches the header's triangle count, but some
// exporters (e.g. Meshmixer) pad the end, so trailing bytes are tolerated here.
func LoadSTL(path string) (*fauxgl.Mesh, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !isBinarySTL(data) {
		return fauxgl.LoadSTL(path)
	}
	count := int(binary.LittleEndian.Uint32(data[80:84]))
	tris := make([]fauxgl.Triangle, count)
	ptrs := make([]*fauxgl.Triangle, count)
	vec := func(b []byte) fauxgl.Vector {
		return fauxgl.V(
			float64(math.Float32frombits(binary.LittleEndian.Uint32(b[0:]))),
			float64(math.Float32frombits(binary.LittleEndian.Uint32(b[4:]))),
			float64(math.Float32frombits(binary.LittleEndian.Uint32(b[8:]))),
		)
	}
	for i := range count {
		rec := data[84+i*50:]
		t := &tris[i]
		t.V1.Position, t.V2.Position, t.V3.Position = vec(rec[12:]), vec(rec[24:]), vec(rec[36:])
		t.FixNormals()
		ptrs[i] = t
	}
	return fauxgl.NewTriangleMesh(ptrs), nil
}

// isBinarySTL reports whether data holds a complete binary STL. ASCII files
// start with "solid", but so do some binary headers, so those also need the
// ASCII "facet" keyword before being treated as text.
func isBinarySTL(data []byte) bool {
	if len(data) < 84 {
		return false
	}
	count := int64(binary.LittleEndian.Uint32(data[80:84]))
	if 84+50*count > int64(len(data)) {
		return false
	}
	if bytes.HasPrefix(data, []byte("solid")) {
		head := data[:min(len(data), 1024)]
		return !bytes.Contains(head, []byte("facet"))
	}
	return true
}

// WriteBinarySTL encodes a mesh as binary STL. The viewer uses this for 3MFs
// that three.js can't assemble itself (e.g. geometry split across p:path parts).
func WriteBinarySTL(w io.Writer, m *fauxgl.Mesh) error {
	bw := bufio.NewWriter(w)
	var header [80]byte
	copy(header[:], "stilton")
	bw.Write(header[:])
	binary.Write(bw, binary.LittleEndian, uint32(len(m.Triangles)))

	var rec [50]byte
	put := func(off int, v fauxgl.Vector) {
		binary.LittleEndian.PutUint32(rec[off:], math.Float32bits(float32(v.X)))
		binary.LittleEndian.PutUint32(rec[off+4:], math.Float32bits(float32(v.Y)))
		binary.LittleEndian.PutUint32(rec[off+8:], math.Float32bits(float32(v.Z)))
	}
	for _, t := range m.Triangles {
		put(0, t.Normal())
		put(12, t.V1.Position)
		put(24, t.V2.Position)
		put(36, t.V3.Position)
		if _, err := bw.Write(rec[:]); err != nil {
			return err
		}
	}
	return bw.Flush()
}

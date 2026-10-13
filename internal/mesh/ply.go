package mesh

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/fogleman/fauxgl"
)

// PLYVersion identifies the PLY output. It's part of HTTP cache keys, so bump
// it whenever WriteBinaryPLY's output changes.
const PLYVersion = 2

// WriteBinaryPLY encodes a mesh as indexed binary PLY for the web viewer.
// Vertices shared by triangles are merged (which roughly halves the size),
// but only where the color matches too, so color boundaries stay sharp. A
// colored mesh gets per-vertex colors, adjusted with DisplayColor like
// thumbnails; otherwise none are written and the viewer uses its default.
func WriteBinaryPLY(w io.Writer, m *fauxgl.Mesh, colored bool) error {
	type vertex struct {
		x, y, z uint32 // float32 bits
		r, g, b uint8
	}
	byte8 := func(f float64) uint8 { return uint8(math.Round(math.Max(0, math.Min(1, f)) * 255)) }
	index := map[vertex]uint32{}
	var vertices []vertex
	faces := make([][3]uint32, len(m.Triangles))
	for i, t := range m.Triangles {
		for j, v := range [3]fauxgl.Vertex{t.V1, t.V2, t.V3} {
			key := vertex{
				x: math.Float32bits(float32(v.Position.X)),
				y: math.Float32bits(float32(v.Position.Y)),
				z: math.Float32bits(float32(v.Position.Z)),
			}
			if colored {
				c := DisplayColor(v.Color)
				key.r, key.g, key.b = byte8(c.R), byte8(c.G), byte8(c.B)
			}
			n, ok := index[key]
			if !ok {
				n = uint32(len(vertices))
				index[key] = n
				vertices = append(vertices, key)
			}
			faces[i][j] = n
		}
	}

	bw := bufio.NewWriterSize(w, 1<<16)
	fmt.Fprintf(bw, "ply\nformat binary_little_endian 1.0\ncomment STLton\nelement vertex %d\n", len(vertices))
	bw.WriteString("property float x\nproperty float y\nproperty float z\n")
	if colored {
		bw.WriteString("property uchar red\nproperty uchar green\nproperty uchar blue\n")
	}
	fmt.Fprintf(bw, "element face %d\nproperty list uchar uint vertex_indices\nend_header\n", len(faces))

	var rec [15]byte
	for _, v := range vertices {
		binary.LittleEndian.PutUint32(rec[0:], v.x)
		binary.LittleEndian.PutUint32(rec[4:], v.y)
		binary.LittleEndian.PutUint32(rec[8:], v.z)
		n := 12
		if colored {
			rec[12], rec[13], rec[14] = v.r, v.g, v.b
			n = 15
		}
		if _, err := bw.Write(rec[:n]); err != nil {
			return err
		}
	}
	var face [13]byte
	face[0] = 3
	for _, f := range faces {
		binary.LittleEndian.PutUint32(face[1:], f[0])
		binary.LittleEndian.PutUint32(face[5:], f[1])
		binary.LittleEndian.PutUint32(face[9:], f[2])
		if _, err := bw.Write(face[:]); err != nil {
			return err
		}
	}
	return bw.Flush()
}

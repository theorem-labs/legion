package declindex

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
)

// graph is the declaration graph in compressed sparse rows, written beside the index after each refresh so
// that a reach query walks memory instead of the edges table: a declaration that 80,000 statements depend on
// is answered in milliseconds.
//
// File layout, little endian: magic, then the node names and the module names as string tables (count, offsets,
// bytes), then the CSR offsets (node count + 1) and the edges, each a target node and a module index whose top bit
// says that the edge propagates a change (a statement use, or a use by a definition).
type graph struct {
	names   []string // sorted: a node's id is its index
	modules []string
	offs    []uint32
	dst     []uint32
	mod     []uint32
}

const graphMagic = "declindex-graph-1\n"

const propBit = uint32(1) << 31

func (g *graph) id(name string) (uint32, bool) {
	i := sort.SearchStrings(g.names, name)
	if i < len(g.names) && g.names[i] == name {
		return uint32(i), true
	}
	return 0, false
}

// buildGraph reads the edges table into a graph.
func buildGraph(ctx context.Context, q querier) (*graph, error) {
	rows, err := q.QueryContext(ctx, `SELECT src, dst, module, `+propagates+` FROM edges e`)
	if err != nil {
		return nil, err
	}
	type edge struct {
		src, dst, mod string
		prop          bool
	}
	var edges []edge
	nameSet, modSet := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var e edge
		if err := rows.Scan(&e.src, &e.dst, &e.mod, &e.prop); err != nil {
			rows.Close()
			return nil, err
		}
		nameSet[e.src], nameSet[e.dst], modSet[e.mod] = true, true, true
		edges = append(edges, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	g := &graph{names: keys(nameSet), modules: keys(modSet)}
	modID := make(map[string]uint32, len(g.modules))
	for i, m := range g.modules {
		modID[m] = uint32(i)
	}
	nodeID := make(map[string]uint32, len(g.names))
	for i, n := range g.names {
		nodeID[n] = uint32(i)
	}
	type packed struct{ src, dst, mod uint32 }
	ps := make([]packed, len(edges))
	for i, e := range edges {
		m := modID[e.mod]
		if e.prop {
			m |= propBit
		}
		ps[i] = packed{nodeID[e.src], nodeID[e.dst], m}
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].src != ps[j].src {
			return ps[i].src < ps[j].src
		}
		if ps[i].dst != ps[j].dst {
			return ps[i].dst < ps[j].dst
		}
		return ps[i].mod < ps[j].mod
	})
	g.offs = make([]uint32, len(g.names)+1)
	g.dst = make([]uint32, len(ps))
	g.mod = make([]uint32, len(ps))
	for i, p := range ps {
		g.offs[p.src+1]++
		g.dst[i], g.mod[i] = p.dst, p.mod
	}
	for i := 1; i < len(g.offs); i++ {
		g.offs[i] += g.offs[i-1]
	}
	return g, nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func putStrings(b *bytes.Buffer, ss []string) {
	_ = binary.Write(b, binary.LittleEndian, uint32(len(ss)))
	off := uint32(0)
	for _, s := range ss {
		_ = binary.Write(b, binary.LittleEndian, off)
		off += uint32(len(s))
	}
	_ = binary.Write(b, binary.LittleEndian, off)
	for _, s := range ss {
		b.WriteString(s)
	}
}

// write stores the graph at path atomically.
func (g *graph) write(path string) error {
	var b bytes.Buffer
	b.WriteString(graphMagic)
	putStrings(&b, g.names)
	putStrings(&b, g.modules)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(g.dst)))
	_ = binary.Write(&b, binary.LittleEndian, g.offs)
	_ = binary.Write(&b, binary.LittleEndian, g.dst)
	_ = binary.Write(&b, binary.LittleEndian, g.mod)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) u32() uint32 {
	if r.err != nil || len(r.b) < 4 {
		r.err = errors.New("truncated graph file")
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) u32s(n int) []uint32 {
	if r.err != nil || len(r.b) < 4*n {
		r.err = errors.New("truncated graph file")
		return nil
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(r.b[4*i:])
	}
	r.b = r.b[4*n:]
	return out
}

func (r *reader) strings() []string {
	n := int(r.u32())
	offs := r.u32s(n + 1)
	if r.err != nil {
		return nil
	}
	total := int(offs[n])
	if len(r.b) < total {
		r.err = errors.New("truncated graph file")
		return nil
	}
	blob := string(r.b[:total])
	r.b = r.b[total:]
	out := make([]string, n)
	for i := range out {
		out[i] = blob[offs[i]:offs[i+1]]
	}
	return out
}

func loadGraph(path string) (*graph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(data, []byte(graphMagic)) {
		return nil, fmt.Errorf("%s is not a declaration graph of this version", path)
	}
	r := &reader{b: data[len(graphMagic):]}
	g := &graph{names: r.strings(), modules: r.strings()}
	e := int(r.u32())
	g.offs = r.u32s(len(g.names) + 1)
	g.dst = r.u32s(e)
	g.mod = r.u32s(e)
	if r.err != nil {
		return nil, fmt.Errorf("%s: %w", path, r.err)
	}
	return g, nil
}

// reachedDecl is one declaration a change reaches: a name and the module of the use that reached it.
type reachedDecl struct{ name, module string }

// walk computes the statements that a change to root changes (followed through propagating edges) and the proofs
// at their edge, and for each, the declaration it was first reached from.
func (g *graph) walk(root string) (changed, reprove map[reachedDecl]bool, via map[string]string) {
	changed, reprove, via = map[reachedDecl]bool{}, map[reachedDecl]bool{}, map[string]string{}
	r, ok := g.id(root)
	if !ok {
		return
	}
	inClosure := map[uint32]bool{r: true}
	frontier := []uint32{r}
	var nonProp [][2]uint32 // (src, edge index) of edges that do not propagate, checked after the closure
	for len(frontier) > 0 {
		var next []uint32
		for _, s := range frontier {
			for i := g.offs[s]; i < g.offs[s+1]; i++ {
				d := g.dst[i]
				if g.mod[i]&propBit == 0 {
					nonProp = append(nonProp, [2]uint32{s, i})
					continue
				}
				if d != r {
					changed[reachedDecl{g.names[d], g.modules[g.mod[i]&^propBit]}] = true
				}
				if !inClosure[d] {
					inClosure[d] = true
					via[g.names[d]] = g.names[s]
					next = append(next, d)
				}
			}
		}
		frontier = next
	}
	for _, e := range nonProp {
		d := g.dst[e[1]]
		if inClosure[d] {
			continue
		}
		reprove[reachedDecl{g.names[d], g.modules[g.mod[e[1]]&^propBit]}] = true
		if _, ok := via[g.names[d]]; !ok {
			via[g.names[d]] = g.names[e[0]]
		}
	}
	return changed, reprove, via
}

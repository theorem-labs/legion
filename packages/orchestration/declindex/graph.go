package declindex

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
)

// graph is the declaration graph in compressed sparse rows, written beside the index after each refresh so
// that a reach query walks memory instead of the edges table: a declaration that 80,000 statements depend on
// is answered in milliseconds. A node is a declaration, its module and its name (declRef).
//
// File layout, little endian: magic, then the node names and the module names as string tables (count, offsets,
// bytes), each node's module index, then the CSR offsets (node count + 1) and the edges, each a target node whose
// top bit says that the edge propagates a change (a statement use, or a use by a definition).
type graph struct {
	// token is the index generation the graph was built from (meta "graph"); a query refuses another one.
	token   string
	names   []string // the nodes, sorted by name and then module: a node's id is its index
	modules []string
	nodeMod []uint32 // each node's index in modules
	offs    []uint32
	dst     []uint32
}

const graphMagic = "declindex-graph-3\n"

const propBit = uint32(1) << 31

func (g *graph) node(i uint32) declRef { return declRef{g.modules[g.nodeMod[i]], g.names[i]} }

func (g *graph) id(d declRef) (uint32, bool) {
	i, ok := slices.BinarySearchFunc(g.names, d, func(name string, d declRef) int { return cmp.Compare(name, d.name) })
	for ; ok && i < len(g.names) && g.names[i] == d.name; i++ {
		if g.modules[g.nodeMod[i]] == d.module {
			return uint32(i), true
		}
	}
	return 0, false
}

// buildGraph reads the edges table into a graph. Each edge's target is counted as the declaration it belongs to
// (ownerResolver.owner), against the declarations and imports of every module now, so an incremental refresh gives
// the graph a full one gives.
func buildGraph(ctx context.Context, q querier) (*graph, error) {
	declaring, err := allDeclaring(ctx, q)
	if err != nil {
		return nil, err
	}
	owners := newOwnerResolver(ctx, q, declaring)
	type edge struct {
		src, dst declRef
		prop     bool
	}
	var edges []edge
	nodeSet := map[declRef]bool{}
	rows, err := q.QueryContext(ctx, `SELECT target, tmodule, dst, module, `+propagates+` FROM edges e ORDER BY tmodule`)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var e edge
		var target declRef
		if err := r.Scan(&target.name, &target.module, &e.dst.name, &e.dst.module, &e.prop); err != nil {
			return err
		}
		src, err := owners.owner(target)
		if err != nil {
			return err
		}
		if e.src = src; e.src == e.dst {
			return nil
		}
		nodeSet[e.src], nodeSet[e.dst] = true, true
		edges = append(edges, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	nodes := slices.SortedFunc(maps.Keys(nodeSet), declRef.compare)
	modSet := map[string]bool{}
	for _, n := range nodes {
		modSet[n.module] = true
	}
	g := &graph{names: make([]string, len(nodes)), modules: slices.Sorted(maps.Keys(modSet)), nodeMod: make([]uint32, len(nodes))}
	modID := make(map[string]uint32, len(g.modules))
	for i, m := range g.modules {
		modID[m] = uint32(i)
	}
	nodeID := make(map[declRef]uint32, len(nodes))
	for i, n := range nodes {
		g.names[i], g.nodeMod[i] = n.name, modID[n.module]
		nodeID[n] = uint32(i)
	}
	type packed struct {
		src, dst uint32
		prop     bool
	}
	ps := make([]packed, len(edges))
	for i, e := range edges {
		ps[i] = packed{nodeID[e.src], nodeID[e.dst], e.prop}
	}
	slices.SortFunc(ps, func(a, b packed) int { return cmp.Or(cmp.Compare(a.src, b.src), cmp.Compare(a.dst, b.dst)) })
	// Two targets with one owner give one edge, which propagates when either use does.
	merged := ps[:0]
	for _, p := range ps {
		if n := len(merged); n > 0 && merged[n-1].src == p.src && merged[n-1].dst == p.dst {
			merged[n-1].prop = merged[n-1].prop || p.prop
			continue
		}
		merged = append(merged, p)
	}
	g.offs = make([]uint32, len(g.names)+1)
	g.dst = make([]uint32, len(merged))
	for i, p := range merged {
		g.offs[p.src+1]++
		g.dst[i] = p.dst
		if p.prop {
			g.dst[i] |= propBit
		}
	}
	for i := 1; i < len(g.offs); i++ {
		g.offs[i] += g.offs[i-1]
	}
	return g, nil
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

// write stores the graph at path atomically, with the generation token of the index it was built from.
func (g *graph) write(path string) error {
	var b bytes.Buffer
	b.WriteString(graphMagic)
	putStrings(&b, []string{g.token})
	putStrings(&b, g.names)
	putStrings(&b, g.modules)
	_ = binary.Write(&b, binary.LittleEndian, g.nodeMod)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(g.dst)))
	_ = binary.Write(&b, binary.LittleEndian, g.offs)
	_ = binary.Write(&b, binary.LittleEndian, g.dst)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// graphToken reads only the generation token of the graph file at path; empty when there is none.
func graphToken(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, _ := io.ReadFull(f, head)
	if !bytes.HasPrefix(head[:n], []byte(graphMagic)) {
		return ""
	}
	r := &reader{b: head[len(graphMagic):n]}
	t := r.strings()
	if r.err != nil || len(t) != 1 {
		return ""
	}
	return t[0]
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

// parseGraph reads the bytes of the graph file at path, which errors name.
func parseGraph(path string, data []byte) (*graph, error) {
	if !bytes.HasPrefix(data, []byte(graphMagic)) {
		return nil, fmt.Errorf("%s is not a declaration graph of this version", path)
	}
	r := &reader{b: data[len(graphMagic):]}
	tok := r.strings()
	if r.err == nil && len(tok) != 1 {
		return nil, fmt.Errorf("%s has no generation token", path)
	}
	g := &graph{names: r.strings(), modules: r.strings()}
	if len(tok) == 1 {
		g.token = tok[0]
	}
	g.nodeMod = r.u32s(len(g.names))
	e := int(r.u32())
	g.offs = r.u32s(len(g.names) + 1)
	g.dst = r.u32s(e)
	if r.err != nil {
		return nil, fmt.Errorf("%s: %w", path, r.err)
	}
	return g, nil
}

// walk computes the statements that a change to root changes (followed through propagating edges) and the proofs
// at their edge, and for each, the declaration it was first reached from.
func (g *graph) walk(root declRef) (changed, reprove map[declRef]bool, via map[declRef]declRef) {
	changed, reprove, via = map[declRef]bool{}, map[declRef]bool{}, map[declRef]declRef{}
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
				if g.dst[i]&propBit == 0 {
					nonProp = append(nonProp, [2]uint32{s, i})
					continue
				}
				d := g.dst[i] &^ propBit
				if d != r {
					changed[g.node(d)] = true
				}
				if !inClosure[d] {
					inClosure[d] = true
					via[g.node(d)] = g.node(s)
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
		reprove[g.node(d)] = true
		if _, ok := via[g.node(d)]; !ok {
			via[g.node(d)] = g.node(e[0])
		}
	}
	return changed, reprove, via
}

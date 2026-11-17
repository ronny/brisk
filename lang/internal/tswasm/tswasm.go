// Package tswasm runs tree-sitter compiled to WebAssembly on wazero, so brisk
// parses Go and TypeScript without cgo. treesitter.wasm is built by build.sh
// from pinned upstream sources and embedded in the binary.
//
// One call parses a file and copies the whole syntax tree into Go as a flat
// node slice; nothing crosses the boundary per node.
package tswasm

import (
	"context"
	_ "embed"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed treesitter.wasm
var wasm []byte

// Lang selects a grammar. The values match the ids in shim.c.
type Lang uint32

const (
	Go Lang = iota
	TypeScript
	TSX
	nlang
)

const (
	flagNamed = 1 << iota
	flagExtra
	flagError
	flagMissing
	flagHasError
)

// Node is one syntax node. Rows are 0-based.
type Node struct {
	Symbol    uint16
	Field     uint8
	flags     uint8
	StartByte uint32
	EndByte   uint32
	StartRow  uint32
	EndRow    uint32
	Parent    int32 // index into Tree.Nodes, -1 for the root
}

func (n *Node) Named() bool    { return n.flags&flagNamed != 0 }
func (n *Node) Extra() bool    { return n.flags&flagExtra != 0 }
func (n *Node) IsError() bool  { return n.flags&flagError != 0 }
func (n *Node) Missing() bool  { return n.flags&flagMissing != 0 }
func (n *Node) HasError() bool { return n.flags&flagHasError != 0 }

// Tree is a parsed file. Nodes are in preorder; Nodes[0] is the root.
type Tree struct {
	Lang     Lang
	Source   []byte
	Nodes    []Node
	Children [][]int32 // child indices per node, in source order
	names    *names
}

// Kind returns the node's grammar symbol name, such as "function_declaration".
func (t *Tree) Kind(i int32) string { return t.names.symbols[t.Nodes[i].Symbol] }

// FieldName returns the node's field name in its parent, or "".
func (t *Tree) FieldName(i int32) string { return t.names.fields[t.Nodes[i].Field] }

// Text returns the node's source text.
func (t *Tree) Text(i int32) string {
	n := &t.Nodes[i]
	return string(t.Source[n.StartByte:n.EndByte])
}

// ChildByField returns the first child of i in field name, or -1.
func (t *Tree) ChildByField(i int32, name string) int32 {
	for _, c := range t.Children[i] {
		if t.FieldName(c) == name {
			return c
		}
	}
	return -1
}

type names struct {
	symbols []string
	fields  []string
}

type runtime struct {
	mu    sync.Mutex
	ctx   context.Context
	mod   api.Module
	alloc api.Function
	free  api.Function
	parse api.Function
	names [nlang]*names
}

var (
	once    sync.Once
	rt      *runtime
	initErr error
)

func get() (*runtime, error) {
	once.Do(func() { rt, initErr = start(context.Background()) })
	return rt, initErr
}

func start(ctx context.Context) (*runtime, error) {
	cfg := wazero.NewRuntimeConfig()
	if dir, err := os.UserCacheDir(); err == nil {
		// Compiling the module to machine code takes ~150 ms; cache it.
		if cache, err := wazero.NewCompilationCacheWithDir(filepath.Join(dir, "brisk", "wazero")); err == nil {
			cfg = cfg.WithCompilationCache(cache)
		}
	}
	r := wazero.NewRuntimeWithConfig(ctx, cfg)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	mod, err := r.InstantiateWithConfig(ctx, wasm,
		wazero.NewModuleConfig().WithStartFunctions("_initialize"))
	if err != nil {
		return nil, fmt.Errorf("tswasm: instantiate: %w", err)
	}
	x := &runtime{
		ctx:   ctx,
		mod:   mod,
		alloc: mod.ExportedFunction("bk_alloc"),
		free:  mod.ExportedFunction("bk_free"),
		parse: mod.ExportedFunction("bk_parse"),
	}
	for l := Lang(0); l < nlang; l++ {
		n, err := x.loadNames(l)
		if err != nil {
			return nil, err
		}
		x.names[l] = n
	}
	return x, nil
}

func (x *runtime) call(name string, args ...uint64) (uint64, error) {
	res, err := x.mod.ExportedFunction(name).Call(x.ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("tswasm: %s: %w", name, err)
	}
	return res[0], nil
}

func (x *runtime) cstring(ptr uint64) string {
	mem := x.mod.Memory()
	var b []byte
	for p := uint32(ptr); ptr != 0; p++ {
		c, ok := mem.ReadByte(p)
		if !ok || c == 0 {
			break
		}
		b = append(b, c)
	}
	return string(b)
}

func (x *runtime) loadNames(l Lang) (*names, error) {
	n := &names{}
	count, err := x.call("bk_symbol_count", uint64(l))
	if err != nil {
		return nil, err
	}
	for s := uint64(0); s < count; s++ {
		p, err := x.call("bk_symbol_name", uint64(l), s)
		if err != nil {
			return nil, err
		}
		n.symbols = append(n.symbols, x.cstring(p))
	}
	// ts_language_symbol_name(65535) is "ERROR"; keep the slice indexable.
	for len(n.symbols) <= 0xFFFF {
		n.symbols = append(n.symbols, "ERROR")
	}
	count, err = x.call("bk_field_count", uint64(l))
	if err != nil {
		return nil, err
	}
	if count >= 256 {
		return nil, fmt.Errorf("tswasm: grammar %d has %d fields; shim.c stores 8 bits", l, count)
	}
	n.fields = []string{""}
	for f := uint64(1); f <= count; f++ {
		p, err := x.call("bk_field_name", uint64(l), f)
		if err != nil {
			return nil, err
		}
		n.fields = append(n.fields, x.cstring(p))
	}
	for len(n.fields) < 256 {
		n.fields = append(n.fields, "")
	}
	return n, nil
}

// Parse parses src. A file with syntax errors still returns a tree; check
// Nodes[0].HasError().
func Parse(l Lang, src []byte) (*Tree, error) {
	if l >= nlang {
		return nil, fmt.Errorf("tswasm: unknown language %d", l)
	}
	x, err := get()
	if err != nil {
		return nil, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()

	res, err := x.alloc.Call(x.ctx, uint64(len(src)+4))
	if err != nil || res[0] == 0 {
		return nil, fmt.Errorf("tswasm: alloc %d bytes: %v", len(src), err)
	}
	buf := uint32(res[0])
	defer x.free.Call(x.ctx, uint64(buf)) //nolint:errcheck
	countPtr := buf + uint32(len(src))
	mem := x.mod.Memory()
	if !mem.Write(buf, src) {
		return nil, fmt.Errorf("tswasm: write source")
	}
	res, err = x.parse.Call(x.ctx, uint64(l), uint64(buf), uint64(len(src)), uint64(countPtr))
	if err != nil {
		return nil, fmt.Errorf("tswasm: parse: %w", err)
	}
	if res[0] == 0 {
		return nil, fmt.Errorf("tswasm: parse failed")
	}
	count, _ := mem.ReadUint32Le(countPtr)
	raw, ok := mem.Read(uint32(res[0]), count*6*4)
	if !ok {
		return nil, fmt.Errorf("tswasm: read %d records", count)
	}
	t := &Tree{Lang: l, Source: src, Nodes: make([]Node, count), Children: make([][]int32, count), names: x.names[l]}
	for i := range t.Nodes {
		r := raw[i*24:]
		w0 := binary.LittleEndian.Uint32(r)
		parent := int32(binary.LittleEndian.Uint32(r[20:]))
		t.Nodes[i] = Node{
			Symbol:    uint16(w0),
			Field:     uint8(w0 >> 16),
			flags:     uint8(w0 >> 24),
			StartByte: binary.LittleEndian.Uint32(r[4:]),
			EndByte:   binary.LittleEndian.Uint32(r[8:]),
			StartRow:  binary.LittleEndian.Uint32(r[12:]),
			EndRow:    binary.LittleEndian.Uint32(r[16:]),
			Parent:    parent,
		}
		if parent >= 0 {
			t.Children[parent] = append(t.Children[parent], int32(i))
		}
	}
	return t, nil
}

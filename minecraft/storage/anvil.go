/*
 * Copyright (c) 2026 Gilles Chehade <gilles@poolp.org>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

// The anvil side of the store: the chunk NBT we write and read back,
// the bit packing of palette indices, and the region files that hold
// the chunks.  Only what the codec needs, nothing the server would
// have to tolerate.

package storage

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"

	"github.com/Tnze/go-mc/nbt"
	"github.com/Tnze/go-mc/save/region"
)

// dataVersion is the world format we write, 1.20.1.  A newer server
// upgrades the chunks when it first loads them, which keeps the block
// identities the codec depends on.
const dataVersion = 3465

const (
	minSection  = -4 // y = -64
	numSections = 24 // up to y = 319
	worldHeight = numSections * 16
)

// chunkNBT is the part of a chunk we write and the only part we read.
// Anything else the server adds on a rewrite (lights, ticks, block
// entities) is ignored by the decoder.
type chunkNBT struct {
	DataVersion   int32               `nbt:"DataVersion"`
	XPos          int32               `nbt:"xPos"`
	YPos          int32               `nbt:"yPos"`
	ZPos          int32               `nbt:"zPos"`
	Status        string              `nbt:"Status"`
	LastUpdate    int64               `nbt:"LastUpdate"`
	InhabitedTime int64               `nbt:"InhabitedTime"`
	IsLightOn     byte                `nbt:"isLightOn"`
	Sections      []sectionNBT        `nbt:"sections"`
	Heightmaps    map[string][]uint64 `nbt:"Heightmaps"`
}

type sectionNBT struct {
	Y           int8       `nbt:"Y"`
	BlockStates paletteNBT `nbt:"block_states"`
	Biomes      biomesNBT  `nbt:"biomes"`
}

type paletteNBT struct {
	Palette []blockNBT `nbt:"palette"`
	Data    []uint64   `nbt:"data,omitempty"`
}

type blockNBT struct {
	Name       string            `nbt:"Name"`
	Properties map[string]string `nbt:"Properties,omitempty"`
}

// UnmarshalNBT accepts a palette entry as the compound we write and as
// the bare string a 26.x server writes for a block without properties.
func (b *blockNBT) UnmarshalNBT(tagType byte, r nbt.DecoderReader) error {
	var raw nbt.RawMessage
	if err := raw.UnmarshalNBT(tagType, r); err != nil {
		return err
	}
	switch tagType {
	case nbt.TagString:
		b.Properties = nil
		return raw.Unmarshal(&b.Name)
	case nbt.TagCompound:
		var c struct {
			Name       string            `nbt:"Name"`
			Properties map[string]string `nbt:"Properties"`
		}
		if err := raw.Unmarshal(&c); err != nil {
			return err
		}
		b.Name, b.Properties = c.Name, c.Properties
		return nil
	default:
		return fmt.Errorf("palette entry of type %d", tagType)
	}
}

type biomesNBT struct {
	Palette []string `nbt:"palette"`
	Data    []uint64 `nbt:"data,omitempty"`
}

// paletteBits is the width of a palette index in a block states array,
// as the game packs it: at least 4 bits, none at all for a single entry.
func paletteBits(n int) int {
	if n <= 1 {
		return 0
	}
	b := bits.Len(uint(n - 1))
	if b < 4 {
		b = 4
	}
	return b
}

// packBits packs values of nbits each into longs, never straddling a
// long boundary, the way the game does since 1.16.
func packBits(vals []uint16, nbits int) []uint64 {
	per := 64 / nbits
	out := make([]uint64, (len(vals)+per-1)/per)
	for i, v := range vals {
		out[i/per] |= uint64(v) << ((i % per) * nbits)
	}
	return out
}

func unpackBits(data []uint64, nbits, i int) (int, error) {
	per := 64 / nbits
	if i/per >= len(data) {
		return 0, fmt.Errorf("packed array too short: %d longs for index %d at %d bits", len(data), i, nbits)
	}
	return int(data[i/per]>>((i%per)*nbits)) & (1<<nbits - 1), nil
}

// block returns the name of the block at index i of a section, i being
// y*256 + z*16 + x within the section.
func (s *sectionNBT) block(i int) (string, error) {
	n := len(s.BlockStates.Palette)
	switch {
	case n == 0:
		return "minecraft:air", nil
	case n == 1:
		return s.BlockStates.Palette[0].Name, nil
	}
	idx, err := unpackBits(s.BlockStates.Data, paletteBits(n), i)
	if err != nil {
		return "", err
	}
	if idx >= n {
		return "", fmt.Errorf("palette index %d out of %d entries", idx, n)
	}
	return s.BlockStates.Palette[idx].Name, nil
}

// sectionBuilder accumulates the blocks of one section before packing.
type sectionBuilder struct {
	y       int8
	names   []string
	index   map[string]uint16
	indices [4096]uint16
}

func newSectionBuilder(y int8) *sectionBuilder {
	return &sectionBuilder{y: y, index: make(map[string]uint16)}
}

func (b *sectionBuilder) set(i int, name string) {
	idx, ok := b.index[name]
	if !ok {
		idx = uint16(len(b.names))
		b.names = append(b.names, name)
		b.index[name] = idx
	}
	b.indices[i] = idx
}

func (b *sectionBuilder) build() sectionNBT {
	s := sectionNBT{
		Y:      b.y,
		Biomes: biomesNBT{Palette: []string{"minecraft:plains"}},
	}
	for _, name := range b.names {
		s.BlockStates.Palette = append(s.BlockStates.Palette, blockNBT{Name: name})
	}
	if nbits := paletteBits(len(b.names)); nbits > 0 {
		s.BlockStates.Data = packBits(b.indices[:], nbits)
	}
	return s
}

func uniformSection(y int8, name string) sectionNBT {
	return sectionNBT{
		Y:           y,
		BlockStates: paletteNBT{Palette: []blockNBT{{Name: name}}},
		Biomes:      biomesNBT{Palette: []string{"minecraft:plains"}},
	}
}

// sectorData is a chunk as stored in a region file: a compression byte
// followed by the compressed NBT.
func sectorData(c *chunkNBT) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte(2)
	zw := zlib.NewWriter(&buf)
	if err := nbt.NewEncoder(zw).Encode(c, ""); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func parseSector(data []byte) (*chunkNBT, error) {
	if len(data) < 2 {
		return nil, errors.New("chunk too short")
	}
	var r io.Reader = bytes.NewReader(data[1:])
	var err error
	switch data[0] {
	case 1:
		r, err = gzip.NewReader(r)
	case 2:
		r, err = zlib.NewReader(r)
	case 3:
	default:
		err = fmt.Errorf("unknown chunk compression %d", data[0])
	}
	if err != nil {
		return nil, err
	}
	var c chunkNBT
	if _, err := nbt.NewDecoder(r).Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

func regionFile(dir string, x, z int32) string {
	return filepath.Join(dir, fmt.Sprintf("r.%d.%d.mca", x, z))
}

func parseRegionFile(name string) (x, z int32, ok bool) {
	var tail string
	n, err := fmt.Sscanf(name, "r.%d.%d.mca%s", &x, &z, &tail)
	return x, z, err == io.EOF && n == 2 || err == nil && n == 2 && tail == ""
}

// regionReader reads chunks from one region file.
type regionReader struct {
	f *os.File
	r *region.Region
}

func openRegion(path string) (*regionReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r, err := region.Load(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &regionReader{f: f, r: r}, nil
}

func (rr *regionReader) Close() error { return rr.f.Close() }

// chunk reads the chunk at (cx, cz) within the region.
func (rr *regionReader) chunk(cx, cz int) (*chunkNBT, error) {
	data, err := rr.r.ReadSector(cx, cz)
	if err != nil {
		return nil, err
	}
	return parseSector(data)
}

// writeRegion writes a brand new region file holding the given chunks,
// keyed by their position within the region, through a temporary file
// so that a region is either whole or absent.
func writeRegion(path string, chunks map[[2]int]*chunkNBT) error {
	tmp := path + ".tmp"
	os.Remove(tmp)
	r, err := region.Create(tmp)
	if err != nil {
		return err
	}
	for pos, c := range chunks {
		data, err := sectorData(c)
		if err != nil {
			r.Close()
			os.Remove(tmp)
			return err
		}
		if err := r.WriteSector(pos[0], pos[1], data); err != nil {
			r.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := r.PadToFullSector(); err != nil {
		r.Close()
		os.Remove(tmp)
		return err
	}
	if err := r.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

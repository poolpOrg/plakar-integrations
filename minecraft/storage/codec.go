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

// The terrain codec.  Every column of the world carries two bytes:
//
//	byte 0: low nibble  = height, 0 to 15 blocks of stone above the base
//	        high nibble = colour of the stained glass on the stone
//	byte 1: low nibble  = colour of the terracotta above the glass
//	        high nibble = colour of the concrete on top, where players walk
//
// so a chunk holds 512 bytes and a region 1023 chunks of payload, chunk
// (0,0) of every region being a header that names the resource the
// region belongs to.  Everything below the base is stone, everything
// above the concrete is air.  None of the blocks fall, burn, flow,
// spread or get picked up, and the decoder only looks at block names,
// which is what survives a server relighting or upgrading a chunk.

package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const (
	codecTerrain = 1

	chunkBytes      = 512
	chunksPerRegion = 1023
	regionBytes     = chunkBytes * chunksPerRegion

	// footprintWidth is how many regions wide a resource is laid out,
	// row after row.
	footprintWidth = 16

	baseY     = 64 // y of the first stone block of a column
	maxHeight = 15 // stone blocks above the base, a nibble
	layers    = 3  // glass, terracotta, concrete
	topY      = baseY + maxHeight + layers - 1
)

var colours = [16]string{
	"white", "orange", "magenta", "light_blue",
	"yellow", "lime", "pink", "gray",
	"light_gray", "cyan", "purple", "blue",
	"brown", "green", "red", "black",
}

var colourIndex = func() map[string]byte {
	m := make(map[string]byte, len(colours))
	for i, c := range colours {
		m[c] = byte(i)
	}
	return m
}()

// column is what one column of the chunk holds, from the base up.
func column(b0, b1 byte) (height int, names [layers]string) {
	height = int(b0 & 15)
	names[0] = "minecraft:" + colours[b0>>4] + "_stained_glass"
	names[1] = "minecraft:" + colours[b1&15] + "_terracotta"
	names[2] = "minecraft:" + colours[b1>>4] + "_concrete"
	return
}

// encodeChunk builds the chunk at (cx, cz) holding payload, at most
// chunkBytes of it; a shorter payload is padded with zeros, which is a
// flat white plain.
func encodeChunk(cx, cz int32, payload []byte) *chunkNBT {
	var data [chunkBytes]byte
	copy(data[:], payload)

	c := &chunkNBT{
		DataVersion: dataVersion,
		XPos:        cx,
		YPos:        minSection,
		ZPos:        cz,
		Status:      "full",
		Heightmaps:  make(map[string][]uint64),
	}

	var heights [256]uint16
	for s := minSection; s < minSection+numSections; s++ {
		y0 := s * 16
		switch {
		case y0+16 <= baseY:
			c.Sections = append(c.Sections, uniformSection(int8(s), "minecraft:stone"))
			continue
		case y0 > topY:
			c.Sections = append(c.Sections, uniformSection(int8(s), "minecraft:air"))
			continue
		}

		b := newSectionBuilder(int8(s))
		for z := 0; z < 16; z++ {
			for x := 0; x < 16; x++ {
				col := z*16 + x
				height, names := column(data[2*col], data[2*col+1])
				top := baseY + height + layers - 1
				heights[col] = uint16(top - minSection*16 + 1)
				for dy := 0; dy < 16; dy++ {
					y := y0 + dy
					var name string
					switch {
					case y < baseY+height:
						name = "minecraft:stone"
					case y <= top:
						name = names[y-baseY-height]
					default:
						name = "minecraft:air"
					}
					b.set(dy*256+col, name)
				}
			}
		}
		c.Sections = append(c.Sections, b.build())
	}

	packed := packBits(heights[:], 9)
	for _, key := range []string{"MOTION_BLOCKING", "MOTION_BLOCKING_NO_LEAVES", "OCEAN_FLOOR", "WORLD_SURFACE"} {
		c.Heightmaps[key] = packed
	}
	return c
}

var errNotTerrain = errors.New("chunk is not plakar terrain")

// decodeChunk reads the payload back from a chunk, ours or the
// server's rewrite of it.
func decodeChunk(c *chunkNBT) ([]byte, error) {
	sections := make(map[int]*sectionNBT, len(c.Sections))
	for i := range c.Sections {
		sections[int(c.Sections[i].Y)] = &c.Sections[i]
	}
	blockAt := func(x, y, z int) (string, error) {
		s, ok := sections[y>>4]
		if !ok {
			return "minecraft:air", nil
		}
		return s.block((y&15)*256 + z*16 + x)
	}

	out := make([]byte, chunkBytes)
	for z := 0; z < 16; z++ {
		for x := 0; x < 16; x++ {
			top := -1
			for y := topY; y >= baseY; y-- {
				name, err := blockAt(x, y, z)
				if err != nil {
					return nil, err
				}
				if name != "minecraft:air" {
					top = y
					break
				}
			}
			height := top - baseY - layers + 1
			if height < 0 || height > maxHeight {
				return nil, fmt.Errorf("%w: column %d,%d tops at %d", errNotTerrain, x, z, top)
			}
			var c [layers]byte
			for i, suffix := range []string{"_stained_glass", "_terracotta", "_concrete"} {
				name, err := blockAt(x, top-layers+1+i, z)
				if err != nil {
					return nil, err
				}
				colour, ok := strings.CutPrefix(name, "minecraft:")
				if !ok {
					return nil, fmt.Errorf("%w: %s", errNotTerrain, name)
				}
				colour, ok = strings.CutSuffix(colour, suffix)
				if !ok {
					return nil, fmt.Errorf("%w: %s", errNotTerrain, name)
				}
				idx, ok := colourIndex[colour]
				if !ok {
					return nil, fmt.Errorf("%w: %s", errNotTerrain, name)
				}
				c[i] = idx
			}
			col := z*16 + x
			out[2*col] = byte(height) | c[0]<<4
			out[2*col+1] = c[1] | c[2]<<4
		}
	}
	return out, nil
}

// header is what chunk (0,0) of every region of a resource carries, as
// terrain like the rest.
type header struct {
	codec    uint8
	resource uint8
	mac      [32]byte
	length   uint64
	index    uint32 // this region's rank within the resource
	count    uint32 // regions in the resource
}

const (
	headerMagic   = "PLAKARMC"
	headerVersion = 1
	headerSize    = 8 + 1 + 1 + 1 + 32 + 8 + 4 + 4
)

func (h header) marshal() []byte {
	buf := make([]byte, 0, headerSize)
	buf = append(buf, headerMagic...)
	buf = append(buf, headerVersion, h.codec, h.resource)
	buf = append(buf, h.mac[:]...)
	buf = binary.BigEndian.AppendUint64(buf, h.length)
	buf = binary.BigEndian.AppendUint32(buf, h.index)
	buf = binary.BigEndian.AppendUint32(buf, h.count)
	return buf
}

var errNoHeader = errors.New("no plakar header")

func unmarshalHeader(buf []byte) (header, error) {
	var h header
	if len(buf) < headerSize || !bytes.Equal(buf[:8], []byte(headerMagic)) {
		return h, errNoHeader
	}
	if buf[8] != headerVersion {
		return h, fmt.Errorf("header version %d not supported", buf[8])
	}
	h.codec = buf[9]
	h.resource = buf[10]
	copy(h.mac[:], buf[11:43])
	h.length = binary.BigEndian.Uint64(buf[43:51])
	h.index = binary.BigEndian.Uint32(buf[51:55])
	h.count = binary.BigEndian.Uint32(buf[55:59])
	if h.codec != codecTerrain {
		return h, fmt.Errorf("codec %d not supported", h.codec)
	}
	return h, nil
}

// regionCount is how many regions a resource of the given length takes.
func regionCount(length uint64) uint32 {
	n := (length + regionBytes - 1) / regionBytes
	if n == 0 {
		n = 1
	}
	return uint32(n)
}

// footprint is where a resource starts in the world, from its MAC;
// probe moves it elsewhere when that spot is taken.
func footprint(mac [32]byte, probe int) (x, z int32) {
	seed := binary.BigEndian.Uint64(mac[:8]) + uint64(probe)*0x9E3779B97F4A7C15
	x = int32(seed>>32&0xffff) - 32768
	z = int32(seed&0xffff) - 32768
	return
}

// regionAt is the position of the i-th region of a resource laid out
// from (x, z).
func regionAt(x, z int32, i uint32) (int32, int32) {
	return x + int32(i%footprintWidth), z + int32(i/footprintWidth)
}

// chunkAt is the position within its region of the j-th payload chunk,
// j counting from 0 and skipping the header at (0,0).
func chunkAt(j int) (int, int) {
	j++
	return j % 32, j / 32
}

// encodeRegion builds the chunks of region i of a resource laid out
// from (x, z): the header at (0,0) and one chunk per chunkBytes of
// data, chunks past the end of the data being left out.
func encodeRegion(h header, i uint32, x, z int32, data []byte) map[[2]int]*chunkNBT {
	h.index = i
	rx, rz := regionAt(x, z, i)
	place := func(cx, cz int, c *chunkNBT) {
		c.XPos = rx*32 + int32(cx)
		c.ZPos = rz*32 + int32(cz)
	}

	chunks := make(map[[2]int]*chunkNBT)
	hc := encodeChunk(0, 0, h.marshal())
	place(0, 0, hc)
	chunks[[2]int{0, 0}] = hc

	start := uint64(i) * regionBytes
	for j := 0; j < chunksPerRegion; j++ {
		off := start + uint64(j)*chunkBytes
		if off >= uint64(len(data)) {
			break
		}
		end := min(off+chunkBytes, uint64(len(data)))
		cx, cz := chunkAt(j)
		c := encodeChunk(0, 0, data[off:end])
		place(cx, cz, c)
		chunks[[2]int{cx, cz}] = c
	}
	return chunks
}

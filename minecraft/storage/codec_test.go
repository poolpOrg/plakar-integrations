package storage

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/Tnze/go-mc/nbt"
	"github.com/stretchr/testify/require"
)

func TestPaletteBits(t *testing.T) {
	for n, want := range map[int]int{0: 0, 1: 0, 2: 4, 16: 4, 17: 5, 32: 5, 33: 6, 64: 6} {
		require.Equal(t, want, paletteBits(n), "palette of %d", n)
	}
}

func TestPackUnpack(t *testing.T) {
	vals := make([]uint16, 4096)
	for i := range vals {
		vals[i] = uint16(i % 37)
	}
	packed := packBits(vals, 6)
	require.Len(t, packed, (4096+9)/10)
	for i, v := range vals {
		got, err := unpackBits(packed, 6, i)
		require.NoError(t, err)
		require.Equal(t, int(v), got)
	}
	_, err := unpackBits(packed, 6, 4096*2)
	require.Error(t, err)
}

func TestChunkRoundTrip(t *testing.T) {
	payload := make([]byte, chunkBytes)
	_, err := rand.Read(payload)
	require.NoError(t, err)

	c := encodeChunk(3, -7, payload)
	require.Equal(t, int32(3), c.XPos)
	require.Equal(t, int32(-7), c.ZPos)
	require.Len(t, c.Sections, numSections)
	require.Len(t, c.Heightmaps["WORLD_SURFACE"], 37)

	data, err := sectorData(c)
	require.NoError(t, err)
	require.Less(t, len(data), 1<<20)

	back, err := parseSector(data)
	require.NoError(t, err)
	got, err := decodeChunk(back)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestChunkShortPayloadIsPadded(t *testing.T) {
	c := encodeChunk(0, 0, []byte("hello"))
	got, err := decodeChunk(c)
	require.NoError(t, err)
	require.Equal(t, []byte("hello"), got[:5])
	require.Equal(t, make([]byte, chunkBytes-5), got[5:])
}

func TestDecodeRejectsForeignChunk(t *testing.T) {
	c := encodeChunk(0, 0, nil)
	// a chunk the server would hand us for an area we never wrote
	c.Sections = nil
	_, err := decodeChunk(c)
	require.ErrorIs(t, err, errNotTerrain)
}

func TestHeaderRoundTrip(t *testing.T) {
	var h header
	h.codec = codecTerrain
	h.resource = 1
	_, _ = rand.Read(h.mac[:])
	h.length = 123456789
	h.index = 3
	h.count = 9

	buf := h.marshal()
	require.Len(t, buf, headerSize)
	got, err := unmarshalHeader(buf)
	require.NoError(t, err)
	require.Equal(t, h, got)

	_, err = unmarshalHeader(bytes.Repeat([]byte{0}, headerSize))
	require.ErrorIs(t, err, errNoHeader)

	// through the terrain, as the store reads it
	c := encodeChunk(0, 0, buf)
	data, err := decodeChunk(c)
	require.NoError(t, err)
	got, err = unmarshalHeader(data)
	require.NoError(t, err)
	require.Equal(t, h, got)
}

func TestLayout(t *testing.T) {
	require.Equal(t, uint32(1), regionCount(0))
	require.Equal(t, uint32(1), regionCount(regionBytes))
	require.Equal(t, uint32(2), regionCount(regionBytes+1))

	var mac [32]byte
	_, _ = rand.Read(mac[:])
	x0, z0 := footprint(mac, 0)
	x1, z1 := footprint(mac, 0)
	require.Equal(t, [2]int32{x0, z0}, [2]int32{x1, z1})
	x2, z2 := footprint(mac, 1)
	require.NotEqual(t, [2]int32{x0, z0}, [2]int32{x2, z2})

	rx, rz := regionAt(10, 20, footprintWidth+3)
	require.Equal(t, int32(13), rx)
	require.Equal(t, int32(21), rz)

	cx, cz := chunkAt(0)
	require.Equal(t, [2]int{1, 0}, [2]int{cx, cz})
	cx, cz = chunkAt(chunksPerRegion - 1)
	require.Equal(t, [2]int{31, 31}, [2]int{cx, cz})
}

func TestRegionFileName(t *testing.T) {
	x, z, ok := parseRegionFile("r.-3.17.mca")
	require.True(t, ok)
	require.Equal(t, int32(-3), x)
	require.Equal(t, int32(17), z)
	_, _, ok = parseRegionFile("r.-3.17.mca.tmp")
	require.False(t, ok)
	_, _, ok = parseRegionFile("level.dat")
	require.False(t, ok)
}

// A 26.x server rewrites the palette of a section whose blocks carry no
// properties as a list of strings; the decoder must take both forms.
func TestDecodeStringPalette(t *testing.T) {
	payload := make([]byte, chunkBytes)
	_, err := rand.Read(payload)
	require.NoError(t, err)
	c := encodeChunk(0, 0, payload)

	type stringPalette struct {
		Palette []string `nbt:"palette"`
		Data    []uint64 `nbt:"data,omitempty"`
	}
	type stringSection struct {
		Y           int8          `nbt:"Y"`
		BlockStates stringPalette `nbt:"block_states"`
		Biomes      biomesNBT     `nbt:"biomes"`
	}
	type stringChunk struct {
		Sections []stringSection `nbt:"sections"`
	}
	var sc stringChunk
	for _, s := range c.Sections {
		names := make([]string, 0, len(s.BlockStates.Palette))
		for _, b := range s.BlockStates.Palette {
			names = append(names, b.Name)
		}
		sc.Sections = append(sc.Sections, stringSection{
			Y:           s.Y,
			BlockStates: stringPalette{Palette: names, Data: s.BlockStates.Data},
			Biomes:      s.Biomes,
		})
	}
	var buf bytes.Buffer
	require.NoError(t, nbt.NewEncoder(&buf).Encode(sc, ""))

	var back chunkNBT
	_, err = nbt.NewDecoder(&buf).Decode(&back)
	require.NoError(t, err)
	got, err := decodeChunk(&back)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

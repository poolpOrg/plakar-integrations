package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/Tnze/go-mc/save"
	"github.com/poolpOrg/plakar-integrations/minecraft/server/servertest"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "server")
	st, err := NewStore(context.Background(), "minecraft", map[string]string{
		"location": "minecraft://" + dir,
	})
	require.NoError(t, err)
	return st.(*Store), dir
}

func randomResource(t *testing.T, size int) (integration.MAC, []byte) {
	t.Helper()
	data := make([]byte, size)
	_, err := rand.Read(data)
	require.NoError(t, err)
	return sha256.Sum256(data), data
}

func get(t *testing.T, st *Store, res integration.StorageResource, mac integration.MAC, rg *integration.Range) []byte {
	t.Helper()
	rd, err := st.Get(context.Background(), res, 0, mac, rg)
	require.NoError(t, err)
	defer rd.Close()
	data, err := io.ReadAll(rd)
	require.NoError(t, err)
	return data
}

func list(t *testing.T, st *Store, res integration.StorageResource) []integration.MAC {
	t.Helper()
	macs := make([]integration.MAC, 0)
	for e, err := range st.List(context.Background(), res, 0) {
		require.NoError(t, err)
		macs = append(macs, e.MAC)
	}
	return macs
}

func TestCreateOpen(t *testing.T) {
	ctx := context.Background()
	st, dir := newTestStore(t)

	config := []byte("the kloset configuration")
	require.NoError(t, st.Create(ctx, config))
	require.DirExists(t, filepath.Join(dir, "world", "region"))
	require.FileExists(t, filepath.Join(dir, "world", "level.dat"))

	got, err := st.Open(ctx)
	require.NoError(t, err)
	require.Equal(t, config, got)

	require.ErrorIs(t, st.Create(ctx, config), ErrExists)

	// level-name in server.properties names the world
	other, odir := newTestStore(t)
	require.NoError(t, os.MkdirAll(odir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(odir, "server.properties"), []byte("level-name=backups\n"), 0600))
	require.NoError(t, other.Create(ctx, config))
	require.DirExists(t, filepath.Join(odir, "backups", "region"))
}

func TestLevelDat(t *testing.T) {
	st, dir := newTestStore(t)
	require.NoError(t, st.Create(context.Background(), nil))

	f, err := os.Open(filepath.Join(dir, "world", "level.dat"))
	require.NoError(t, err)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)

	// the game's own struct, strict about what it finds
	level, err := save.ReadLevel(zr)
	require.NoError(t, err)
	require.Equal(t, "world", level.Data.LevelName)
	require.Equal(t, int32(2), level.Data.GameType)
	require.Equal(t, "0", level.Data.GameRules["randomTickSpeed"])
	require.Equal(t, "minecraft:flat", level.Data.WorldGenSettings.Dimensions["minecraft:overworld"].Generator["type"])
	require.NotZero(t, level.Data.BorderSize)
}

func TestResourceRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, dir := newTestStore(t)
	require.NoError(t, st.Create(ctx, nil))

	// three regions, the last one partial
	mac, data := randomResource(t, 2*regionBytes+12345)
	n, err := st.Put(ctx, integration.StorageResourcePackfile, 0, mac, bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), n)

	regions, err := filepath.Glob(filepath.Join(dir, "world", "region", "r.*.mca"))
	require.NoError(t, err)
	require.Len(t, regions, 3)

	macs := list(t, st, integration.StorageResourcePackfile)
	require.Equal(t, []integration.MAC{mac}, macs)
	macs = list(t, st, integration.StorageResourceState)
	require.Empty(t, macs)

	got := get(t, st, integration.StorageResourcePackfile, mac, nil)
	require.Equal(t, data, got)

	for _, rg := range []integration.Range{
		{Offset: 0, Length: 1},
		{Offset: 0, Length: chunkBytes},
		{Offset: chunkBytes - 1, Length: 2},
		{Offset: 777, Length: 3 * chunkBytes},
		{Offset: regionBytes - 10, Length: 20},
		{Offset: 2 * regionBytes, Length: 12345},
		{Offset: uint64(len(data)) - 1, Length: 1},
		{Offset: uint64(len(data)) - 100, Length: 1000}, // clamped
	} {
		got := get(t, st, integration.StorageResourcePackfile, mac, &rg)
		end := min(rg.Offset+uint64(rg.Length), uint64(len(data)))
		require.Equal(t, data[rg.Offset:end], got, "range %+v", rg)
	}

	// not the same resource type
	_, err = st.Get(ctx, integration.StorageResourceState, 0, mac, nil)
	require.ErrorIs(t, err, ErrNotFound)

	// a second put of the same thing is a no-op
	n, err = st.Put(ctx, integration.StorageResourcePackfile, 0, mac, bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), n)
	regions, _ = filepath.Glob(filepath.Join(dir, "world", "region", "r.*.mca"))
	require.Len(t, regions, 3)

	require.NoError(t, st.Delete(ctx, integration.StorageResourcePackfile, 0, mac))
	regions, _ = filepath.Glob(filepath.Join(dir, "world", "region", "r.*.mca"))
	require.Empty(t, regions)
	_, err = st.Get(ctx, integration.StorageResourcePackfile, 0, mac, nil)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, st.Delete(ctx, integration.StorageResourcePackfile, 0, mac), ErrNotFound)
}

func TestSmallResources(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)
	require.NoError(t, st.Create(ctx, nil))

	var want []integration.MAC
	for _, size := range []int{0, 1, 511, 512, 513, 4096} {
		mac, data := randomResource(t, size)
		_, err := st.Put(ctx, integration.StorageResourceState, 0, mac, bytes.NewReader(data))
		require.NoError(t, err)
		got := get(t, st, integration.StorageResourceState, mac, nil)
		require.Equal(t, data, got)
		want = append(want, mac)
	}
	macs := list(t, st, integration.StorageResourceState)
	require.ElementsMatch(t, want, macs)

	size, err := st.Size(ctx)
	require.NoError(t, err)
	require.Positive(t, size)
}

func TestLocks(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)
	require.NoError(t, st.Create(ctx, nil))

	mac, data := randomResource(t, 64)
	_, err := st.Put(ctx, integration.StorageResourceLock, 0, mac, bytes.NewReader(data))
	require.NoError(t, err)
	macs := list(t, st, integration.StorageResourceLock)
	require.Equal(t, []integration.MAC{mac}, macs)
	require.Equal(t, data, get(t, st, integration.StorageResourceLock, mac, nil))
	require.NoError(t, st.Delete(ctx, integration.StorageResourceLock, 0, mac))
	macs = list(t, st, integration.StorageResourceLock)
	require.Empty(t, macs)

	// locks are files, not terrain
	regions, _ := filepath.Glob(filepath.Join(st.regionDir(), "r.*.mca"))
	require.Empty(t, regions)
}

func TestMain(m *testing.M) {
	servertest.Main(m)
}

func TestRefusesRunningServer(t *testing.T) {
	ctx := context.Background()
	st, dir := newTestStore(t)
	require.NoError(t, st.Create(ctx, []byte("config")))

	mac, data := randomResource(t, 100)
	_, err := st.Put(ctx, integration.StorageResourcePackfile, 0, mac, bytes.NewReader(data))
	require.NoError(t, err)

	servertest.Hold(t, filepath.Join(dir, "world", "session.lock"))

	// writes are refused
	other, _ := randomResource(t, 100)
	_, err = st.Put(ctx, integration.StorageResourcePackfile, 0, other, bytes.NewReader(data))
	require.ErrorIs(t, err, ErrRunning)
	require.ErrorIs(t, st.Delete(ctx, integration.StorageResourcePackfile, 0, mac), ErrRunning)
	require.ErrorIs(t, st.Create(ctx, nil), ErrRunning)

	// reads, locks and the configuration are not
	require.Equal(t, data, get(t, st, integration.StorageResourcePackfile, mac, nil))
	require.Equal(t, []integration.MAC{mac}, list(t, st, integration.StorageResourcePackfile))
	config, err := st.Open(ctx)
	require.NoError(t, err)
	require.Equal(t, []byte("config"), config)
	_, err = st.Put(ctx, integration.StorageResourceLock, 0, mac, bytes.NewReader(data))
	require.NoError(t, err)
}

func TestModernWorldLayout(t *testing.T) {
	ctx := context.Background()
	st, dir := newTestStore(t)
	require.NoError(t, st.Create(ctx, nil))

	mac, data := randomResource(t, 3000)
	_, err := st.Put(ctx, integration.StorageResourcePackfile, 0, mac, bytes.NewReader(data))
	require.NoError(t, err)

	// what a 26.3 server does to the world on its first start
	modern := filepath.Join(dir, "world", "dimensions", "minecraft", "overworld")
	require.NoError(t, os.MkdirAll(modern, 0700))
	require.NoError(t, os.Rename(filepath.Join(dir, "world", "region"), filepath.Join(modern, "region")))

	require.Equal(t, data, get(t, st, integration.StorageResourcePackfile, mac, nil))
	require.Equal(t, []integration.MAC{mac}, list(t, st, integration.StorageResourcePackfile))

	other, odata := randomResource(t, 100)
	_, err = st.Put(ctx, integration.StorageResourceState, 0, other, bytes.NewReader(odata))
	require.NoError(t, err)
	regions, _ := filepath.Glob(filepath.Join(modern, "region", "r.*.mca"))
	require.Len(t, regions, 2)
	require.NoDirExists(t, filepath.Join(dir, "world", "region"))
}

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

package importer_test

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/PlakarKorp/kloset/snapshot/vfs"
	ptesting "github.com/PlakarKorp/kloset/testing"
	"github.com/poolpOrg/plakar-integrations/android/adb/adbtest"
	"github.com/poolpOrg/plakar-integrations/android/exporter"
	"github.com/poolpOrg/plakar-integrations/android/importer"
	"github.com/stretchr/testify/require"
)

// backup takes a snapshot of the fake device through kloset, seeding the
// VFS cache from a previous snapshot when given.
func backup(t *testing.T, repo *repository.Repository, srv *adbtest.Server, cache *vfs.Filesystem) *snapshot.Snapshot {
	t.Helper()

	builder, err := snapshot.Create(repo, repository.DefaultType, "", objects.NilMac, &snapshot.BuilderOptions{Name: "android"})
	require.NoError(t, err)
	if cache != nil {
		builder.WithVFSCache(cache)
	}

	opts := &integration.Options{Hostname: "test", MaxConcurrency: 4}
	imp, err := importer.NewImporter(repo.AppContext(), opts, "android", map[string]string{
		"location": "android://" + serial, "adb_server": srv.Addr, "start_server": "false",
	})
	require.NoError(t, err)
	defer imp.Close(context.Background())

	src, err := snapshot.NewSource(repo.AppContext(), imp)
	require.NoError(t, err)
	require.NoError(t, src.SetExcludes(nil))

	require.NoError(t, builder.Backup(src))
	require.NoError(t, builder.Commit())
	require.NoError(t, builder.Close())
	require.NoError(t, builder.Repository().RebuildState())

	snap, err := snapshot.Load(repo, builder.Header.Identifier)
	require.NoError(t, err)

	checkCache, err := repo.AppContext().GetCache().Check()
	require.NoError(t, err)
	snap.SetCheckCache(checkCache)
	return snap
}

func readSnapshotFile(t *testing.T, snap *snapshot.Snapshot, p string) []byte {
	t.Helper()
	pvfs, err := snap.Filesystem()
	require.NoError(t, err)
	f, err := pvfs.Open(p)
	require.NoError(t, err)
	defer f.Close()
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	return data
}

// TestRoundTrip drives the whole pipeline without plakar: three backups
// of a fake device through kloset (the second one unchanged, the third
// one with one modified, one added and one deleted file), then a restore
// of the last snapshot back onto the device.
func TestRoundTrip(t *testing.T) {
	srv := adbtest.New(t)
	dev := srv.AddDevice(&adbtest.Device{Serial: serial, Model: "Fake_Phone"})
	populate(dev)
	dev.FS.NoSymlinks = true
	dev.FS.Remove(root + "/Movies")
	repo := ptesting.GenerateRepository(t, nil, nil, nil)

	first := backup(t, repo, srv, nil)
	defer first.Close()
	require.Equal(t, 7, srv.Counters.RecvTotal(), "every regular file is read once")
	require.Equal(t, bigJPG, readSnapshotFile(t, first, root+"/DCIM/Camera/IMG_0001.jpg"))
	require.Equal(t, "unicode", string(readSnapshotFile(t, first, root+"/Download/fichier é 日本 'quote'.txt")))
	require.NoError(t, first.Check("/", &snapshot.CheckOptions{}))

	// Unchanged device: the VFS cache matches every file on its stable
	// metadata and nothing is transferred again.
	firstFS, err := first.FilesystemWithCache()
	require.NoError(t, err)
	second := backup(t, repo, srv, firstFS)
	defer second.Close()
	require.Equal(t, 7, srv.Counters.RecvTotal(), "unchanged files are not read again")
	require.Equal(t, bigJPG, readSnapshotFile(t, second, root+"/DCIM/Camera/IMG_0001.jpg"))

	// Modify, add, delete.
	dev.FS.WriteFile(root+"/DCIM/Camera/IMG_0002.jpg", []byte("second, edited"), 0o660, ts.Add(2*time.Hour))
	dev.FS.WriteFile(root+"/DCIM/Camera/IMG_0003.jpg", []byte("third"), 0o660, ts.Add(3*time.Hour))
	dev.FS.Remove(root + "/Download/.hidden")

	secondFS, err := second.FilesystemWithCache()
	require.NoError(t, err)
	third := backup(t, repo, srv, secondFS)
	defer third.Close()
	require.Equal(t, 9, srv.Counters.RecvTotal(), "only the modified and the new file are read")
	require.Equal(t, 2, srv.Counters.RecvCount(root+"/DCIM/Camera/IMG_0002.jpg"))
	require.Equal(t, 1, srv.Counters.RecvCount(root+"/DCIM/Camera/IMG_0003.jpg"))
	require.Equal(t, "second, edited", string(readSnapshotFile(t, third, root+"/DCIM/Camera/IMG_0002.jpg")))
	thirdFS, err := third.Filesystem()
	require.NoError(t, err)
	_, err = thirdFS.GetEntry(root + "/Download/.hidden")
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, third.Check("/", &snapshot.CheckOptions{}))

	// Restore the DCIM subtree of the last snapshot to the device.
	exp, err := exporter.NewExporter(repo.AppContext(), &integration.Options{Hostname: "test", MaxConcurrency: 4}, "android", map[string]string{
		"location": "android://" + serial + root + "/restore", "adb_server": srv.Addr, "start_server": "false",
	})
	require.NoError(t, err)
	defer exp.Close(context.Background())

	require.NoError(t, third.Export(exp, root+"/DCIM", &snapshot.ExportOptions{}))

	got, ok := dev.FS.ReadFile(root + "/restore/Camera/IMG_0001.jpg")
	require.True(t, ok)
	require.Equal(t, bigJPG, got)
	got, _ = dev.FS.ReadFile(root + "/restore/Camera/IMG_0002.jpg")
	require.Equal(t, "second, edited", string(got))
	got, _ = dev.FS.ReadFile(root + "/restore/Camera/IMG_0003.jpg")
	require.Equal(t, "third", string(got))
	n, e := dev.FS.Lstat(root + "/restore/Camera/IMG_0003.jpg")
	require.Zero(t, e)
	require.Equal(t, ts.Add(3*time.Hour).Unix(), n.ModTime.Unix(), "mtime is restored")
	require.Equal(t, []string{"Camera"}, dev.FS.Entries(root+"/restore"))

	// Restoring again over identical files is a no-op, not an error.
	require.Equal(t, 3, srv.Counters.SendCount(root+"/restore/Camera/IMG_0001.jpg")+srv.Counters.SendCount(root+"/restore/Camera/IMG_0002.jpg")+srv.Counters.SendCount(root+"/restore/Camera/IMG_0003.jpg"))
	require.NoError(t, third.Export(exp, root+"/DCIM", &snapshot.ExportOptions{}))
	require.Equal(t, 3, srv.Counters.SendCount(root+"/restore/Camera/IMG_0001.jpg")+srv.Counters.SendCount(root+"/restore/Camera/IMG_0002.jpg")+srv.Counters.SendCount(root+"/restore/Camera/IMG_0003.jpg"))
}

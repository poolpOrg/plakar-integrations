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
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/poolpOrg/plakar-integrations/android/adb"
	"github.com/poolpOrg/plakar-integrations/android/adb/adbtest"
	"github.com/poolpOrg/plakar-integrations/android/importer"
	"github.com/stretchr/testify/require"
)

const (
	serial = "FAKE0001"
	root   = "/storage/emulated/0"
)

var (
	ts     = time.Unix(1700000000, 0)
	bigJPG = bytes.Repeat([]byte("jpeg"), 100*1024) // 400 KiB, several chunks
)

// populate builds a representative shared storage.
func populate(d *adbtest.Device) {
	d.FS.MkdirAll(root+"/DCIM/Camera", ts)
	d.FS.WriteFile(root+"/DCIM/Camera/IMG_0001.jpg", bigJPG, 0o660, ts)
	d.FS.WriteFile(root+"/DCIM/Camera/IMG_0002.jpg", []byte("second"), 0o660, ts.Add(time.Hour))
	d.FS.MkdirAll(root+"/Pictures/empty", ts)
	d.FS.WriteFile(root+"/Download/empty.txt", nil, 0o660, ts)
	d.FS.WriteFile(root+"/Download/fichier é 日本 'quote'.txt", []byte("unicode"), 0o660, ts)
	d.FS.WriteFile(root+"/Download/with,comma and space.bin", []byte("comma"), 0o660, ts)
	d.FS.WriteFile(root+"/Download/.hidden", []byte("h"), 0o660, ts)
	d.FS.MkdirAll(root+"/Android/data/com.example/files", ts)
	d.FS.WriteFile(root+"/Android/data/com.example/files/cache.bin", []byte("private"), 0o660, ts)
	d.FS.Symlink(root+"/Movies", "DCIM/Camera", ts)
	d.FS.Symlink("/sdcard", root, ts)
}

type result struct {
	records map[string]*integration.Record
	errors  map[string]error
	content map[string][]byte
	meta    map[string]string
}

// run imports with imp and reads every regular file, like kloset would.
func run(t *testing.T, imp integration.Importer, readers bool) (*result, error) {
	t.Helper()
	ctx := context.Background()
	records := make(chan *integration.Record, 8)
	done := make(chan error, 1)
	var meta map[string]string
	go func() {
		m, err := imp.Import(ctx, records, nil)
		meta = m
		done <- err
	}()
	res := &result{
		records: map[string]*integration.Record{},
		errors:  map[string]error{},
		content: map[string][]byte{},
	}
	for rec := range records {
		if rec.Err != nil {
			res.errors[rec.Pathname] = rec.Err
			continue
		}
		res.records[rec.Pathname] = rec
		if readers && rec.FileInfo.Mode().IsRegular() {
			data, err := io.ReadAll(rec.Reader)
			if err != nil {
				res.errors[rec.Pathname] = err
			} else {
				res.content[rec.Pathname] = data
			}
			require.NoError(t, rec.Reader.Close())
		}
	}
	err := <-done
	res.meta = meta
	return res, err
}

func newImporter(t *testing.T, srv *adbtest.Server, location string, extra map[string]string, excludes ...string) integration.Importer {
	t.Helper()
	config := map[string]string{"location": location, "adb_server": srv.Addr, "start_server": "false"}
	for k, v := range extra {
		config[k] = v
	}
	opts := &integration.Options{Hostname: "test", MaxConcurrency: 4, Excludes: excludes}
	imp, err := importer.NewImporter(context.Background(), opts, "android", config)
	require.NoError(t, err)
	t.Cleanup(func() { imp.Close(context.Background()) })
	return imp
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestImportTree(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []string
	}{
		{"v2", adbtest.DefaultFeatures},
		{"v1", adbtest.LegacyFeatures},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := adbtest.New(t)
			dev := srv.AddDevice(&adbtest.Device{Serial: serial, Model: "Fake_Phone", Product: "fake", Name: "fakedev", Features: tc.features})
			populate(dev)

			imp := newImporter(t, srv, "android://", nil)
			require.Equal(t, root, imp.Root())
			require.Equal(t, serial, imp.Origin())
			require.Equal(t, "android", imp.Type())
			require.NoError(t, imp.Ping(context.Background()))

			res, err := run(t, imp, true)
			require.NoError(t, err)
			require.Empty(t, res.errors, "errors: %v", res.errors)
			require.Equal(t, map[string]string{"serial": serial, "model": "Fake_Phone", "product": "fake", "device": "fakedev"}, res.meta)

			require.Equal(t, []string{
				root,
				root + "/Android",
				root + "/Android/data",
				root + "/Android/data/com.example",
				root + "/Android/data/com.example/files",
				root + "/Android/data/com.example/files/cache.bin",
				root + "/DCIM",
				root + "/DCIM/Camera",
				root + "/DCIM/Camera/IMG_0001.jpg",
				root + "/DCIM/Camera/IMG_0002.jpg",
				root + "/Download",
				root + "/Download/.hidden",
				root + "/Download/empty.txt",
				root + "/Download/fichier é 日本 'quote'.txt",
				root + "/Download/with,comma and space.bin",
				root + "/Movies",
				root + "/Pictures",
				root + "/Pictures/empty",
			}, sortedKeys(res.records))

			require.Equal(t, bigJPG, res.content[root+"/DCIM/Camera/IMG_0001.jpg"])
			require.Equal(t, []byte{}, res.content[root+"/Download/empty.txt"])
			require.Equal(t, "unicode", string(res.content[root+"/Download/fichier é 日本 'quote'.txt"]))
			require.Equal(t, "comma", string(res.content[root+"/Download/with,comma and space.bin"]))

			rootRec := res.records[root]
			require.True(t, rootRec.FileInfo.IsDir())
			require.Equal(t, "0", rootRec.FileInfo.Lname)

			jpg := res.records[root+"/DCIM/Camera/IMG_0002.jpg"]
			require.Equal(t, "IMG_0002.jpg", jpg.FileInfo.Lname)
			require.True(t, jpg.FileInfo.Mode().IsRegular())
			require.Equal(t, fs.FileMode(0o660), jpg.FileInfo.Mode().Perm())
			require.Equal(t, ts.Add(time.Hour).Unix(), jpg.FileInfo.ModTime().Unix())
			if tc.name == "v2" {
				require.EqualValues(t, 6, jpg.FileInfo.Size())
				require.NotZero(t, jpg.FileInfo.Ino())
				require.EqualValues(t, 1, jpg.FileInfo.Dev())
				require.EqualValues(t, 1, jpg.FileInfo.Nlink())
			} else {
				require.EqualValues(t, -1, jpg.FileInfo.Size(), "v1 sizes are untrusted")
			}

			lnk := res.records[root+"/Movies"]
			require.NotZero(t, lnk.FileInfo.Mode()&os.ModeSymlink)
			require.Equal(t, "DCIM/Camera", lnk.Target)

			empty := res.records[root+"/Pictures/empty"]
			require.True(t, empty.FileInfo.IsDir())
			require.Equal(t, "empty", empty.FileInfo.Lname)

			// Every regular file was pulled exactly once.
			require.Equal(t, 7, srv.Counters.RecvTotal())
			require.Equal(t, 1, srv.Counters.RecvCount(root+"/DCIM/Camera/IMG_0001.jpg"))
		})
	}
}

func TestImportExcludes(t *testing.T) {
	srv := adbtest.New(t)
	populate(srv.AddDevice(&adbtest.Device{Serial: serial}))

	imp := newImporter(t, srv, "android://"+serial, nil, "**/Android/data", "*.bin")
	res, err := run(t, imp, true)
	require.NoError(t, err)
	require.Empty(t, res.errors)
	for p := range res.records {
		require.NotContains(t, p, "/Android/data", p)
		require.False(t, strings.HasSuffix(p, ".bin"), p)
	}
	require.Contains(t, res.records, root+"/Android")
	require.Contains(t, res.records, root+"/DCIM/Camera/IMG_0001.jpg")
	require.Zero(t, srv.Counters.List[root+"/Android/data"], "excluded directories are not listed")
}

func TestImportSubpath(t *testing.T) {
	srv := adbtest.New(t)
	populate(srv.AddDevice(&adbtest.Device{Serial: serial}))

	imp := newImporter(t, srv, "android://"+serial+root+"/DCIM", nil)
	require.Equal(t, root+"/DCIM", imp.Root())
	res, err := run(t, imp, false)
	require.NoError(t, err)
	require.Equal(t, []string{root + "/DCIM", root + "/DCIM/Camera", root + "/DCIM/Camera/IMG_0001.jpg", root + "/DCIM/Camera/IMG_0002.jpg"}, sortedKeys(res.records))

	// A symlinked root (/sdcard) keeps its name.
	imp = newImporter(t, srv, "android:///sdcard/Download", map[string]string{"serial": serial})
	res, err = run(t, imp, false)
	require.NoError(t, err)
	require.Contains(t, res.records, "/sdcard/Download/empty.txt")

	// A single file.
	imp = newImporter(t, srv, "android://", map[string]string{"source_path": root + "/Download/empty.txt"})
	res, err = run(t, imp, true)
	require.NoError(t, err)
	require.Equal(t, []string{root + "/Download/empty.txt"}, sortedKeys(res.records))

	// A missing path fails at construction.
	_, err = importer.NewImporter(context.Background(), &integration.Options{}, "android", map[string]string{
		"location": "android://" + serial + "/storage/emulated/0/nope", "adb_server": srv.Addr, "start_server": "false",
	})
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestImportPermissionErrors(t *testing.T) {
	srv := adbtest.New(t)
	dev := srv.AddDevice(&adbtest.Device{Serial: serial})
	populate(dev)
	n, _ := dev.FS.Lstat(root + "/Android/data")
	n.Unreadable = true
	n, _ = dev.FS.Lstat(root + "/Download/.hidden")
	n.StatErrno = 13

	imp := newImporter(t, srv, "android://", nil)
	res, err := run(t, imp, true)
	require.NoError(t, err, "per-entry failures do not abort the import")

	require.ErrorIs(t, res.errors[root+"/Android/data"], fs.ErrPermission)
	require.Contains(t, res.records, root+"/Android/data", "the directory itself is recorded")
	require.NotContains(t, res.records, root+"/Android/data/com.example")
	require.ErrorIs(t, res.errors[root+"/Download/.hidden"], fs.ErrPermission)
	require.NotContains(t, res.records, root+"/Download/.hidden")
	require.Contains(t, res.records, root+"/Download/empty.txt", "the rest of the directory is still imported")
	require.Len(t, res.errors, 2)
}

func TestImportReadErrors(t *testing.T) {
	srv := adbtest.New(t)
	dev := srv.AddDevice(&adbtest.Device{Serial: serial})
	populate(dev)
	n, _ := dev.FS.Lstat(root + "/Download/with,comma and space.bin")
	n.ReadError = "Input/output error"
	dev.DropRecvAfter = map[string]int64{root + "/DCIM/Camera/IMG_0001.jpg": 100 * 1024}

	imp := newImporter(t, srv, "android://", nil)
	res, err := run(t, imp, true)
	require.NoError(t, err)
	require.Len(t, res.errors, 2)
	require.Contains(t, res.errors[root+"/Download/with,comma and space.bin"].Error(), "Input/output error")
	require.Contains(t, res.errors[root+"/DCIM/Camera/IMG_0001.jpg"].Error(), "IMG_0001.jpg")
	require.NotContains(t, res.content, root+"/DCIM/Camera/IMG_0001.jpg")
	require.Equal(t, "second", string(res.content[root+"/DCIM/Camera/IMG_0002.jpg"]), "other files still stream")
}

func TestImportSizeChanged(t *testing.T) {
	srv := adbtest.New(t)
	dev := srv.AddDevice(&adbtest.Device{Serial: serial})
	populate(dev)

	imp := newImporter(t, srv, "android://", nil)
	res, err := run(t, imp, false)
	require.NoError(t, err)

	// The file grows between enumeration and read, as a camera app
	// still writing would do.
	dev.FS.WriteFile(root+"/DCIM/Camera/IMG_0002.jpg", []byte("second, longer"), 0o660, ts)
	rec := res.records[root+"/DCIM/Camera/IMG_0002.jpg"]
	_, err = io.ReadAll(rec.Reader)
	require.Error(t, err)
	require.Contains(t, err.Error(), "size changed during backup")
	require.NoError(t, rec.Reader.Close())
}

func TestImportDisconnectDuringListing(t *testing.T) {
	srv := adbtest.New(t)
	dev := srv.AddDevice(&adbtest.Device{Serial: serial})
	populate(dev)
	dev.DropOnList = map[string]bool{root + "/Download": true}

	imp := newImporter(t, srv, "android://", nil)
	_, err := run(t, imp, false)
	require.Error(t, err, "a dead listing stream aborts the import")
	require.Contains(t, err.Error(), "listing "+root+"/Download")
}

func TestImportDeviceGone(t *testing.T) {
	srv := adbtest.New(t)
	populate(srv.AddDevice(&adbtest.Device{Serial: serial}))

	imp := newImporter(t, srv, "android://", nil)
	srv.RemoveDevice(serial)

	require.Error(t, imp.Ping(context.Background()))
	_, err := run(t, imp, false)
	require.Error(t, err)
	var aerr *adb.Error
	require.ErrorAs(t, err, &aerr)
	require.Contains(t, aerr.Message, "not found")
}

func TestImportCancellation(t *testing.T) {
	srv := adbtest.New(t)
	dev := srv.AddDevice(&adbtest.Device{Serial: serial})
	for i := range 50 {
		dev.FS.WriteFile(root+"/Download/f"+string(rune('a'+i%26))+string(rune('a'+i/26)), []byte("x"), 0o660, ts)
	}

	imp := newImporter(t, srv, "android://", nil)
	ctx, cancel := context.WithCancel(context.Background())
	records := make(chan *integration.Record) // unbuffered: Import blocks on us
	done := make(chan error, 1)
	go func() {
		_, err := imp.Import(ctx, records, nil)
		done <- err
	}()
	<-records
	<-records
	cancel()
	for range records {
	}
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestImportDeviceSelection(t *testing.T) {
	srv := adbtest.New(t)
	populate(srv.AddDevice(&adbtest.Device{Serial: "AAA1", Model: "One"}))
	populate(srv.AddDevice(&adbtest.Device{Serial: "BBB2", Model: "Two"}))
	srv.AddDevice(&adbtest.Device{Serial: "CCC3", State: adb.StateUnauthorized})

	opts := &integration.Options{}
	mk := func(location string) error {
		_, err := importer.NewImporter(context.Background(), opts, "android", map[string]string{
			"location": location, "adb_server": srv.Addr, "start_server": "false",
		})
		return err
	}

	err := mk("android://")
	require.Error(t, err)
	require.Contains(t, err.Error(), "3 devices connected")
	require.Contains(t, err.Error(), "AAA1 (One) [device]")
	require.Contains(t, err.Error(), "CCC3 [unauthorized]")

	require.NoError(t, mk("android://BBB2"))

	err = mk("android://CCC3")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not authorized")

	err = mk("android://ZZZ9")
	require.Error(t, err)
	require.Contains(t, err.Error(), "device ZZZ9 not found")

	srv.SetState("BBB2", adb.StateOffline)
	err = mk("android://BBB2")
	require.Error(t, err)
	require.Contains(t, err.Error(), "offline")
}

func TestImportNoDevice(t *testing.T) {
	srv := adbtest.New(t)
	_, err := importer.NewImporter(context.Background(), &integration.Options{}, "android", map[string]string{
		"location": "android://", "adb_server": srv.Addr, "start_server": "false",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no Android device connected")
	require.Contains(t, err.Error(), "USB debugging")
}

func TestImportSymlinkReadlinkFailure(t *testing.T) {
	srv := adbtest.New(t)
	dev := srv.AddDevice(&adbtest.Device{Serial: serial, ShellError: "sh: readlink: not found"})
	populate(dev)

	imp := newImporter(t, srv, "android://", nil)
	res, err := run(t, imp, false)
	require.NoError(t, err)
	require.Contains(t, res.errors[root+"/Movies"].Error(), "readlink")
	require.NotContains(t, res.records, root+"/Movies")
	require.Contains(t, res.records, root+"/DCIM/Camera/IMG_0001.jpg")
}

func TestImportStreamsBound(t *testing.T) {
	srv := adbtest.New(t)
	dev := srv.AddDevice(&adbtest.Device{Serial: serial})
	for i := range 20 {
		dev.FS.WriteFile(root+"/Download/f"+string(rune('a'+i)), bytes.Repeat([]byte{byte(i)}, 10000), 0o660, ts)
	}
	imp := newImporter(t, srv, "android://", map[string]string{"streams": "2"})
	res, err := run(t, imp, false)
	require.NoError(t, err)

	// Open every reader without closing: only two sessions exist, the
	// rest wait; reading and closing them one by one drains the queue.
	var readers []io.ReadCloser
	for _, p := range sortedKeys(res.records) {
		rec := res.records[p]
		if rec.FileInfo.Mode().IsRegular() {
			readers = append(readers, rec.Reader)
		}
	}
	require.Len(t, readers, 20)
	for i, r := range readers {
		data, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Len(t, data, 10000)
		require.Equal(t, byte(i), data[0])
		require.NoError(t, r.Close())
	}
}

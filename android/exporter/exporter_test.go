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

package exporter_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/poolpOrg/plakar-integrations/android/adb"
	"github.com/poolpOrg/plakar-integrations/android/adb/adbtest"
	"github.com/poolpOrg/plakar-integrations/android/exporter"
	"github.com/stretchr/testify/require"
)

const (
	serial = "FAKE0001"
	root   = "/storage/emulated/0"
)

var ts = time.Unix(1600000000, 0)

func newDevice(t *testing.T) (*adbtest.Server, *adbtest.Device) {
	t.Helper()
	srv := adbtest.New(t)
	dev := srv.AddDevice(&adbtest.Device{Serial: serial})
	dev.FS.MkdirAll(root+"/DCIM", ts)
	dev.FS.NoSymlinks = true
	return srv, dev
}

func newExporter(t *testing.T, srv *adbtest.Server, location string, extra map[string]string) integration.Exporter {
	t.Helper()
	exp, err := tryExporter(srv, location, extra)
	require.NoError(t, err)
	t.Cleanup(func() { exp.Close(context.Background()) })
	return exp
}

func tryExporter(srv *adbtest.Server, location string, extra map[string]string) (integration.Exporter, error) {
	config := map[string]string{"location": location, "adb_server": srv.Addr, "start_server": "false"}
	for k, v := range extra {
		config[k] = v
	}
	opts := &integration.Options{Hostname: "test", MaxConcurrency: 4}
	return exporter.NewExporter(context.Background(), opts, "android", config)
}

func dir(p string) *integration.Record {
	name := p[strings.LastIndex(p, "/")+1:]
	if p == "/" {
		name = "/"
	}
	return integration.NewRecord(p, "", objects.FileInfo{Lname: name, Lmode: os.ModeDir | 0o755, LmodTime: ts}, nil, nil)
}

func file(p string, content []byte, mtime time.Time) *integration.Record {
	name := p[strings.LastIndex(p, "/")+1:]
	return integration.NewRecord(p, "", objects.FileInfo{Lname: name, Lmode: 0o644, Lsize: int64(len(content)), LmodTime: mtime}, nil,
		func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(content)), nil })
}

func symlink(p, target string) *integration.Record {
	name := p[strings.LastIndex(p, "/")+1:]
	return integration.NewRecord(p, target, objects.FileInfo{Lname: name, Lmode: os.ModeSymlink | 0o777, LmodTime: ts}, nil, nil)
}

// export runs the records through exp and returns the per-path errors.
func export(t *testing.T, exp integration.Exporter, recs ...*integration.Record) (map[string]error, error) {
	t.Helper()
	records := make(chan *integration.Record)
	results := make(chan *integration.Result, len(recs)+1)
	done := make(chan error, 1)
	go func() { done <- exp.Export(context.Background(), nil, records, results) }()
	for _, r := range recs {
		records <- r
	}
	close(records)
	errs := map[string]error{}
	count := 0
	for res := range results {
		count++
		if res.Err != nil {
			errs[res.Record.Pathname] = res.Err
		}
	}
	require.Equal(t, len(recs), count, "every record gets a result")
	return errs, <-done
}

func TestExportTree(t *testing.T) {
	srv, dev := newDevice(t)
	big := bytes.Repeat([]byte("v"), 3*adb.SyncDataMax+1)

	exp := newExporter(t, srv, "android://"+serial+root+"/restore", nil)
	require.Equal(t, root+"/restore", exp.Root())
	require.Equal(t, serial, exp.Origin())
	require.Equal(t, "android", exp.Type())
	require.NoError(t, exp.Ping(context.Background()))

	errs, err := export(t, exp,
		dir("/"),
		dir("/DCIM"),
		dir("/DCIM/Camera"),
		file("/DCIM/Camera/VID_0001.mp4", big, ts.Add(time.Hour)),
		file("/DCIM/Camera/IMG_0002.jpg", []byte("second"), ts),
		dir("/Pictures"),
		dir("/Pictures/empty"),
		file("/Download/fichier é 日本 'quote'.txt", []byte("unicode"), ts),
		file("/Download/empty.txt", nil, ts),
		integration.NewError("/broken", io.ErrUnexpectedEOF),
	)
	require.NoError(t, err)
	require.Empty(t, errs)

	require.Equal(t, []string{
		"/storage", "/storage/emulated", "/storage/emulated/0", "/storage/emulated/0/DCIM",
		root + "/restore",
		root + "/restore/DCIM",
		root + "/restore/DCIM/Camera",
		root + "/restore/DCIM/Camera/IMG_0002.jpg",
		root + "/restore/DCIM/Camera/VID_0001.mp4",
		root + "/restore/Download",
		root + "/restore/Download/empty.txt",
		root + "/restore/Download/fichier é 日本 'quote'.txt",
		root + "/restore/Pictures",
		root + "/restore/Pictures/empty",
	}, dev.FS.Paths())

	got, _ := dev.FS.ReadFile(root + "/restore/DCIM/Camera/VID_0001.mp4")
	require.Equal(t, big, got)
	n, _ := dev.FS.Lstat(root + "/restore/DCIM/Camera/VID_0001.mp4")
	require.Equal(t, ts.Add(time.Hour).Unix(), n.ModTime.Unix())
	require.EqualValues(t, adb.S_IFREG|0o644, n.Mode)
	got, _ = dev.FS.ReadFile(root + "/restore/Download/empty.txt")
	require.Empty(t, got)

	// Directories were created with few shell commands, all quoted.
	cmds := srv.Counters.ShellCommands()
	require.Len(t, cmds, 2, "one mkdir at construction, one batch at the end")
	require.True(t, strings.HasPrefix(cmds[1], "'mkdir' '-p' '--' '"), cmds[1])
}

func TestExportDefaultDestination(t *testing.T) {
	srv, dev := newDevice(t)
	exp := newExporter(t, srv, "android://", nil)
	require.True(t, strings.HasPrefix(exp.Root(), root+"/plakar-restore-"), exp.Root())

	errs, err := export(t, exp, dir("/"), file("/a.txt", []byte("a"), ts))
	require.NoError(t, err)
	require.Empty(t, errs)
	got, ok := dev.FS.ReadFile(exp.Root() + "/a.txt")
	require.True(t, ok)
	require.Equal(t, "a", string(got))
}

func TestExportDestinationChecks(t *testing.T) {
	srv, dev := newDevice(t)

	_, err := tryExporter(srv, "android://"+serial+"/system/app", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside shared storage")

	_, err = tryExporter(srv, "android://"+serial+"/", nil)
	require.Error(t, err)

	_, err = tryExporter(srv, "android://"+serial+"/data/local/tmp/x", nil)
	require.NoError(t, err)

	exp, err := tryExporter(srv, "android://"+serial+"/data/x", map[string]string{"allow_any_path": "true"})
	require.NoError(t, err)
	require.Equal(t, "/data/x", exp.Root())

	dev.FS.WriteFile(root+"/afile", []byte("x"), 0o660, ts)
	_, err = tryExporter(srv, "android://"+serial+root+"/afile", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a directory")

	// destination_path overrides the location path.
	exp = newExporter(t, srv, "android://"+serial+"/sdcard/one", map[string]string{"destination_path": root + "/two"})
	require.Equal(t, root+"/two", exp.Root())
	_, e := dev.FS.Lstat(root + "/two")
	require.Zero(t, e, "the destination is created up front")
}

func TestExportPathTraversal(t *testing.T) {
	srv, dev := newDevice(t)
	exp := newExporter(t, srv, "android://"+serial+root+"/restore", nil)

	errs, err := export(t, exp,
		dir("/"),
		file("/../escape.txt", []byte("x"), ts),
		file("/a/../../escape2.txt", []byte("x"), ts),
		file("/a//b.txt", []byte("x"), ts),
		file("ok.txt", []byte("x"), ts),
		file("/"+strings.Repeat("n", adb.MaxPathLength), []byte("x"), ts),
		file("/good.txt", []byte("x"), ts),
	)
	require.NoError(t, err)
	require.Len(t, errs, 5)
	for p, e := range errs {
		require.True(t, strings.Contains(e.Error(), "not clean") || strings.Contains(e.Error(), "too long"), "%s: %v", p, e)
	}
	_, ok := dev.FS.ReadFile(root + "/restore/good.txt")
	require.True(t, ok)
	require.Equal(t, []string{"/storage", "/storage/emulated", "/storage/emulated/0", "/storage/emulated/0/DCIM", root + "/restore", root + "/restore/good.txt"}, dev.FS.Paths())
}

func TestExportOverwrite(t *testing.T) {
	srv, dev := newDevice(t)
	dev.FS.WriteFile(root+"/restore/same.txt", []byte("same"), 0o660, ts)
	dev.FS.WriteFile(root+"/restore/other.txt", []byte("device version"), 0o660, ts)
	dev.FS.MkdirAll(root+"/restore/isdir", ts)

	exp := newExporter(t, srv, "android://"+serial+root+"/restore", nil)
	errs, err := export(t, exp,
		dir("/"),
		file("/same.txt", []byte("SAME"), ts),
		file("/other.txt", []byte("snapshot version"), ts),
		file("/isdir", []byte("x"), ts),
		file("/new.txt", []byte("new"), ts),
	)
	require.NoError(t, err)
	require.Len(t, errs, 2)
	require.Contains(t, errs["/other.txt"].Error(), "already exists")
	require.Contains(t, errs["/other.txt"].Error(), "overwrite=true")
	require.Contains(t, errs["/isdir"].Error(), "is a directory")

	got, _ := dev.FS.ReadFile(root + "/restore/same.txt")
	require.Equal(t, "same", string(got), "same size and mtime: left untouched")
	got, _ = dev.FS.ReadFile(root + "/restore/other.txt")
	require.Equal(t, "device version", string(got))
	got, _ = dev.FS.ReadFile(root + "/restore/new.txt")
	require.Equal(t, "new", string(got))
	require.Zero(t, srv.Counters.SendCount(root+"/restore/same.txt"))
	require.Zero(t, srv.Counters.SendCount(root+"/restore/other.txt"))

	exp = newExporter(t, srv, "android://"+serial+root+"/restore", map[string]string{"overwrite": "true"})
	errs, err = export(t, exp, dir("/"), file("/other.txt", []byte("snapshot version"), ts))
	require.NoError(t, err)
	require.Empty(t, errs)
	got, _ = dev.FS.ReadFile(root + "/restore/other.txt")
	require.Equal(t, "snapshot version", string(got))
}

func TestExportSymlinks(t *testing.T) {
	srv, dev := newDevice(t)
	exp := newExporter(t, srv, "android://"+serial+root+"/restore", nil)

	errs, err := export(t, exp, dir("/"), symlink("/lnk", "DCIM"), file("/f", []byte("f"), ts))
	require.NoError(t, err)
	require.Len(t, errs, 1)
	require.Contains(t, errs["/lnk"].Error(), "symlink failed", "shared storage refuses symlinks")

	dev.FS.NoSymlinks = false
	errs, err = export(t, exp, symlink("/lnk", "DCIM"))
	require.NoError(t, err)
	require.Empty(t, errs)
	n, e := dev.FS.Lstat(root + "/restore/lnk")
	require.Zero(t, e)
	require.Equal(t, "DCIM", n.Target)
}

func TestExportPartialFailures(t *testing.T) {
	srv, dev := newDevice(t)
	dev.FailSend = map[string]string{root + "/restore/denied.txt": "couldn't create file: Permission denied"}

	exp := newExporter(t, srv, "android://"+serial+root+"/restore", nil)
	errs, err := export(t, exp,
		dir("/"),
		file("/denied.txt", []byte("x"), ts),
		file("/ok.txt", []byte("x"), ts),
		integration.NewRecord("/fifo", "", objects.FileInfo{Lname: "fifo", Lmode: os.ModeNamedPipe | 0o644}, nil, nil),
	)
	require.NoError(t, err, "partial failures are per record")
	require.Len(t, errs, 2)
	require.Contains(t, errs["/denied.txt"].Error(), "Permission denied")
	require.Contains(t, errs["/fifo"].Error(), "cannot restore a pipe")
	_, ok := dev.FS.ReadFile(root + "/restore/ok.txt")
	require.True(t, ok)
}

func TestExportMkdirFailure(t *testing.T) {
	srv, dev := newDevice(t)
	exp := newExporter(t, srv, "android://"+serial+root+"/restore", nil)
	dev.ShellError = "mkdir: Permission denied"

	_, err := export(t, exp, dir("/"), dir("/empty"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "Permission denied")
}

func TestExportDeviceGone(t *testing.T) {
	srv, _ := newDevice(t)
	exp := newExporter(t, srv, "android://"+serial+root+"/restore", nil)
	srv.RemoveDevice(serial)

	require.Error(t, exp.Ping(context.Background()))
	errs, err := export(t, exp, dir("/"), file("/f", []byte("f"), ts))
	require.Error(t, err)
	require.Contains(t, errs["/f"].Error(), "not found")
}

func TestExportCancellation(t *testing.T) {
	srv, _ := newDevice(t)
	exp := newExporter(t, srv, "android://"+serial+root+"/restore", nil)

	ctx, cancel := context.WithCancel(context.Background())
	records := make(chan *integration.Record)
	results := make(chan *integration.Result, 16)
	done := make(chan error, 1)
	go func() { done <- exp.Export(ctx, nil, records, results) }()
	records <- dir("/")
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

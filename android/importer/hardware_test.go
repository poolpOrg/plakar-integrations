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

// Hardware tests run against a real device selected with
// PLAKAR_ANDROID_TEST_SERIAL (see "make hwtest SERIAL=...").  They only
// read the shared storage and write under a directory of their own,
// <storage>/Download/plakar-hwtest-<timestamp>, which they delete.

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/poolpOrg/plakar-integrations/android/common"
	"github.com/poolpOrg/plakar-integrations/android/exporter"
	"github.com/poolpOrg/plakar-integrations/android/importer"
	"github.com/stretchr/testify/require"
)

const hwTestPrefix = "plakar-hwtest-"

func hardwareSerial(t *testing.T) string {
	t.Helper()
	serial := os.Getenv("PLAKAR_ANDROID_TEST_SERIAL")
	if serial == "" {
		t.Skip("set PLAKAR_ANDROID_TEST_SERIAL to the adb serial of a test device")
	}
	return serial
}

func hardwareConfig(serial, location string, extra map[string]string) map[string]string {
	config := map[string]string{"location": location}
	for k, v := range extra {
		config[k] = v
	}
	if config["location"] == "" {
		config["location"] = "android://" + serial
	}
	return config
}

func TestHardwareDiscovery(t *testing.T) {
	serial := hardwareSerial(t)
	ctx := context.Background()

	cfg, err := common.ParseConfig("android", map[string]string{"location": "android://" + serial}, "source_path")
	require.NoError(t, err)

	start := time.Now()
	dev, err := common.Connect(ctx, cfg)
	require.NoError(t, err)
	t.Logf("discovery: %s in %s, features %v", dev.Info.Describe(), time.Since(start), dev.Features)
	require.NoError(t, dev.Ping(ctx))

	s, err := dev.Client.Sync(ctx, dev.Serial, dev.Features)
	require.NoError(t, err)
	defer s.Close()
	root, st, err := common.ResolveRoot(s, "")
	require.NoError(t, err)
	require.True(t, st.IsDir())
	t.Logf("shared storage: %s", root)
}

// TestHardwareEnumerate walks the whole shared storage without reading
// contents and reports the throughput.
func TestHardwareEnumerate(t *testing.T) {
	serial := hardwareSerial(t)
	ctx := context.Background()

	imp, err := importer.NewImporter(ctx, &integration.Options{Hostname: "hwtest", MaxConcurrency: 4}, "android", hardwareConfig(serial, "", nil))
	require.NoError(t, err)
	defer imp.Close(ctx)

	records := make(chan *integration.Record, 64)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := imp.Import(ctx, records, nil)
		done <- err
	}()
	var files, dirs, errs int
	var size int64
	for rec := range records {
		switch {
		case rec.Err != nil:
			errs++
			t.Logf("error: %s: %v", rec.Pathname, rec.Err)
		case rec.FileInfo.IsDir():
			dirs++
		default:
			files++
			if rec.FileInfo.Mode().IsRegular() {
				size += rec.FileInfo.Size()
				require.NoError(t, rec.Reader.Close())
			}
		}
	}
	require.NoError(t, <-done)
	elapsed := time.Since(start)
	t.Logf("enumerated %d files (%d bytes) and %d directories, %d errors, in %s (%.0f entries/s)",
		files, size, dirs, errs, elapsed, float64(files+dirs)/elapsed.Seconds())
}

// TestHardwareRoundTrip writes a small tree to the device through the
// exporter, reads it back through the importer and compares.
func TestHardwareRoundTrip(t *testing.T) {
	serial := hardwareSerial(t)
	ctx := context.Background()
	opts := &integration.Options{Hostname: "hwtest", MaxConcurrency: 4}

	cfg, err := common.ParseConfig("android", map[string]string{"location": "android://" + serial}, "source_path")
	require.NoError(t, err)
	dev, err := common.Connect(ctx, cfg)
	require.NoError(t, err)
	s, err := dev.Client.Sync(ctx, dev.Serial, dev.Features)
	require.NoError(t, err)
	root, _, err := common.ResolveRoot(s, "")
	s.Close()
	require.NoError(t, err)

	testDir := root + "/Download/" + hwTestPrefix + time.Now().Format("20060102150405")
	t.Cleanup(func() {
		require.True(t, strings.Contains(testDir, "/"+hwTestPrefix), "refusing to delete %s", testDir)
		res, err := dev.Client.Shell(ctx, dev.Serial, dev.Features, "rm", "-rf", "--", testDir)
		if err != nil || res.ExitCode != 0 {
			t.Logf("cleanup of %s failed, remove it by hand: %v %s", testDir, err, res.Stderr)
		}
	})

	big := make([]byte, 4*1024*1024+123)
	_, err = rand.Read(big)
	require.NoError(t, err)
	mtime := time.Unix(1700000000, 0)
	want := map[string][]byte{
		"/big.bin":                  big,
		"/empty":                    {},
		"/sub/fichier é 日本 'q'.txt": []byte("unicode"),
		"/sub/with,comma space.txt": []byte("comma"),
	}

	exp, err := exporter.NewExporter(ctx, opts, "android", hardwareConfig(serial, "android://"+serial+testDir, nil))
	require.NoError(t, err)
	defer exp.Close(ctx)

	records := make(chan *integration.Record)
	results := make(chan *integration.Result, 16)
	done := make(chan error, 1)
	go func() { done <- exp.Export(ctx, nil, records, results) }()
	records <- integration.NewRecord("/", "", objects.FileInfo{Lname: "/", Lmode: os.ModeDir | 0o755, LmodTime: mtime}, nil, nil)
	records <- integration.NewRecord("/sub", "", objects.FileInfo{Lname: "sub", Lmode: os.ModeDir | 0o755, LmodTime: mtime}, nil, nil)
	records <- integration.NewRecord("/emptydir", "", objects.FileInfo{Lname: "emptydir", Lmode: os.ModeDir | 0o755, LmodTime: mtime}, nil, nil)
	for p, content := range want {
		name := p[strings.LastIndex(p, "/")+1:]
		records <- integration.NewRecord(p, "", objects.FileInfo{Lname: name, Lmode: 0o644, Lsize: int64(len(content)), LmodTime: mtime}, nil,
			func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(content)), nil })
	}
	close(records)
	for res := range results {
		require.NoError(t, res.Err, res.Record.Pathname)
	}
	require.NoError(t, <-done)

	imp, err := importer.NewImporter(ctx, opts, "android", hardwareConfig(serial, "android://"+serial+testDir, nil))
	require.NoError(t, err)
	defer imp.Close(ctx)
	in := make(chan *integration.Record, 16)
	go func() {
		_, err := imp.Import(ctx, in, nil)
		done <- err
	}()
	got := map[string][]byte{}
	var sawEmptyDir bool
	for rec := range in {
		require.NoError(t, rec.Err, rec.Pathname)
		rel := strings.TrimPrefix(rec.Pathname, testDir)
		if rel == "/emptydir" && rec.FileInfo.IsDir() {
			sawEmptyDir = true
		}
		if !rec.FileInfo.Mode().IsRegular() {
			continue
		}
		data, err := io.ReadAll(rec.Reader)
		require.NoError(t, err, rec.Pathname)
		require.NoError(t, rec.Reader.Close())
		got[rel] = data
		require.Equal(t, mtime.Unix(), rec.FileInfo.ModTime().Unix(), "mtime of %s", rel)
	}
	require.NoError(t, <-done)
	require.True(t, sawEmptyDir, "empty directories are restored and enumerated")
	require.Len(t, got, len(want))
	for p, content := range want {
		require.Equal(t, content, got[p], p)
	}
}

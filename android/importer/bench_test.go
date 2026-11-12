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

// These benchmarks measure the protocol and importer overhead against the
// in-memory fake adbd over loopback.  They say nothing about USB: real
// numbers come from a device (see README.md, "Performance").

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/poolpOrg/plakar-integrations/android/adb/adbtest"
	"github.com/poolpOrg/plakar-integrations/android/importer"
)

func benchDevice(b *testing.B, files int, size int) *adbtest.Server {
	srv := adbtest.New(b)
	dev := srv.AddDevice(&adbtest.Device{Serial: serial})
	content := bytes.Repeat([]byte("b"), size)
	for i := range files {
		dev.FS.WriteFile(fmt.Sprintf("%s/d%03d/f%05d.bin", root, i%100, i), content, 0o660, ts)
	}
	return srv
}

func benchImport(b *testing.B, srv *adbtest.Server, read bool) (entries, bytes int) {
	ctx := context.Background()
	imp, err := importer.NewImporter(ctx, &integration.Options{MaxConcurrency: 4}, "android", map[string]string{
		"location": "android://" + serial, "adb_server": srv.Addr, "start_server": "false",
	})
	if err != nil {
		b.Fatal(err)
	}
	defer imp.Close(ctx)
	records := make(chan *integration.Record, 64)
	done := make(chan error, 1)
	go func() {
		_, err := imp.Import(ctx, records, nil)
		done <- err
	}()
	for rec := range records {
		entries++
		if rec.Err == nil && read && rec.FileInfo.Mode().IsRegular() {
			n, err := io.Copy(io.Discard, rec.Reader)
			if err != nil {
				b.Fatal(err)
			}
			rec.Reader.Close()
			bytes += int(n)
		}
	}
	if err := <-done; err != nil {
		b.Fatal(err)
	}
	return entries, bytes
}

func BenchmarkEnumerate10kFiles(b *testing.B) {
	srv := benchDevice(b, 10000, 16)
	b.ResetTimer()
	for range b.N {
		entries, _ := benchImport(b, srv, false)
		b.ReportMetric(float64(entries)/b.Elapsed().Seconds()*float64(b.N), "entries/s")
	}
}

func BenchmarkSmallFiles10k(b *testing.B) {
	srv := benchDevice(b, 10000, 4096)
	b.ResetTimer()
	for range b.N {
		_, n := benchImport(b, srv, true)
		b.SetBytes(int64(n))
	}
}

func BenchmarkLargeFiles(b *testing.B) {
	srv := benchDevice(b, 4, 64*1024*1024)
	b.ResetTimer()
	for range b.N {
		_, n := benchImport(b, srv, true)
		b.SetBytes(int64(n))
	}
}

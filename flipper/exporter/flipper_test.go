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

package exporter

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/poolpOrg/plakar-integrations/flipper/device"
	"github.com/stretchr/testify/require"
)

func record(pathname string, mode os.FileMode, content string) *integration.Record {
	r := &integration.Record{
		Pathname: pathname,
		FileInfo: integration.NewFileInfo(pathname, int64(len(content)), mode, epochTime(), 0, 0, 0, 0, 1),
	}
	if mode.IsRegular() {
		r.Reader = io.NopCloser(strings.NewReader(content))
	}
	return r
}

func run(t *testing.T, exp *Exporter, records ...*integration.Record) map[string]*integration.Result {
	t.Helper()

	in := make(chan *integration.Record, len(records))
	for _, r := range records {
		in <- r
	}
	close(in)

	out := make(chan *integration.Result, len(records))
	require.NoError(t, exp.Export(context.Background(), in, out))

	got := map[string]*integration.Result{}
	for r := range out {
		got[r.Record.Pathname] = r
	}
	return got
}

func TestExport(t *testing.T) {
	f := device.NewFake("Lc4str1g1")
	exp := New(&integration.Options{}, device.Location{Path: "/ext/restore"}, f)
	require.Equal(t, "Lc4str1g1", exp.Origin())
	require.Equal(t, "/ext/restore", exp.Root())

	got := run(t, exp,
		record("/", os.ModeDir|0755, ""),
		record("/nfc", os.ModeDir|0755, ""),
		record("/nfc/a.nfc", 0644, "aaa"),
		record("/nfc/deep/b.nfc", 0644, "bbb"),
		record("/nfc/link", os.ModeSymlink|0777, ""),
		record("/nfc/fifo", os.ModeNamedPipe|0644, ""),
	)

	require.NoError(t, got["/nfc"].Err)
	require.NoError(t, got["/nfc/a.nfc"].Err)
	require.NoError(t, got["/nfc/deep/b.nfc"].Err)
	require.Error(t, got["/nfc/link"].Err)
	require.NoError(t, got["/nfc/fifo"].Err)

	data, err := f.Read("/ext/restore/nfc/a.nfc")
	require.NoError(t, err)
	require.Equal(t, "aaa", string(data))

	data, err = f.Read("/ext/restore/nfc/deep/b.nfc")
	require.NoError(t, err)
	require.Equal(t, "bbb", string(data))

	st, err := f.Stat("/ext/restore/nfc/deep")
	require.NoError(t, err)
	require.True(t, st.IsDir)
}

func TestExportOverwrite(t *testing.T) {
	f := device.NewFake("x")
	require.NoError(t, f.Mkdir("/ext/nfc"))
	require.NoError(t, f.Write("/ext/nfc/a.nfc", []byte("old")))

	exp := New(&integration.Options{}, device.Location{Path: "/ext"}, f)
	got := run(t, exp, record("/nfc/a.nfc", 0644, "new"))
	require.NoError(t, got["/nfc/a.nfc"].Err)

	data, err := f.Read("/ext/nfc/a.nfc")
	require.NoError(t, err)
	require.Equal(t, "new", string(data))
}

func TestExportUncleanPath(t *testing.T) {
	f := device.NewFake("x")
	exp := New(&integration.Options{}, device.Location{Path: "/ext"}, f)
	got := run(t, exp, record("/nfc/../../etc", 0644, "x"))
	require.Error(t, got["/nfc/../../etc"].Err)
}

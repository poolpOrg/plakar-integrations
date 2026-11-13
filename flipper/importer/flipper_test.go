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

package importer

import (
	"context"
	"io"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/kloset/exclude"
	"github.com/poolpOrg/plakar-integrations/flipper/device"
	"github.com/stretchr/testify/require"
)

func collect(t *testing.T, imp *Importer) map[string]*integration.Record {
	t.Helper()

	records := make(chan *integration.Record, 64)
	require.NoError(t, imp.Import(context.Background(), records, nil))

	got := map[string]*integration.Record{}
	for r := range records {
		got[r.Pathname] = r
	}
	return got
}

func newFake(t *testing.T) *device.Fake {
	t.Helper()
	f := device.NewFake("Lc4str1g1")
	require.NoError(t, f.Mkdir("/ext/nfc"))
	require.NoError(t, f.Mkdir("/ext/nfc/sub"))
	require.NoError(t, f.Write("/ext/nfc/a.nfc", []byte("Filetype: Flipper NFC device\n")))
	require.NoError(t, f.Write("/ext/nfc/sub/b.nfc", []byte("b")))
	require.NoError(t, f.Write("/ext/nfc/junk.bak", []byte("old")))
	require.NoError(t, f.Write("/ext/other.txt", []byte("not in scope")))
	return f
}

func TestImport(t *testing.T) {
	f := newFake(t)
	loc := device.Location{Path: "/ext/nfc"}
	imp := New(&integration.Options{}, loc, f, nil, true)

	require.Equal(t, "Lc4str1g1", imp.Origin())
	require.Equal(t, "/ext/nfc", imp.Root())
	require.NoError(t, imp.Ping(context.Background()))

	got := collect(t, imp)
	for _, p := range []string{"/", "/ext", "/ext/nfc", "/ext/nfc/sub"} {
		r, ok := got[p]
		require.True(t, ok, p)
		require.NoError(t, r.Err)
		require.True(t, r.FileInfo.IsDir(), p)
	}
	require.NotContains(t, got, "/ext/other.txt")

	a := got["/ext/nfc/a.nfc"]
	require.NotNil(t, a)
	require.NoError(t, a.Err)
	require.True(t, a.FileInfo.Mode().IsRegular())
	require.Equal(t, int64(29), a.FileInfo.Size())
	require.Equal(t, epoch, a.FileInfo.ModTime())
	require.NotZero(t, a.FileInfo.Ino())
	require.NotZero(t, a.FileInfo.Dev())

	data, err := io.ReadAll(a.Reader)
	require.NoError(t, err)
	require.Equal(t, "Filetype: Flipper NFC device\n", string(data))

	// same content, same identity; different content, different identity
	b := got["/ext/nfc/sub/b.nfc"]
	require.NotEqual(t, a.FileInfo.Ino(), b.FileInfo.Ino())
	again := collect(t, New(&integration.Options{}, loc, f, nil, true))
	require.True(t, again["/ext/nfc/a.nfc"].FileInfo.Equal(&a.FileInfo))
}

func TestImportNoMd5(t *testing.T) {
	f := newFake(t)
	imp := New(&integration.Options{}, device.Location{Path: "/ext/nfc"}, f, nil, false)

	got := collect(t, imp)
	a := got["/ext/nfc/a.nfc"]
	require.Zero(t, a.FileInfo.Ino())
	require.NotEqual(t, epoch, a.FileInfo.ModTime())
}

func TestImportExcludes(t *testing.T) {
	f := newFake(t)
	rules := exclude.NewRuleSet()
	require.NoError(t, rules.AddRulesFromArray([]string{"*.bak", "sub"}))
	imp := New(&integration.Options{}, device.Location{Path: "/ext/nfc"}, f, rules, true)

	got := collect(t, imp)
	require.Contains(t, got, "/ext/nfc/a.nfc")
	require.NotContains(t, got, "/ext/nfc/junk.bak")
	require.NotContains(t, got, "/ext/nfc/sub")
	require.NotContains(t, got, "/ext/nfc/sub/b.nfc")
}

func TestImportSingleFile(t *testing.T) {
	f := newFake(t)
	imp := New(&integration.Options{}, device.Location{Path: "/ext/nfc/a.nfc"}, f, nil, true)

	got := collect(t, imp)
	require.Len(t, got, 4)
	require.Contains(t, got, "/ext/nfc")
	require.True(t, got["/ext/nfc/a.nfc"].FileInfo.Mode().IsRegular())
}

func TestImportMissingRoot(t *testing.T) {
	f := newFake(t)
	imp := New(&integration.Options{}, device.Location{Path: "/ext/nope"}, f, nil, true)

	records := make(chan *integration.Record, 64)
	require.ErrorIs(t, imp.Import(context.Background(), records, nil), device.ErrNotExist)
}

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

package device

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseLocation(t *testing.T) {
	tests := []struct {
		in   string
		want Location
		err  bool
	}{
		{in: "flipper://", want: Location{Path: "/ext"}},
		{in: "flipper:///ext/nfc", want: Location{Path: "/ext/nfc"}},
		{in: "flipper://ext/nfc/", want: Location{Path: "/ext/nfc"}},
		{in: "flipper://int", want: Location{Path: "/int"}},
		{in: "flipper://Lc4str1g1", want: Location{Name: "Lc4str1g1", Path: "/ext"}},
		{in: "flipper://Lc4str1g1/ext/subghz", want: Location{Name: "Lc4str1g1", Path: "/ext/subghz"}},
		{in: "fs:///tmp", err: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseLocation("flipper", tc.in)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFake(t *testing.T) {
	f := NewFake("test")

	require.ErrorIs(t, f.Write("/ext/nfc/a.nfc", []byte("x")), ErrNotExist)
	require.NoError(t, f.Mkdir("/ext/nfc"))
	require.ErrorIs(t, f.Mkdir("/ext/nfc"), ErrExist)
	require.NoError(t, f.Write("/ext/nfc/a.nfc", []byte("x")))

	st, err := f.Stat("/ext/nfc/a.nfc")
	require.NoError(t, err)
	require.Equal(t, int64(1), st.Size)

	entries, err := f.List("/ext")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].IsDir)

	require.NoError(t, f.Write("/ext/nfc/b.nfc", []byte("old")))
	require.NoError(t, f.Rename("/ext/nfc/a.nfc", "/ext/nfc/b.nfc"))
	_, err = f.Stat("/ext/nfc/a.nfc")
	require.ErrorIs(t, err, ErrNotExist)
	data, err := f.Read("/ext/nfc/b.nfc")
	require.NoError(t, err)
	require.Equal(t, "x", string(data))
	require.ErrorIs(t, f.Rename("/ext/nfc/b.nfc", "/ext/nfc"), ErrExist)

	require.ErrorIs(t, f.Delete("/ext/nfc", false), ErrExist)
	require.NoError(t, f.Delete("/ext/nfc", true))
	_, err = f.Stat("/ext/nfc")
	require.ErrorIs(t, err, ErrNotExist)
}

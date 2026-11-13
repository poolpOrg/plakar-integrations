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

package layout

import (
	"testing"

	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/stretchr/testify/require"
)

func TestHexChecksum(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "sha1", in: "kKvgAK3XT5ojBdcx8dwt9cOU46Y=", want: "90abe000add74f9a2305d731f1dc2df5c394e3a6"},
		{name: "not base64", in: "!!", wantErr: true},
		{name: "wrong length", in: "AAAA", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := HexChecksum(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestAssetDir(t *testing.T) {
	tests := []struct {
		name  string
		asset api.Asset
		want  string
	}{
		{name: "local date", asset: api.Asset{LocalDateTime: "2024-07-09T15:00:00.000Z", FileCreatedAt: "2020-01-01T00:00:00Z"}, want: "/assets/2024/07"},
		{name: "falls back to file date", asset: api.Asset{LocalDateTime: "garbage", FileCreatedAt: "2020-01-31T23:00:00Z"}, want: "/assets/2020/01"},
		{name: "undated", asset: api.Asset{}, want: "/assets/undated"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, AssetDir(&tc.asset))
		})
	}
}

func TestSafeName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"IMG_0001.HEIC", "IMG_0001.HEIC"},
		{"a/b\\c", "a_b_c"},
		{"tab\there", "tab_here"},
		{"  ", "untitled"},
		{"..", "untitled"},
		{"été.jpg", "été.jpg"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, SafeName(tc.in))
		})
	}
}

func TestNamerUnique(t *testing.T) {
	n := NewNamer()
	sum1 := "90abe000add74f9a2305d731f1dc2df5c394e3a6"
	sum2 := "aaaaaaaabbbbbbbbccccccccddddddddeeeeeeee"

	require.Equal(t, "/assets/2024/07/IMG_0001.jpg", n.Unique("/assets/2024/07", "IMG_0001.jpg", sum1))
	// Same name, other bytes: the first 8 hex of the checksum go before the extension.
	require.Equal(t, "/assets/2024/07/IMG_0001-aaaaaaaa.jpg", n.Unique("/assets/2024/07", "IMG_0001.jpg", sum2))
	// The side files of a taken name are taken too.
	n.Reserve("/assets/2024/07/x.jpg.json")
	require.Equal(t, "/assets/2024/07/x-90abe000.jpg", n.Unique("/assets/2024/07", "x.jpg", sum1))
	// Different directories do not collide.
	require.Equal(t, "/assets/2024/08/IMG_0001.jpg", n.Unique("/assets/2024/08", "IMG_0001.jpg", sum2))
	// Album names are sanitized.
	require.Equal(t, "/albums/Trip_2024.json", n.Unique("/albums", "Trip/2024.json", ""))
	// Same name and same checksum twice still yields distinct paths.
	require.Equal(t, "/assets/2024/07/IMG_0001-90abe000.jpg", n.Unique("/assets/2024/07", "IMG_0001.jpg", sum1))
	require.Equal(t, "/assets/2024/07/IMG_0001-"+sum1+".jpg", n.Unique("/assets/2024/07", "IMG_0001.jpg", sum1))
	require.Equal(t, "/assets/2024/07/IMG_0001-"+sum1+"-2.jpg", n.Unique("/assets/2024/07", "IMG_0001.jpg", sum1))
}

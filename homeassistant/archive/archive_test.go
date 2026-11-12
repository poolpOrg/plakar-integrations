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

package archive_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poolpOrg/plakar-integrations/homeassistant/archive"
	"github.com/poolpOrg/plakar-integrations/homeassistant/hass/hasstest"
	"github.com/stretchr/testify/require"
)

var fixture = hasstest.Fixture{
	Slug: "abc12345", Name: "nightly", Date: "2026-11-10T12:00:00+00:00", Compressed: true,
	Addons:  []string{"core_mosquitto"},
	Folders: []string{"share"},
	Members: map[string]map[string]string{
		"homeassistant": {
			"data/":                             "",
			"data/configuration.yaml":           "homeassistant:\n  name: Home\n",
			"data/.storage/":                    "",
			"data/.storage/core.config_entries": strings.Repeat("{\"entries\": []}\n", 100),
			"data/home-assistant_v2.db":         strings.Repeat("SQLite format 3\x00", 50),
			"data/link":                         "-> configuration.yaml",
		},
		"core_mosquitto": {
			"addon.json":          "{\"slug\": \"core_mosquitto\"}",
			"data/":               "",
			"data/mosquitto.conf": "listener 1883\n",
		},
		"share": {
			"notes.txt": "hello\n",
		},
	},
	Blobs: map[string][]byte{
		"ssl.tar.gz": []byte("SecureTar\x02\x00 not really but opaque"),
	},
}

func writeFixture(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "backup.tar")
	require.NoError(t, os.WriteFile(p, data, 0600))
	return p
}

func readEntry(t *testing.T, e *archive.Entry) string {
	t.Helper()
	rd, err := e.Open()
	require.NoError(t, err)
	defer rd.Close()
	data, err := io.ReadAll(rd)
	require.NoError(t, err)
	return string(data)
}

func TestOpen(t *testing.T) {
	for _, compressed := range []bool{true, false} {
		fx := fixture
		fx.Compressed = compressed
		t.Run(map[bool]string{true: "gzip", false: "plain"}[compressed], func(t *testing.T) {
			p := writeFixture(t, fx.Build(t))
			a, err := archive.Open(p)
			require.NoError(t, err)
			defer a.Close()

			require.JSONEq(t, string(fx.Metadata(t)), string(a.Metadata))

			byStem := map[string]*archive.Member{}
			for _, m := range a.Members {
				if m.Entries != nil {
					byStem[m.Stem] = m
				} else {
					byStem[m.Name] = m
				}
			}
			require.Len(t, byStem, 4)

			ha := byStem["homeassistant"]
			require.NotNil(t, ha)
			require.Nil(t, ha.Blob)
			names := map[string]*archive.Entry{}
			for _, e := range ha.Entries {
				names[e.Name] = e
			}
			require.Equal(t, "homeassistant:\n  name: Home\n", readEntry(t, names["data/configuration.yaml"]))
			require.Equal(t, strings.Repeat("SQLite format 3\x00", 50), readEntry(t, names["data/home-assistant_v2.db"]))
			require.Equal(t, byte(tar.TypeDir), names["data/.storage"].Header.Typeflag)
			require.Equal(t, byte(tar.TypeSymlink), names["data/link"].Header.Typeflag)
			require.Equal(t, "configuration.yaml", names["data/link"].Header.Linkname)
			require.Equal(t, 1000, names["data/configuration.yaml"].Header.Uid)

			require.Equal(t, "listener 1883\n", readEntry(t, func() *archive.Entry {
				for _, e := range byStem["core_mosquitto"].Entries {
					if e.Name == "data/mosquitto.conf" {
						return e
					}
				}
				return nil
			}()))

			blob := byStem["ssl.tar.gz"]
			require.NotNil(t, blob)
			require.Nil(t, blob.Entries)
			require.Equal(t, string(fx.Blobs["ssl.tar.gz"]), readEntry(t, blob.Blob))

			require.NoError(t, a.Close())
			_, err = os.Stat(p)
			require.True(t, os.IsNotExist(err), "the spool file is removed on close")
		})
	}
}

func TestOpenRejects(t *testing.T) {
	t.Run("no metadata", func(t *testing.T) {
		var out bytes.Buffer
		tw := tar.NewWriter(&out)
		require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "./x.tar", Size: 0}))
		require.NoError(t, tw.Close())
		_, err := archive.Open(writeFixture(t, out.Bytes()))
		require.ErrorContains(t, err, "no backup.json")
	})

	t.Run("nested entry", func(t *testing.T) {
		var out bytes.Buffer
		tw := tar.NewWriter(&out)
		require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "./sub/x.tar", Size: 0}))
		require.NoError(t, tw.Close())
		_, err := archive.Open(writeFixture(t, out.Bytes()))
		require.ErrorContains(t, err, "nested")
	})

	t.Run("escaping name", func(t *testing.T) {
		inner := bytes.Buffer{}
		tw := tar.NewWriter(&inner)
		require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "../../etc/passwd", Size: 1}))
		_, err := tw.Write([]byte("x"))
		require.NoError(t, err)
		require.NoError(t, tw.Close())

		var out bytes.Buffer
		tw = tar.NewWriter(&out)
		meta := fixture.Metadata(t)
		require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "./backup.json", Size: int64(len(meta))}))
		_, err = tw.Write(meta)
		require.NoError(t, err)
		require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "./homeassistant.tar", Size: int64(inner.Len())}))
		_, err = tw.Write(inner.Bytes())
		require.NoError(t, err)
		require.NoError(t, tw.Close())

		_, err = archive.Open(writeFixture(t, out.Bytes()))
		require.ErrorContains(t, err, "refusing entry name")
	})

	t.Run("not a tar", func(t *testing.T) {
		_, err := archive.Open(writeFixture(t, []byte("hello")))
		require.Error(t, err)
	})
}

// TestRoundTrip explodes a backup, rebuilds it with the builder, and
// checks the result reads the same and is the archive Home Assistant
// expects: backup.json first, the inner tars gzipped or not as the
// metadata says, blobs untouched.
func TestRoundTrip(t *testing.T) {
	for _, compressed := range []bool{true, false} {
		fx := fixture
		fx.Compressed = compressed
		t.Run(map[bool]string{true: "gzip", false: "plain"}[compressed], func(t *testing.T) {
			a, err := archive.Open(writeFixture(t, fx.Build(t)))
			require.NoError(t, err)
			defer a.Close()

			b, err := archive.NewBuilder()
			require.NoError(t, err)
			defer b.Close()

			require.Nil(t, b.Metadata())
			require.NoError(t, b.SetMetadata(a.Metadata))
			require.Equal(t, compressed, b.Metadata().IsCompressed())
			require.Equal(t, []string{"core_mosquitto"}, b.Metadata().AddonSlugs())
			require.Equal(t, "abc12345", b.Metadata().Slug)

			for _, m := range a.Members {
				if m.Blob != nil {
					rd, err := m.Blob.Open()
					require.NoError(t, err)
					require.NoError(t, b.AddBlob(m.Name, rd))
					continue
				}
				for _, e := range m.Entries {
					hdr := *e.Header
					hdr.Name = e.Name
					var body io.Reader
					if hdr.Typeflag == tar.TypeReg {
						rd, err := e.Open()
						require.NoError(t, err)
						body = rd
					}
					require.NoError(t, b.AddEntry(m.Stem, &hdr, body))
				}
			}

			var out bytes.Buffer
			n, err := b.WriteTo(&out)
			require.NoError(t, err)
			require.Equal(t, int64(out.Len()), n)

			// the outer layout is what Home Assistant reads
			tr := tar.NewReader(bytes.NewReader(out.Bytes()))
			var names []string
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				names = append(names, hdr.Name)
				if hdr.Name == "./homeassistant.tar.gz" {
					zr, err := gzip.NewReader(tr)
					require.NoError(t, err)
					inner := tar.NewReader(zr)
					first, err := inner.Next()
					require.NoError(t, err)
					require.True(t, strings.HasPrefix(first.Name, "data"), first.Name)
				}
			}
			suffix := ".tar"
			if compressed {
				suffix = ".tar.gz"
			}
			require.Equal(t, "./backup.json", names[0])
			require.ElementsMatch(t, []string{"./backup.json", "./homeassistant" + suffix, "./core_mosquitto" + suffix, "./share" + suffix, "./ssl.tar.gz"}, names)

			// and it explodes back to the same content
			again, err := archive.Open(writeFixture(t, out.Bytes()))
			require.NoError(t, err)
			defer again.Close()
			require.Equal(t, a.Metadata, again.Metadata)
			require.Equal(t, dump(t, a), dump(t, again))
		})
	}
}

func dump(t *testing.T, a *archive.Archive) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range a.Members {
		if m.Blob != nil {
			out[m.Name] = readEntry(t, m.Blob)
			continue
		}
		for _, e := range m.Entries {
			key := m.Stem + "/" + e.Name
			switch e.Header.Typeflag {
			case tar.TypeReg:
				out[key] = readEntry(t, e)
			case tar.TypeSymlink:
				out[key] = "-> " + e.Header.Linkname
			default:
				out[key] = "dir"
			}
		}
	}
	return out
}

func TestBuilderRejects(t *testing.T) {
	b, err := archive.NewBuilder()
	require.NoError(t, err)
	defer b.Close()

	_, err = b.WriteTo(io.Discard)
	require.ErrorContains(t, err, "no backup.json")
	require.ErrorContains(t, b.SetMetadata([]byte("nope")), "backup.json")

	require.NoError(t, b.AddBlob("ssl.tar.gz", strings.NewReader("x")))
	require.ErrorContains(t, b.AddBlob("ssl.tar.gz", strings.NewReader("x")), "duplicate")

	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "data/short", Size: 10}
	require.ErrorContains(t, b.AddEntry("homeassistant", hdr, strings.NewReader("x")), "expected 10")
}

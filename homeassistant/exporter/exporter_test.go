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
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/poolpOrg/plakar-integrations/homeassistant/archive"
	"github.com/poolpOrg/plakar-integrations/homeassistant/exporter"
	"github.com/poolpOrg/plakar-integrations/homeassistant/hass"
	"github.com/poolpOrg/plakar-integrations/homeassistant/hass/hasstest"
	"github.com/poolpOrg/plakar-integrations/homeassistant/importer"
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
			"data/.storage/core.config_entries": "{}\n",
			"data/link":                         "-> configuration.yaml",
		},
		"core_mosquitto": {
			"addon.json":  "{\"slug\": \"core_mosquitto\"}",
			"data/":       "",
			"data/x.conf": "x\n",
		},
		"share": {
			"notes.txt": "hello\n",
		},
	},
	Blobs: map[string][]byte{
		"ssl.tar.gz": []byte("opaque"),
	},
}

// snapshot runs the importer against srv and returns the records a
// snapshot of that backup would hold, in walk order, with the
// backup.json the instance generated.
func snapshot(t *testing.T, srv *hasstest.Server) ([]*integration.Record, []byte) {
	t.Helper()
	var meta []byte
	srv.Generate = func(name string, cmd map[string]any) (*hasstest.Entry, string) {
		fx := fixture
		fx.Name = name
		meta = fx.Metadata(t)
		return &hasstest.Entry{Backup: hass.Backup{ID: fx.Slug, Name: name, Date: fx.Date}, Data: fx.Build(t)}, fx.Slug
	}
	loc, err := hass.ParseLocation("homeassistant", srv.Location(), false)
	require.NoError(t, err)
	imp := importer.New(&integration.Options{}, loc, hass.NewClient(loc, "secret"), importer.Config{Database: true})
	t.Cleanup(func() { imp.Close(context.Background()) })

	records := make(chan *integration.Record, 1000)
	_, err = imp.Import(context.Background(), records, nil)
	require.NoError(t, err)

	var out []*integration.Record
	for r := range records {
		out = append(out, r)
	}
	return out, meta
}

func newExporter(t *testing.T, srv *hasstest.Server, cfg exporter.Config) *exporter.Exporter {
	t.Helper()
	loc, err := hass.ParseLocation("homeassistant", srv.Location(), false)
	require.NoError(t, err)
	var stderr bytes.Buffer
	exp, err := exporter.New(&integration.Options{Stderr: &stderr}, loc, hass.NewClient(loc, "secret"), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, exp.Close(context.Background())) })
	return exp
}

func export(t *testing.T, exp *exporter.Exporter, records []*integration.Record) ([]*integration.Result, error) {
	t.Helper()
	in := make(chan *integration.Record)
	out := make(chan *integration.Result)
	go func() {
		for _, r := range records {
			in <- r
		}
		close(in)
	}()
	var results []*integration.Result
	done := make(chan struct{})
	go func() {
		defer close(done)
		for res := range out {
			results = append(results, res)
		}
	}()
	err := exp.Export(context.Background(), nil, in, out)
	<-done
	return results, err
}

func dump(t *testing.T, data []byte) map[string]string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload.tar")
	require.NoError(t, os.WriteFile(p, data, 0600))
	a, err := archive.Open(p)
	require.NoError(t, err)
	defer a.Close()

	out := map[string]string{"backup.json": string(a.Metadata)}
	for _, m := range a.Members {
		if m.Blob != nil {
			rd, err := m.Blob.Open()
			require.NoError(t, err)
			data, err := io.ReadAll(rd)
			require.NoError(t, err)
			out[m.Name] = string(data)
			continue
		}
		for _, e := range m.Entries {
			key := m.Stem + "/" + e.Name
			switch e.Header.Typeflag {
			case tar.TypeReg:
				rd, err := e.Open()
				require.NoError(t, err)
				data, err := io.ReadAll(rd)
				require.NoError(t, err)
				out[key] = string(data)
			case tar.TypeSymlink:
				out[key] = "-> " + e.Header.Linkname
			default:
				out[key] = "dir"
			}
		}
	}
	return out
}

func TestExportRoundTrip(t *testing.T) {
	srv := hasstest.New(t, "secret")
	records, meta := snapshot(t, srv)

	exp := newExporter(t, srv, exporter.Config{})
	require.NoError(t, exp.Ping(context.Background()))
	require.Equal(t, "homeassistant", exp.Type())

	results, err := export(t, exp, records)
	require.NoError(t, err)
	require.Len(t, results, len(records))
	for _, res := range results {
		require.NoError(t, res.Err, res.Record.Pathname)
	}

	require.Len(t, srv.Uploads, 1)
	require.Equal(t, "backup.local", srv.Uploads[0].Agent)
	require.Equal(t, "abc12345.tar", srv.Uploads[0].Filename)

	want := map[string]string{
		"backup.json":                                     "",
		"ssl.tar.gz":                                      "opaque",
		"homeassistant/data":                              "dir",
		"homeassistant/data/configuration.yaml":           "homeassistant:\n  name: Home\n",
		"homeassistant/data/.storage":                     "dir",
		"homeassistant/data/.storage/core.config_entries": "{}\n",
		"homeassistant/data/link":                         "-> configuration.yaml",
		"core_mosquitto/addon.json":                       "{\"slug\": \"core_mosquitto\"}",
		"core_mosquitto/data":                             "dir",
		"core_mosquitto/data/x.conf":                      "x\n",
		"share/notes.txt":                                 "hello\n",
	}
	got := dump(t, srv.Uploads[0].Data)
	require.Equal(t, string(meta), got["backup.json"], "backup.json goes back untouched")
	delete(got, "backup.json")
	delete(want, "backup.json")
	require.Equal(t, want, got)

	require.Empty(t, srv.Restores, "restore is opt-in")
}

func TestExportRestore(t *testing.T) {
	for _, agent := range []string{"backup.local", "hassio.local"} {
		t.Run(agent, func(t *testing.T) {
			srv := hasstest.New(t, "secret")
			srv.Agents = []string{agent}
			records, _ := snapshot(t, srv)

			exp := newExporter(t, srv, exporter.Config{Restore: true, Password: "pw"})
			_, err := export(t, exp, records)
			require.NoError(t, err)

			require.Len(t, srv.Restores, 1)
			cmd := srv.Restores[0]
			require.Equal(t, "abc12345", cmd["backup_id"])
			require.Equal(t, agent, cmd["agent_id"])
			require.Equal(t, "pw", cmd["password"])
			if agent == "hassio.local" {
				require.Equal(t, []any{"core_mosquitto"}, cmd["restore_addons"])
				require.Equal(t, []any{"share"}, cmd["restore_folders"])
			} else {
				require.Nil(t, cmd["restore_addons"])
				require.Nil(t, cmd["restore_folders"])
			}
		})
	}

	t.Run("instance restarts", func(t *testing.T) {
		srv := hasstest.New(t, "secret")
		srv.DropOnRestore = true
		records, _ := snapshot(t, srv)
		exp := newExporter(t, srv, exporter.Config{Restore: true})
		_, err := export(t, exp, records)
		require.NoError(t, err)
		require.Len(t, srv.Restores, 1)
	})
}

func TestExportPartial(t *testing.T) {
	srv := hasstest.New(t, "secret")
	records, _ := snapshot(t, srv)

	var partial []*integration.Record
	for _, r := range records {
		if r.Pathname != "/backup.json" {
			partial = append(partial, r)
		}
	}
	exp := newExporter(t, srv, exporter.Config{})
	_, err := export(t, exp, partial)
	require.ErrorIs(t, err, archive.ErrNoMetadata)
	require.Empty(t, srv.Uploads)
}

func TestExportFailedRecord(t *testing.T) {
	srv := hasstest.New(t, "secret")
	records, _ := snapshot(t, srv)
	records = append(records, integration.NewError("/core_mosquitto/data/x.conf", io.ErrUnexpectedEOF))

	exp := newExporter(t, srv, exporter.Config{Restore: true})
	results, err := export(t, exp, records)
	require.ErrorContains(t, err, "1 records could not be staged")
	require.Len(t, results, len(records))
	require.Empty(t, srv.Uploads, "an incomplete backup is never uploaded")
	require.Empty(t, srv.Restores)
}

func TestNewExporter(t *testing.T) {
	ctx := context.Background()
	t.Setenv(hass.TokenEnv, "")
	_, err := exporter.NewExporter(ctx, &integration.Options{}, "homeassistant", map[string]string{"location": "homeassistant://h"})
	require.ErrorContains(t, err, "no access token")

	_, err = exporter.NewExporter(ctx, &integration.Options{}, "homeassistant", map[string]string{"location": "homeassistant://h", "token": "t", "restore": "yes please"})
	require.ErrorContains(t, err, "invalid value for restore")

	exp, err := exporter.NewExporter(ctx, &integration.Options{}, "homeassistant", map[string]string{"location": "homeassistant://h", "token": "t"})
	require.NoError(t, err)
	require.Equal(t, "h:8123", exp.Origin())
	require.NoError(t, exp.Close(ctx))
}

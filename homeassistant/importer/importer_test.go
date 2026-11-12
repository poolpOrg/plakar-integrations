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
	"context"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
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
			"data/.storage/core.config_entries": "{}\n", // no directory entry for .storage
			"data/link":                         "-> configuration.yaml",
		},
		"core_mosquitto": {
			"addon.json": "{\"slug\": \"core_mosquitto\"}",
		},
	},
	Blobs: map[string][]byte{
		"ssl.tar.gz": []byte("opaque"),
	},
}

// collected is what came out of Import, by path.
type collected struct {
	record  *integration.Record
	content string
}

func run(t *testing.T, imp *importer.Importer) (map[string]collected, map[string]string) {
	t.Helper()
	records := make(chan *integration.Record)
	got := map[string]collected{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for r := range records {
			c := collected{record: r}
			if r.Err == nil && r.FileInfo.Mode().IsRegular() {
				data, err := io.ReadAll(r.Reader)
				require.NoError(t, err)
				c.content = string(data)
			}
			r.Close()
			got[r.Pathname] = c
		}
	}()
	meta, err := imp.Import(context.Background(), records, nil)
	<-done
	require.NoError(t, err)
	return got, meta
}

func newImporter(t *testing.T, srv *hasstest.Server, cfg importer.Config) *importer.Importer {
	t.Helper()
	loc, err := hass.ParseLocation("homeassistant", srv.Location(), false)
	require.NoError(t, err)
	imp := importer.New(&integration.Options{}, loc, hass.NewClient(loc, "secret"), cfg)
	t.Cleanup(func() { require.NoError(t, imp.Close(context.Background())) })
	return imp
}

func generator(t *testing.T, fx hasstest.Fixture, password string) func(string, map[string]any) (*hasstest.Entry, string) {
	return func(name string, cmd map[string]any) (*hasstest.Entry, string) {
		fx.Name = name
		fx.Protected = password != ""
		return &hasstest.Entry{Backup: hass.Backup{ID: fx.Slug, Name: name, Date: fx.Date}, Data: fx.Build(t), Password: password}, fx.Slug
	}
}

func TestImport(t *testing.T) {
	srv := hasstest.New(t, "secret")
	srv.Generate = generator(t, fixture, "")
	imp := newImporter(t, srv, importer.Config{Database: true, Addons: true, Delete: true})

	require.NoError(t, imp.Ping(context.Background()))
	require.Equal(t, "homeassistant", imp.Type())
	require.Equal(t, "/", imp.Root())
	require.Equal(t, strings.TrimPrefix(srv.URL, "http://"), imp.Origin())

	got, meta := run(t, imp)
	require.Equal(t, "abc12345", meta["backup"])
	require.Equal(t, "backup.local", meta["agent"])
	require.True(t, strings.HasPrefix(meta["name"], "plakar 20"), meta["name"])

	var paths []string
	for p := range got {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	require.Equal(t, []string{
		"/",
		"/backup.json",
		"/core_mosquitto",
		"/core_mosquitto/addon.json",
		"/homeassistant",
		"/homeassistant/data",
		"/homeassistant/data/.storage",
		"/homeassistant/data/.storage/core.config_entries",
		"/homeassistant/data/configuration.yaml",
		"/homeassistant/data/link",
		"/ssl.tar.gz",
	}, paths)

	require.True(t, got["/"].record.FileInfo.IsDir())
	require.True(t, got["/homeassistant/data/.storage"].record.FileInfo.IsDir(), "missing parents are synthesized")
	require.Equal(t, "homeassistant:\n  name: Home\n", got["/homeassistant/data/configuration.yaml"].content)
	require.Equal(t, uint64(1000), got["/homeassistant/data/configuration.yaml"].record.FileInfo.Uid())
	require.Equal(t, os.FileMode(0644), got["/homeassistant/data/configuration.yaml"].record.FileInfo.Mode())
	require.Equal(t, "configuration.yaml", got["/homeassistant/data/link"].record.Target)
	require.NotZero(t, got["/homeassistant/data/link"].record.FileInfo.Mode()&os.ModeSymlink)
	require.Equal(t, "opaque", got["/ssl.tar.gz"].content)
	require.JSONEq(t, string(fixtureNamed(meta["name"]).Metadata(t)), got["/backup.json"].content)

	require.Equal(t, []string{"abc12345"}, srv.Deleted, "delete=true removes the backup once imported")
	require.Contains(t, srv.Commands, "backup/subscribe_events")
}

func fixtureNamed(name string) hasstest.Fixture {
	fx := fixture
	fx.Name = name
	return fx
}

func TestImportExisting(t *testing.T) {
	srv := hasstest.New(t, "secret")
	srv.Add(&hasstest.Entry{Backup: hass.Backup{ID: "old00001", Name: "manual", Date: fixture.Date}, Data: fixture.Build(t)})
	imp := newImporter(t, srv, importer.Config{Backup: "old00001"})

	got, meta := run(t, imp)
	require.Equal(t, "old00001", meta["backup"])
	require.Equal(t, "manual", meta["name"])
	require.Contains(t, got, "/homeassistant/data/configuration.yaml")
	require.NotContains(t, srv.Commands, "backup/generate")
	require.Empty(t, srv.Deleted)

	missing := newImporter(t, srv, importer.Config{Backup: "nope"})
	records := make(chan *integration.Record, 100)
	_, err := missing.Import(context.Background(), records, nil)
	require.ErrorContains(t, err, "no backup nope")
}

func TestImportProtected(t *testing.T) {
	srv := hasstest.New(t, "secret")
	srv.Generate = generator(t, fixture, "hunter2")

	noPassword := newImporter(t, srv, importer.Config{})
	records := make(chan *integration.Record, 100)
	_, err := noPassword.Import(context.Background(), records, nil)
	require.ErrorContains(t, err, "encrypted, set the password")

	imp := newImporter(t, srv, importer.Config{Password: "hunter2"})
	got, _ := run(t, imp)
	require.Equal(t, "homeassistant:\n  name: Home\n", got["/homeassistant/data/configuration.yaml"].content)
}

func TestImportSupervised(t *testing.T) {
	srv := hasstest.New(t, "secret")
	srv.Agents = []string{"hassio.local"}
	var seen map[string]any
	srv.Generate = func(name string, cmd map[string]any) (*hasstest.Entry, string) {
		seen = cmd
		return generator(t, fixture, "")(name, cmd)
	}
	imp := newImporter(t, srv, importer.Config{Database: false, Addons: true, Folders: []string{"share", "media"}, Name: "weekly"})

	_, meta := run(t, imp)
	require.Equal(t, "hassio.local", meta["agent"])
	require.Equal(t, "weekly", meta["name"])
	require.Equal(t, true, seen["include_all_addons"])
	require.Equal(t, false, seen["include_database"])
	require.Equal(t, []any{"share", "media"}, seen["include_folders"])
	require.Equal(t, "weekly", seen["name"])
}

func TestNewImporter(t *testing.T) {
	ctx := context.Background()
	opts := &integration.Options{}
	t.Setenv(hass.TokenEnv, "")

	_, err := importer.NewImporter(ctx, opts, "homeassistant", map[string]string{"location": "homeassistant://h"})
	require.ErrorContains(t, err, "no access token")

	_, err = importer.NewImporter(ctx, opts, "homeassistant", map[string]string{"location": "homeassistant://h", "token": "t", "delete": "maybe"})
	require.ErrorContains(t, err, "invalid value for delete")

	_, err = importer.NewImporter(ctx, opts, "homeassistant", map[string]string{"location": "homeassistant://h", "token": "t", "delete": "true", "backup": "x"})
	require.ErrorContains(t, err, "delete only applies")

	t.Setenv(hass.TokenEnv, "from-env")
	imp, err := importer.NewImporter(ctx, opts, "homeassistant", map[string]string{"location": "homeassistant://h", "tls": "true"})
	require.NoError(t, err)
	require.Equal(t, "h:8123", imp.Origin())
}

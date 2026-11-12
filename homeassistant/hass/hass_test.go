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

package hass_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/poolpOrg/plakar-integrations/homeassistant/hass"
	"github.com/poolpOrg/plakar-integrations/homeassistant/hass/hasstest"
	"github.com/stretchr/testify/require"
)

func TestParseLocation(t *testing.T) {
	cases := []struct {
		in   string
		tls  bool
		host string
		err  string
	}{
		{in: "homeassistant://homeassistant.local", host: "homeassistant.local:8123"},
		{in: "homeassistant://homeassistant.local/", host: "homeassistant.local:8123"},
		{in: "homeassistant://10.0.0.5:8443", tls: true, host: "10.0.0.5:8443"},
		{in: "homeassistant://[::1]:8123", host: "[::1]:8123"},
		{in: "homeassistant://[::1]", host: "[::1]:8123"},
		{in: "homeassistant://", err: "no host"},
		{in: "homeassistant://host/api", err: "path is not supported"},
		{in: "homeassistant://user:pw@host", err: "credentials"},
		{in: "flipper://host", err: "does not start with"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			loc, err := hass.ParseLocation("homeassistant", c.in, c.tls)
			if c.err != "" {
				require.ErrorContains(t, err, c.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.host, loc.Host)
			require.Equal(t, c.tls, loc.TLS)
		})
	}

	loc, err := hass.ParseLocation("homeassistant", "homeassistant://h:1", true)
	require.NoError(t, err)
	require.Equal(t, "https://h:1/api/", loc.URL("/api/"))
	require.Equal(t, "wss://h:1/api/websocket", loc.WebsocketURL())
	require.Equal(t, "homeassistant://h:1", loc.String())
}

func connect(t *testing.T, srv *hasstest.Server, token string) (*hass.Client, *hass.Session) {
	t.Helper()
	loc, err := hass.ParseLocation("homeassistant", srv.Location(), false)
	require.NoError(t, err)
	client := hass.NewClient(loc, token)
	sess, err := client.Connect(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { sess.Close() })
	return client, sess
}

func TestPingAndAuth(t *testing.T) {
	srv := hasstest.New(t, "secret")
	loc, err := hass.ParseLocation("homeassistant", srv.Location(), false)
	require.NoError(t, err)

	require.NoError(t, hass.NewClient(loc, "secret").Ping(context.Background()))
	require.ErrorContains(t, hass.NewClient(loc, "wrong").Ping(context.Background()), "401")

	_, err = hass.NewClient(loc, "wrong").Connect(context.Background())
	require.ErrorContains(t, err, "authentication refused")
}

func TestGenerateFlow(t *testing.T) {
	srv := hasstest.New(t, "secret")
	fx := hasstest.Fixture{Slug: "abc12345", Name: "plakar test", Date: "2026-11-10T12:00:00+00:00", Compressed: true,
		Members: map[string]map[string]string{"homeassistant": {"data/configuration.yaml": "homeassistant:\n"}}}
	srv.Generate = func(name string, cmd map[string]any) (*hasstest.Entry, string) {
		require.Equal(t, []any{"backup.local"}, cmd["agent_ids"])
		require.Equal(t, true, cmd["include_database"])
		require.Nil(t, cmd["include_all_addons"])
		fx.Name = name
		return &hasstest.Entry{Backup: hass.Backup{ID: fx.Slug, Name: name, Date: fx.Date}, Data: fx.Build(t)}, fx.Slug
	}

	client, sess := connect(t, srv, "secret")
	ctx := context.Background()

	agent, err := sess.LocalAgent(ctx)
	require.NoError(t, err)
	require.Equal(t, "backup.local", agent)

	require.NoError(t, sess.Subscribe(ctx))
	job, err := sess.Generate(ctx, hass.GenerateOptions{AgentID: agent, Name: "plakar test", IncludeDatabase: true})
	require.NoError(t, err)
	require.Equal(t, "abc12345", job)
	require.NoError(t, sess.WaitCreated(ctx))

	b, err := sess.Find(ctx, job, "plakar test")
	require.NoError(t, err)
	require.Equal(t, "abc12345", b.ID)
	require.False(t, b.ProtectedOn(agent))

	body, _, err := client.Download(ctx, b.ID, agent, "")
	require.NoError(t, err)
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	body.Close()
	require.Equal(t, fx.Build(t), data)

	require.NoError(t, sess.Delete(ctx, b.ID))
	require.False(t, srv.Has(b.ID))
	got, err := sess.Get(ctx, b.ID)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestGenerateFailedAndSupervisedLookup(t *testing.T) {
	srv := hasstest.New(t, "secret")
	srv.Agents = []string{"hassio.local"}
	srv.Generate = func(name string, cmd map[string]any) (*hasstest.Entry, string) {
		if name == "boom" {
			return nil, "job-1"
		}
		// supervised installs name the job after the supervisor job, not the backup
		return &hasstest.Entry{Backup: hass.Backup{ID: "deadbeef", Name: name, Date: "2026-11-10T12:00:00+00:00"}, Data: []byte("x")}, "a1b2c3d4-job"
	}
	srv.Add(&hasstest.Entry{Backup: hass.Backup{ID: "older", Name: "nightly", Date: "2026-11-09T12:00:00+00:00"}, Data: []byte("y")})

	_, sess := connect(t, srv, "secret")
	ctx := context.Background()
	require.NoError(t, sess.Subscribe(ctx))

	_, err := sess.Generate(ctx, hass.GenerateOptions{AgentID: "hassio.local", Name: "boom"})
	require.NoError(t, err)
	require.ErrorContains(t, sess.WaitCreated(ctx), "backup_manager_error")

	job, err := sess.Generate(ctx, hass.GenerateOptions{AgentID: "hassio.local", Name: "nightly", IncludeAddons: true, IncludeFolders: []string{"share"}})
	require.NoError(t, err)
	require.NoError(t, sess.WaitCreated(ctx))
	b, err := sess.Find(ctx, job, "nightly")
	require.NoError(t, err)
	require.Equal(t, "deadbeef", b.ID, "the newest backup with that name")

	_, err = sess.Find(ctx, "nope", "nothing")
	require.ErrorContains(t, err, `no backup named "nothing"`)
}

func TestUploadAndRestore(t *testing.T) {
	srv := hasstest.New(t, "secret")
	client, sess := connect(t, srv, "secret")
	ctx := context.Background()

	id, err := client.Upload(ctx, "backup.local", "cafe0123.tar", strings.NewReader("tar bytes"))
	require.NoError(t, err)
	require.Equal(t, "cafe0123", id)
	require.Len(t, srv.Uploads, 1)
	require.Equal(t, "tar bytes", string(srv.Uploads[0].Data))

	require.NoError(t, sess.Restore(ctx, hass.RestoreOptions{BackupID: id, AgentID: "backup.local", Addons: []string{"core_mosquitto"}}))
	require.Len(t, srv.Restores, 1)
	require.Equal(t, id, srv.Restores[0]["backup_id"])
	require.Equal(t, true, srv.Restores[0]["restore_homeassistant"])
	require.Equal(t, []any{"core_mosquitto"}, srv.Restores[0]["restore_addons"])
	require.Nil(t, srv.Restores[0]["password"])
}

func TestRestoreDropsConnection(t *testing.T) {
	srv := hasstest.New(t, "secret")
	srv.DropOnRestore = true
	_, sess := connect(t, srv, "secret")

	err := sess.Restore(context.Background(), hass.RestoreOptions{BackupID: "x", AgentID: "backup.local"})
	require.ErrorIs(t, err, hass.ErrRestoreUnconfirmed)
}

func TestLocalAgentMissing(t *testing.T) {
	srv := hasstest.New(t, "secret")
	srv.Agents = []string{"cloud.cloud"}
	_, sess := connect(t, srv, "secret")

	_, err := sess.LocalAgent(context.Background())
	require.ErrorContains(t, err, "cloud.cloud")
}

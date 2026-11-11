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
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/poolpOrg/plakar-integrations/minecraft/rcon/rcontest"
	"github.com/poolpOrg/plakar-integrations/minecraft/server/servertest"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	servertest.Main(m)
}

func serverDir(t *testing.T, rconAddr string) string {
	t.Helper()

	dir := t.TempDir()
	props := "level-name=world\n"
	if rconAddr != "" {
		host, port, err := net.SplitHostPort(rconAddr)
		require.NoError(t, err)
		props += "enable-rcon=true\nserver-ip=" + host + "\nrcon.port=" + port + "\nrcon.password=secret\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.properties"), []byte(props), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "world"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "world", "session.lock"), []byte("☃"), 0644))
	return dir
}

func newExporter(t *testing.T, dir string, extra map[string]string) (*Exporter, error) {
	t.Helper()

	config := map[string]string{"location": "minecraft://" + dir}
	for k, v := range extra {
		config[k] = v
	}
	exp, err := NewExporter(t.Context(), &integration.Options{Hostname: "test", MaxConcurrency: 1}, "minecraft", config)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { exp.Close(context.Background()) })
	return exp.(*Exporter), nil
}

func restore(t *testing.T, exp *Exporter, pathname, content string) error {
	t.Helper()

	records := make(chan *integration.Record, 1)
	results := make(chan *integration.Result, 1)
	records <- &integration.Record{
		Pathname: pathname,
		Reader:   io.NopCloser(strings.NewReader(content)),
		FileInfo: objects.FileInfo{Lname: filepath.Base(pathname), Lmode: 0644, Lsize: int64(len(content))},
	}
	close(records)

	require.NoError(t, exp.Export(t.Context(), nil, records, results))
	res := <-results
	return res.Err
}

func TestRestoreStoppedServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close()

	dir := serverDir(t, addr)
	exp, err := newExporter(t, dir, nil)
	require.NoError(t, err)

	require.NoError(t, restore(t, exp, "/world/level.dat", "level"))
	data, err := os.ReadFile(filepath.Join(dir, "world", "level.dat"))
	require.NoError(t, err)
	require.Equal(t, "level", string(data))
}

func TestRestoreIntoNewDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	exp, err := newExporter(t, dir, nil)
	require.NoError(t, err)

	require.NoError(t, restore(t, exp, "/eula.txt", "eula=true\n"))
	_, err = os.Stat(filepath.Join(dir, "eula.txt"))
	require.NoError(t, err)
}

func TestRefuseLockedWorld(t *testing.T) {
	dir := serverDir(t, "")
	servertest.Hold(t, filepath.Join(dir, "world", "session.lock"))

	_, err := newExporter(t, dir, nil)
	require.ErrorContains(t, err, "the server is running")
}

func TestRefuseAnsweringConsole(t *testing.T) {
	srv := rcontest.New(t, "secret", nil)
	dir := serverDir(t, srv.Addr())

	_, err := newExporter(t, dir, nil)
	require.ErrorContains(t, err, "the server is running")
	require.Empty(t, srv.Commands())

	// a wrong password is still a server answering
	t.Setenv("MINECRAFT_RCON_PASSWORD", "wrong")
	_, err = newExporter(t, dir, nil)
	require.ErrorContains(t, err, "the server is running")
}

func TestAllowRunning(t *testing.T) {
	srv := rcontest.New(t, "secret", nil)
	dir := serverDir(t, srv.Addr())
	servertest.Hold(t, filepath.Join(dir, "world", "session.lock"))

	exp, err := newExporter(t, dir, map[string]string{"allow_running": "true"})
	require.NoError(t, err)
	require.NoError(t, restore(t, exp, "/ops.json", "[]\n"))
}

func TestBadConfig(t *testing.T) {
	dir := serverDir(t, "")

	for name, extra := range map[string]map[string]string{
		"relative":      {"location": "minecraft://relative"},
		"allow_running": {"allow_running": "maybe"},
		"timeout":       {"timeout": "soon"},
		"zero timeout":  {"timeout": "0s"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newExporter(t, dir, extra)
			require.Error(t, err)
		})
	}
}

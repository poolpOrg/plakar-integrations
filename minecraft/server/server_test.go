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

package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poolpOrg/plakar-integrations/minecraft/server/servertest"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	servertest.Main(m)
}

func TestReadProperties(t *testing.T) {
	in := `#Minecraft server properties
#Tue Nov 10 22:14:03 CET 2026
enable-rcon=true
rcon.port=25575
rcon.password=s\:cret\=with\#chars
server-ip=
level-name=world
motd=A Minecraft Serveré
! bang comment
key with spaces = value with spaces
colon:separated
multi=first \
    second
trailing\\=backslash
`
	props, err := ReadProperties(strings.NewReader(in))
	require.NoError(t, err)

	require.Equal(t, Properties{
		"enable-rcon":   "true",
		"rcon.port":     "25575",
		"rcon.password": "s:cret=with#chars",
		"server-ip":     "",
		"level-name":    "world",
		"motd":          "A Minecraft Serveré",
		"key":           "with spaces = value with spaces",
		"colon":         "separated",
		"multi":         "first second",
		"trailing\\":    "backslash",
	}, props)
}

func TestOpen(t *testing.T) {
	dir := t.TempDir()

	srv, err := Open(dir)
	require.NoError(t, err)
	require.Nil(t, srv.Properties)
	_, ok := srv.RconAddr("")
	require.False(t, ok)

	addr, ok := srv.RconAddr("10.0.0.1:1234")
	require.True(t, ok)
	require.Equal(t, "10.0.0.1:1234", addr)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.properties"), []byte("enable-rcon=true\nrcon.password=pw\n"), 0644))
	srv, err = Open(dir)
	require.NoError(t, err)
	addr, ok = srv.RconAddr("")
	require.True(t, ok)
	require.Equal(t, "127.0.0.1:25575", addr)
	require.Equal(t, "pw", srv.RconPassword())

	t.Setenv(PasswordEnv, "fromenv")
	require.Equal(t, "fromenv", srv.RconPassword())

	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.properties"), []byte("enable-rcon=true\nserver-ip=192.168.1.2\nrcon.port=2000\n"), 0644))
	srv, err = Open(dir)
	require.NoError(t, err)
	addr, _ = srv.RconAddr("")
	require.Equal(t, "192.168.1.2:2000", addr)

	_, err = Open(filepath.Join(dir, "server.properties"))
	require.ErrorContains(t, err, "not a directory")

	_, err = Open(filepath.Join(dir, "nope"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRunning(t *testing.T) {
	dir := t.TempDir()
	srv, err := Open(dir)
	require.NoError(t, err)

	// no world at all
	running, err := srv.Running()
	require.NoError(t, err)
	require.False(t, running)

	// a world left by a stopped server
	world := filepath.Join(dir, "world")
	require.NoError(t, os.MkdirAll(world, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(world, "session.lock"), []byte("☃"), 0644))
	running, err = srv.Running()
	require.NoError(t, err)
	require.False(t, running)

	// a server holding the lock of the nether
	nether := filepath.Join(dir, "world_nether")
	require.NoError(t, os.MkdirAll(nether, 0755))
	servertest.Hold(t, filepath.Join(nether, "session.lock"))
	running, err = srv.Running()
	require.NoError(t, err)
	require.True(t, running)
}

// level-name may be a path, and then the world is not a top-level
// directory of the server.
func TestRunningLevelName(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.properties"), []byte("level-name=worlds/main\n"), 0644))
	srv, err := Open(dir)
	require.NoError(t, err)

	world := filepath.Join(dir, "worlds", "main")
	require.NoError(t, os.MkdirAll(world, 0755))
	running, err := srv.Running()
	require.NoError(t, err)
	require.False(t, running)

	servertest.Hold(t, filepath.Join(world, "session.lock"))
	running, err = srv.Running()
	require.NoError(t, err)
	require.True(t, running)
}

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
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/poolpOrg/plakar-integrations/minecraft/rcon/rcontest"
	"github.com/poolpOrg/plakar-integrations/minecraft/server/servertest"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	servertest.Main(m)
}

// serverDir lays out a small server with a world in a temp directory.
// rconAddr goes into server.properties; empty disables RCON.
func serverDir(t *testing.T, rconAddr string) string {
	t.Helper()

	dir := t.TempDir()
	props := "level-name=world\nmotd=test\n"
	if rconAddr != "" {
		host, port, err := net.SplitHostPort(rconAddr)
		require.NoError(t, err)
		props += "enable-rcon=true\nserver-ip=" + host + "\nrcon.port=" + port + "\nrcon.password=secret\n"
	} else {
		props += "enable-rcon=false\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.properties"), []byte(props), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "eula.txt"), []byte("eula=true\n"), 0644))

	region := filepath.Join(dir, "world", "region")
	require.NoError(t, os.MkdirAll(region, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "world", "level.dat"), []byte("level"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "world", "session.lock"), []byte("☃"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(region, "r.0.0.mca"), []byte(strings.Repeat("chunk", 100)), 0644))
	return dir
}

func newImporter(t *testing.T, dir string, extra map[string]string) *Importer {
	t.Helper()

	config := map[string]string{"location": "minecraft://" + dir}
	for k, v := range extra {
		config[k] = v
	}
	opts := &integration.Options{Hostname: "test", MaxConcurrency: 2}
	imp, err := NewImporter(t.Context(), opts, "minecraft", config)
	require.NoError(t, err)
	t.Cleanup(func() { imp.Close(context.Background()) })
	return imp.(*Importer)
}

// consume reads records like kloset does: file contents are read and
// each record acknowledged, results is closed after the last one.
// beforeAck runs before each acknowledgment.
func consume(t *testing.T, records <-chan *integration.Record, results chan<- *integration.Result, beforeAck func()) map[string]struct{} {
	t.Helper()

	seen := map[string]struct{}{}
	for rec := range records {
		if rec.Err == nil {
			seen[rec.Pathname] = struct{}{}
			if rec.FileInfo.Lmode.IsRegular() {
				_, err := io.Copy(io.Discard, rec.Reader)
				require.NoError(t, err)
			}
		}
		if beforeAck != nil {
			beforeAck()
		}
		results <- rec.Ok()
	}
	close(results)
	return seen
}

func runImport(t *testing.T, imp *Importer, beforeAck func()) (map[string]struct{}, error) {
	t.Helper()

	records := make(chan *integration.Record, 4)
	results := make(chan *integration.Result, 4)
	done := make(chan error, 1)
	go func() { _, err := imp.Import(t.Context(), records, results); done <- err }()
	seen := consume(t, records, results, beforeAck)
	return seen, <-done
}

func TestBackupPausesSaving(t *testing.T) {
	srv := rcontest.New(t, "secret", nil)
	dir := serverDir(t, srv.Addr())
	imp := newImporter(t, dir, nil)

	seen, err := runImport(t, imp, func() {
		// saving stays off until the last file has been read
		require.Equal(t, []string{"save-off", "save-all flush"}, srv.Commands())
	})
	require.NoError(t, err)
	require.Equal(t, []string{"save-off", "save-all flush", "save-on"}, srv.Commands())

	require.Contains(t, seen, filepath.ToSlash(filepath.Join(dir, "world", "region", "r.0.0.mca")))
	require.Contains(t, seen, filepath.ToSlash(filepath.Join(dir, "server.properties")))

	// Close has nothing left to do
	require.NoError(t, imp.Close(t.Context()))
	require.Equal(t, []string{"save-off", "save-all flush", "save-on"}, srv.Commands())
}

func TestSavingAlreadyOff(t *testing.T) {
	srv := rcontest.New(t, "secret", func(cmd string) string {
		if cmd == "save-off" {
			return "Saving is already turned off"
		}
		return rcontest.Vanilla(cmd)
	})
	dir := serverDir(t, srv.Addr())
	imp := newImporter(t, dir, nil)

	_, err := runImport(t, imp, nil)
	require.NoError(t, err)
	require.NoError(t, imp.Close(t.Context()))
	require.Equal(t, []string{"save-off", "save-all flush"}, srv.Commands())
}

// A console that does not know the save commands is not a paused server.
func TestUnexpectedReply(t *testing.T) {
	t.Run("save-off", func(t *testing.T) {
		srv := rcontest.New(t, "secret", func(cmd string) string {
			return "Unknown or incomplete command, see below for error"
		})
		dir := serverDir(t, srv.Addr())
		imp := newImporter(t, dir, nil)

		_, err := runImport(t, imp, nil)
		require.ErrorContains(t, err, "save-off: unexpected reply")
		require.NoError(t, imp.Close(t.Context()))
		require.Equal(t, []string{"save-off"}, srv.Commands())
	})

	t.Run("save-all", func(t *testing.T) {
		srv := rcontest.New(t, "secret", func(cmd string) string {
			if cmd == "save-all flush" {
				return "Unknown or incomplete command, see below for error"
			}
			return rcontest.Vanilla(cmd)
		})
		dir := serverDir(t, srv.Addr())
		imp := newImporter(t, dir, nil)

		_, err := runImport(t, imp, nil)
		require.ErrorContains(t, err, "save-all flush: unexpected reply")
		// saving was paused, it is resumed on the way out
		require.Equal(t, []string{"save-off", "save-all flush", "save-on"}, srv.Commands())
	})
}

func TestWithoutAcknowledgments(t *testing.T) {
	srv := rcontest.New(t, "secret", nil)
	dir := serverDir(t, srv.Addr())
	imp := newImporter(t, dir, nil)

	records := make(chan *integration.Record, 4)
	done := make(chan error, 1)
	go func() { _, err := imp.Import(t.Context(), records, nil); done <- err }()
	for range records {
	}
	require.NoError(t, <-done)
	require.Equal(t, []string{"save-off", "save-all flush"}, srv.Commands())

	require.NoError(t, imp.Close(t.Context()))
	require.Equal(t, []string{"save-off", "save-all flush", "save-on"}, srv.Commands())
}

func TestStoppedServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close()

	dir := serverDir(t, addr)
	imp := newImporter(t, dir, nil)

	seen, err := runImport(t, imp, nil)
	require.NoError(t, err)
	require.Contains(t, seen, filepath.ToSlash(filepath.Join(dir, "world", "level.dat")))
}

func TestRconDisabledServerStopped(t *testing.T) {
	dir := serverDir(t, "")
	imp := newImporter(t, dir, nil)

	_, err := runImport(t, imp, nil)
	require.NoError(t, err)
}

func TestRconDisabledServerRunning(t *testing.T) {
	dir := serverDir(t, "")
	servertest.Hold(t, filepath.Join(dir, "world", "session.lock"))
	imp := newImporter(t, dir, nil)

	_, err := runImport(t, imp, nil)
	require.ErrorContains(t, err, "rcon is disabled")
}

func TestRconUnreachableServerRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close()

	dir := serverDir(t, addr)
	servertest.Hold(t, filepath.Join(dir, "world", "session.lock"))
	imp := newImporter(t, dir, nil)

	_, err = runImport(t, imp, nil)
	require.ErrorContains(t, err, "cannot pause saving")
}

func TestBadPassword(t *testing.T) {
	srv := rcontest.New(t, "other", nil)
	dir := serverDir(t, srv.Addr())
	imp := newImporter(t, dir, nil)

	_, err := runImport(t, imp, nil)
	require.ErrorContains(t, err, "authentication failed")
	require.Empty(t, srv.Commands())
}

func TestPasswordFromEnvironment(t *testing.T) {
	srv := rcontest.New(t, "fromenv", nil)
	dir := serverDir(t, srv.Addr())
	t.Setenv("MINECRAFT_RCON_PASSWORD", "fromenv")
	imp := newImporter(t, dir, nil)

	_, err := runImport(t, imp, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"save-off", "save-all flush", "save-on"}, srv.Commands())
}

func TestRconOverride(t *testing.T) {
	srv := rcontest.New(t, "secret", nil)
	dir := serverDir(t, "")
	t.Setenv("MINECRAFT_RCON_PASSWORD", "secret")
	imp := newImporter(t, dir, map[string]string{"rcon": srv.Addr()})

	_, err := runImport(t, imp, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"save-off", "save-all flush", "save-on"}, srv.Commands())
}

func TestNoQuiesce(t *testing.T) {
	srv := rcontest.New(t, "secret", nil)
	dir := serverDir(t, srv.Addr())
	imp := newImporter(t, dir, map[string]string{"quiesce": "false"})

	_, err := runImport(t, imp, nil)
	require.NoError(t, err)
	require.NoError(t, imp.Close(t.Context()))
	require.Empty(t, srv.Commands())
}

func TestCancelResumesSaving(t *testing.T) {
	srv := rcontest.New(t, "secret", nil)
	dir := serverDir(t, srv.Addr())
	imp := newImporter(t, dir, nil)

	// a consumer that reads every record but acknowledges none, like
	// a backup that hangs after the walk
	ctx, cancel := context.WithCancel(t.Context())
	records := make(chan *integration.Record, 4)
	results := make(chan *integration.Result, 4)
	done := make(chan error, 1)
	go func() { _, err := imp.Import(ctx, records, results); done <- err }()
	for range records {
	}
	require.Equal(t, []string{"save-off", "save-all flush"}, srv.Commands())

	cancel()
	require.Eventually(t, func() bool {
		return len(srv.Commands()) == 3 && srv.Commands()[2] == "save-on"
	}, 5*time.Second, 10*time.Millisecond)

	close(results)
	require.NoError(t, <-done)
	require.Equal(t, []string{"save-off", "save-all flush", "save-on"}, srv.Commands())
}

func TestBadConfig(t *testing.T) {
	dir := serverDir(t, "")
	opts := &integration.Options{Hostname: "test", MaxConcurrency: 1}

	for name, config := range map[string]map[string]string{
		"relative":     {"location": "minecraft://relative"},
		"missing":      {"location": "minecraft://" + filepath.Join(dir, "missing")},
		"file":         {"location": "minecraft://" + filepath.Join(dir, "eula.txt")},
		"quiesce":      {"location": "minecraft://" + dir, "quiesce": "maybe"},
		"timeout":      {"location": "minecraft://" + dir, "timeout": "soon"},
		"zero timeout": {"location": "minecraft://" + dir, "timeout": "0s"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewImporter(t.Context(), opts, "minecraft", config)
			require.Error(t, err)
		})
	}
}

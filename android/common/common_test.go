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

package common_test

import (
	"context"
	"io/fs"
	"net"
	"testing"
	"time"

	"github.com/poolpOrg/plakar-integrations/android/adb"
	"github.com/poolpOrg/plakar-integrations/android/adb/adbtest"
	"github.com/poolpOrg/plakar-integrations/android/common"
	"github.com/stretchr/testify/require"
)

func TestParseLocation(t *testing.T) {
	tests := []struct {
		in     string
		serial string
		path   string
		err    bool
	}{
		{in: "android://", serial: "", path: ""},
		{in: "android://R58M12ABCDE", serial: "R58M12ABCDE"},
		{in: "android://R58M12ABCDE/storage/emulated/0/DCIM", serial: "R58M12ABCDE", path: "/storage/emulated/0/DCIM"},
		{in: "android:///sdcard/Download/", serial: "", path: "/sdcard/Download"},
		{in: "android://emulator-5554/sdcard", serial: "emulator-5554", path: "/sdcard"},
		{in: "android://192.168.1.5:5555/sdcard/a/../b", serial: "192.168.1.5:5555", path: "/sdcard/b"},
		{in: "android://bad serial", err: true},
		{in: "android://x\x00y", err: true},
		{in: "fs:///tmp", err: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			serial, p, err := common.ParseLocation("android", tc.in)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.serial, serial)
			require.Equal(t, tc.path, p)
		})
	}
}

func TestParseConfig(t *testing.T) {
	t.Setenv("ANDROID_ADB_SERVER_PORT", "")
	t.Setenv("ANDROID_ADB_SERVER_ADDRESS", "")

	cfg, err := common.ParseConfig("android", map[string]string{"location": "android://"}, "source_path")
	require.NoError(t, err)
	require.Equal(t, "", cfg.Serial)
	require.Equal(t, "", cfg.Path)
	require.Equal(t, "127.0.0.1:5037", cfg.ServerAddr)
	require.True(t, cfg.StartServer)
	require.Equal(t, 4, cfg.Streams)

	cfg, err = common.ParseConfig("android", map[string]string{
		"location":     "android://SER1/sdcard",
		"source_path":  "/storage/emulated/0/DCIM/",
		"adb_server":   "127.0.0.1:6000",
		"start_server": "false",
		"streams":      "2",
	}, "source_path")
	require.NoError(t, err)
	require.Equal(t, "SER1", cfg.Serial)
	require.Equal(t, "/storage/emulated/0/DCIM", cfg.Path, "the option overrides the location path")
	require.Equal(t, "127.0.0.1:6000", cfg.ServerAddr)
	require.False(t, cfg.StartServer)
	require.Equal(t, 2, cfg.Streams)

	cfg, err = common.ParseConfig("android", map[string]string{"location": "android://", "serial": "SER2"}, "source_path")
	require.NoError(t, err)
	require.Equal(t, "SER2", cfg.Serial)

	for name, bad := range map[string]map[string]string{
		"conflicting serials": {"location": "android://SER1", "serial": "SER2"},
		"bad serial option":   {"location": "android://", "serial": "a b"},
		"relative path":       {"location": "android://", "source_path": "DCIM"},
		"bad server":          {"location": "android://", "adb_server": "nohost"},
		"bad bool":            {"location": "android://", "start_server": "maybe"},
		"zero streams":        {"location": "android://", "streams": "0"},
		"too many streams":    {"location": "android://", "streams": "99"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := common.ParseConfig("android", bad, "source_path")
			require.Error(t, err)
		})
	}

	t.Setenv("ANDROID_ADB_SERVER_PORT", "5555")
	cfg, err = common.ParseConfig("android", map[string]string{"location": "android://"}, "source_path")
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:5555", cfg.ServerAddr)
}

func TestSelectDevice(t *testing.T) {
	ok := adb.Device{Serial: "OK1", State: adb.StateDevice}
	ok2 := adb.Device{Serial: "OK2", State: adb.StateDevice, Model: "Pixel_7"}
	unauth := adb.Device{Serial: "UNAUTH", State: adb.StateUnauthorized}
	offline := adb.Device{Serial: "OFF", State: adb.StateOffline}
	noperm := adb.Device{Serial: "NOPERM", State: adb.StateNoPermissions}
	recovery := adb.Device{Serial: "REC", State: adb.StateRecovery}

	t.Run("none", func(t *testing.T) {
		_, err := common.SelectDevice(nil, "")
		require.ErrorIs(t, err, common.ErrNoDevice)
		require.Contains(t, err.Error(), "USB debugging")
		_, err = common.SelectDevice(nil, "OK1")
		require.ErrorIs(t, err, common.ErrNoDevice)
	})

	t.Run("single", func(t *testing.T) {
		d, err := common.SelectDevice([]adb.Device{ok}, "")
		require.NoError(t, err)
		require.Equal(t, "OK1", d.Serial)
	})

	t.Run("several without serial", func(t *testing.T) {
		_, err := common.SelectDevice([]adb.Device{ok, ok2}, "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "2 devices connected")
		require.Contains(t, err.Error(), "OK1")
		require.Contains(t, err.Error(), "OK2 (Pixel 7)")
	})

	t.Run("by serial", func(t *testing.T) {
		d, err := common.SelectDevice([]adb.Device{ok, ok2}, "OK2")
		require.NoError(t, err)
		require.Equal(t, "OK2", d.Serial)
		_, err = common.SelectDevice([]adb.Device{ok, ok2}, "OK3")
		require.Error(t, err)
		require.Contains(t, err.Error(), "device OK3 not found")
		require.Contains(t, err.Error(), "OK1")
	})

	for _, d := range []adb.Device{unauth, offline, noperm, recovery} {
		t.Run(d.State, func(t *testing.T) {
			_, err := common.SelectDevice([]adb.Device{d}, "")
			require.Error(t, err)
			require.Contains(t, err.Error(), d.Serial)
			_, err = common.SelectDevice([]adb.Device{ok, d}, d.Serial)
			require.Error(t, err)
		})
	}
	_, err := common.SelectDevice([]adb.Device{unauth}, "")
	require.Contains(t, err.Error(), "Allow USB debugging")
	_, err = common.SelectDevice([]adb.Device{noperm}, "")
	require.Contains(t, err.Error(), "udev")
}

func TestWithinSharedStorage(t *testing.T) {
	for _, p := range []string{"/storage/emulated/0", "/storage/emulated/0/DCIM", "/sdcard", "/sdcard/x", "/mnt/sdcard/y", "/mnt/user/0/primary", "/data/local/tmp", "/data/local/tmp/x"} {
		require.True(t, common.WithinSharedStorage(p), p)
	}
	for _, p := range []string{"/", "/system", "/data", "/data/data/com.app", "/data/local", "/sdcardx", "/storage", "/mnt"} {
		require.False(t, common.WithinSharedStorage(p), p)
	}
}

func TestConnect(t *testing.T) {
	ctx := context.Background()
	srv := adbtest.New(t)
	srv.AddDevice(&adbtest.Device{Serial: "DEV1", Model: "Fake"})

	dev, err := common.Connect(ctx, &common.Config{ServerAddr: srv.Addr})
	require.NoError(t, err)
	require.Equal(t, "DEV1", dev.Serial)
	require.True(t, dev.Features.Has(adb.FeatureStatV2))
	require.NoError(t, dev.Ping(ctx))

	srv.AddDevice(&adbtest.Device{Serial: "DEV2"})
	_, err = common.Connect(ctx, &common.Config{ServerAddr: srv.Addr})
	require.Error(t, err)
	require.Contains(t, err.Error(), "2 devices connected")

	dev, err = common.Connect(ctx, &common.Config{ServerAddr: srv.Addr, Serial: "DEV2"})
	require.NoError(t, err)
	require.Equal(t, "DEV2", dev.Serial)

	srv.RemoveDevice("DEV2")
	err = dev.Ping(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "disconnected")

	srv.SetState("DEV1", adb.StateUnauthorized)
	_, err = common.Connect(ctx, &common.Config{ServerAddr: srv.Addr, Serial: "DEV1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not authorized")
}

func TestConnectNoServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close()

	_, err = common.Connect(context.Background(), &common.Config{ServerAddr: addr, StartServer: false})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot reach the adb server")
	require.Contains(t, err.Error(), "adb start-server")

	// With start_server, a bogus adb binary is reported, not hidden.
	t.Setenv("PLAKAR_ADB", "/nonexistent/adb")
	_, err = common.Connect(context.Background(), &common.Config{ServerAddr: addr, StartServer: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "starting it failed")

	// A remote server is never started.
	err = common.StartServer(context.Background(), "192.0.2.1:5037")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not local")
}

func TestResolveRoot(t *testing.T) {
	ctx := context.Background()
	srv := adbtest.New(t)
	dev := srv.AddDevice(&adbtest.Device{Serial: "DEV1"})
	client := adb.NewClient(srv.Addr)
	features := adb.ParseFeatures("stat_v2,ls_v2")

	s, err := client.Sync(ctx, "DEV1", features)
	require.NoError(t, err)
	defer s.Close()

	_, _, err = common.ResolveRoot(s, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no accessible shared storage")

	ts := time.Unix(1700000000, 0)
	dev.FS.MkdirAll("/storage/emulated/0/DCIM", ts)
	dev.FS.Symlink("/sdcard", "/storage/emulated/0", ts)
	dev.FS.WriteFile("/storage/emulated/0/DCIM/a.jpg", []byte("a"), 0o660, ts)

	root, st, err := common.ResolveRoot(s, "")
	require.NoError(t, err)
	require.Equal(t, "/storage/emulated/0", root)
	require.True(t, st.IsDir())

	// An unreadable primary storage falls through to the next candidate.
	n, _ := dev.FS.Lstat("/storage/emulated/0")
	n.Unreadable = true
	_, _, err = common.ResolveRoot(s, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not readable")
	n.Unreadable = false

	root, st, err = common.ResolveRoot(s, "/sdcard")
	require.NoError(t, err)
	require.Equal(t, "/sdcard", root, "the path given is kept, not the symlink target")
	require.True(t, st.IsDir())

	root, st, err = common.ResolveRoot(s, "/storage/emulated/0/DCIM/a.jpg")
	require.NoError(t, err)
	require.True(t, st.IsRegular())
	require.Equal(t, "/storage/emulated/0/DCIM/a.jpg", root)

	_, _, err = common.ResolveRoot(s, "/storage/emulated/0/nope")
	require.ErrorIs(t, err, fs.ErrNotExist)

	n.Unreadable = true
	_, _, err = common.ResolveRoot(s, "/storage/emulated/0")
	require.ErrorIs(t, err, fs.ErrPermission)
}

func TestPool(t *testing.T) {
	ctx := context.Background()
	srv := adbtest.New(t)
	d := srv.AddDevice(&adbtest.Device{Serial: "DEV1"})
	d.FS.WriteFile("/sdcard/f", []byte("hello"), 0o660, time.Unix(1, 0))

	dev, err := common.Connect(ctx, &common.Config{ServerAddr: srv.Addr})
	require.NoError(t, err)

	pool := common.NewPool(ctx, dev, 2)
	s1, err := pool.Acquire(ctx)
	require.NoError(t, err)
	s2, err := pool.Acquire(ctx)
	require.NoError(t, err)

	// The third caller waits until a session is released.
	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err = pool.Acquire(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	pool.Release(s1)
	s3, err := pool.Acquire(ctx)
	require.NoError(t, err)
	require.Same(t, s1, s3, "healthy sessions are reused")

	// A broken session is dropped, the next Acquire opens a new one.
	r, err := s2.Recv("/sdcard/f")
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.True(t, s2.Broken())
	pool.Release(s2)
	s4, err := pool.Acquire(ctx)
	require.NoError(t, err)
	require.NotSame(t, s2, s4)
	require.False(t, s4.Broken())

	pool.Release(s3)
	pool.Release(s4)
	pool.Close()
	_, err = pool.Acquire(ctx)
	require.ErrorIs(t, err, common.ErrPoolClosed)

	// Cancelling the pool's context closes its sessions.
	pctx, pcancel := context.WithCancel(ctx)
	pool = common.NewPool(pctx, dev, 1)
	s5, err := pool.Acquire(ctx)
	require.NoError(t, err)
	pcancel()
	// The socket is closed from the context's own goroutine.
	require.Eventually(t, func() bool {
		_, err := s5.Lstat("/sdcard/f")
		return err != nil
	}, time.Second, 10*time.Millisecond)
	pool.Release(s5)
	_, err = pool.Acquire(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

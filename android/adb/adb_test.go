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

package adb_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/poolpOrg/plakar-integrations/android/adb"
	"github.com/poolpOrg/plakar-integrations/android/adb/adbtest"
	"github.com/stretchr/testify/require"
)

func TestParseDevices(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []adb.Device
		err  bool
	}{
		{name: "empty", in: ""},
		{
			name: "usb device",
			in:   "R58M12ABCDE            device usb:1-2 product:beyond1lte model:SM_G973F device:beyond1 transport_id:3\n",
			want: []adb.Device{{Serial: "R58M12ABCDE", State: "device", Product: "beyond1lte", Model: "SM_G973F", Name: "beyond1", TransportID: "3"}},
		},
		{
			name: "unauthorized and offline",
			in:   "ZY22ABCD               unauthorized usb:1-1 transport_id:1\nemulator-5554          offline transport_id:2\n",
			want: []adb.Device{
				{Serial: "ZY22ABCD", State: "unauthorized", TransportID: "1"},
				{Serial: "emulator-5554", State: "offline", TransportID: "2"},
			},
		},
		{
			name: "no permissions",
			in:   "0123456789ABCDEF       no permissions (user in plugdev group; are your udev rules wrong?); see [http://developer.android.com/tools/device.html] usb:3-1 transport_id:7\n",
			want: []adb.Device{{Serial: "0123456789ABCDEF", State: "no permissions", TransportID: "7"}},
		},
		{
			name: "tcp device",
			in:   "192.168.1.20:5555      device product:x model:Pixel_7 device:panther transport_id:9\n",
			want: []adb.Device{{Serial: "192.168.1.20:5555", State: "device", Product: "x", Model: "Pixel_7", Name: "panther", TransportID: "9"}},
		},
		{name: "malformed", in: "onlyserial\n", err: true},
		{name: "bad serial", in: "bad$serial device\n", err: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := adb.ParseDevices(tc.in)
			if tc.err {
				require.ErrorIs(t, err, adb.ErrProtocol)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseFeatures(t *testing.T) {
	f := adb.ParseFeatures("shell_v2,cmd,stat_v2, ls_v2 ,\n")
	require.True(t, f.Has("shell_v2"))
	require.True(t, f.Has("ls_v2"))
	require.False(t, f.Has("sendrecv_v2"))
	require.False(t, adb.Features(nil).Has("shell_v2"))
}

func TestValidateSerial(t *testing.T) {
	require.NoError(t, adb.ValidateSerial("R58M12ABCDE"))
	require.NoError(t, adb.ValidateSerial("emulator-5554"))
	require.NoError(t, adb.ValidateSerial("192.168.1.2:5555"))
	require.Error(t, adb.ValidateSerial(""))
	require.Error(t, adb.ValidateSerial("a b"))
	require.Error(t, adb.ValidateSerial("x\ny"))
	require.Error(t, adb.ValidateSerial("../etc"))
	require.Error(t, adb.ValidateSerial(strings.Repeat("a", 300)))
}

func TestFileModeRoundTrip(t *testing.T) {
	for _, mode := range []uint32{
		adb.S_IFREG | 0o644, adb.S_IFDIR | 0o771, adb.S_IFLNK | 0o777,
		adb.S_IFREG | adb.S_ISUID | 0o755, adb.S_IFDIR | adb.S_ISVTX | 0o1777&0o777,
		adb.S_IFCHR | 0o600, adb.S_IFBLK | 0o600, adb.S_IFIFO | 0o644, adb.S_IFSOCK | 0o644,
	} {
		m := adb.FileModeFromUnix(mode)
		require.Equal(t, mode, adb.UnixFromFileMode(m), "mode %o", mode)
	}
	require.True(t, adb.FileModeFromUnix(adb.S_IFDIR|0o755).IsDir())
	require.True(t, adb.FileModeFromUnix(adb.S_IFREG|0o644).IsRegular())
	require.NotZero(t, adb.FileModeFromUnix(adb.S_IFLNK|0o777)&fs.ModeSymlink)
	require.NotZero(t, adb.FileModeFromUnix(0)&fs.ModeIrregular)
}

func TestErrno(t *testing.T) {
	require.ErrorIs(t, adb.Errno(2), fs.ErrNotExist)
	require.ErrorIs(t, adb.Errno(13), fs.ErrPermission)
	require.ErrorIs(t, adb.Errno(17), fs.ErrExist)
	require.Equal(t, "permission denied", adb.Errno(13).Error())
	require.Equal(t, "errno 9999", adb.Errno(9999).Error())
}

func TestQuote(t *testing.T) {
	require.Equal(t, `'plain'`, adb.Quote("plain"))
	require.Equal(t, `'it'\''s'`, adb.Quote("it's"))
	require.Equal(t, `'$(rm -rf /); `+"`x`"+` "y"'`, adb.Quote(`$(rm -rf /); `+"`x`"+` "y"`))
}

func fakeDevice(t *testing.T, features []string) (*adbtest.Server, *adbtest.Device, *adb.Client) {
	t.Helper()
	srv := adbtest.New(t)
	dev := srv.AddDevice(&adbtest.Device{Serial: "FAKE0001", Model: "Fake_Phone", Features: features})
	return srv, dev, adb.NewClient(srv.Addr)
}

func TestHostServices(t *testing.T) {
	srv, _, client := fakeDevice(t, nil)
	ctx := context.Background()

	v, err := client.Version(ctx)
	require.NoError(t, err)
	require.Equal(t, "0029", v)

	devices, err := client.Devices(ctx)
	require.NoError(t, err)
	require.Len(t, devices, 1)
	require.Equal(t, "FAKE0001", devices[0].Serial)
	require.True(t, devices[0].Usable())
	require.Equal(t, "FAKE0001 (Fake Phone)", devices[0].Describe())

	f, err := client.Features(ctx, "FAKE0001")
	require.NoError(t, err)
	require.True(t, f.Has(adb.FeatureStatV2))

	_, err = client.Features(ctx, "NOPE")
	var aerr *adb.Error
	require.ErrorAs(t, err, &aerr)
	require.Contains(t, aerr.Message, "not found")

	srv.SetState("FAKE0001", adb.StateUnauthorized)
	_, err = client.Sync(ctx, "FAKE0001", f)
	require.ErrorAs(t, err, &aerr)
	require.Contains(t, aerr.Message, "unauthorized")
}

func TestServerUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close()

	_, err = adb.NewClient(addr).Version(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot reach the adb server")
}

func populate(dev *adbtest.Device) {
	ts := time.Unix(1700000000, 0)
	dev.FS.MkdirAll("/storage/emulated/0/DCIM/Camera", ts)
	dev.FS.WriteFile("/storage/emulated/0/DCIM/Camera/IMG_0001.jpg", bytes.Repeat([]byte("j"), 200*1024+17), 0o660, ts)
	dev.FS.WriteFile("/storage/emulated/0/Download/empty.txt", nil, 0o660, ts)
	dev.FS.WriteFile("/storage/emulated/0/Download/fichier é 日本 'quote'.txt", []byte("unicode"), 0o660, ts)
	dev.FS.WriteFile("/storage/emulated/0/Download/with,comma and space.bin", []byte("comma"), 0o660, ts)
	dev.FS.Symlink("/storage/emulated/0/link", "DCIM", ts)
	dev.FS.MkdirAll("/storage/emulated/0/Android/data/secret", ts)
}

func TestSyncStatAndList(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []string
	}{
		{"v2", adbtest.DefaultFeatures},
		{"v1", adbtest.LegacyFeatures},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, dev, client := fakeDevice(t, tc.features)
			populate(dev)
			ctx := context.Background()
			f := adb.ParseFeatures(strings.Join(tc.features, ","))

			s, err := client.Sync(ctx, "FAKE0001", f)
			require.NoError(t, err)
			defer s.Close()

			st, err := s.Lstat("/storage/emulated/0/DCIM/Camera/IMG_0001.jpg")
			require.NoError(t, err)
			require.True(t, st.IsRegular())
			require.EqualValues(t, 200*1024+17, st.Size)
			require.Equal(t, int64(1700000000), st.ModTime.Unix())

			st, err = s.Lstat("/storage/emulated/0/link")
			require.NoError(t, err)
			require.True(t, st.IsSymlink())

			_, err = s.Lstat("/storage/emulated/0/nope")
			require.ErrorIs(t, err, fs.ErrNotExist)

			if f.Has(adb.FeatureStatV2) {
				st, err = s.Stat("/storage/emulated/0/link")
				require.NoError(t, err)
				require.True(t, st.IsDir())
				require.NotZero(t, st.Ino)
			} else {
				_, err = s.Stat("/storage/emulated/0/link")
				require.ErrorIs(t, err, errors.ErrUnsupported)
			}

			var names []string
			readable, err := s.List("/storage/emulated/0/Download", func(d adb.Dirent) error {
				require.NoError(t, d.Err)
				names = append(names, d.Name)
				return nil
			})
			require.NoError(t, err)
			require.True(t, readable)
			require.Equal(t, []string{"empty.txt", "fichier é 日本 'quote'.txt", "with,comma and space.bin"}, names)

			readable, err = s.List("/storage/emulated/0/missing", func(adb.Dirent) error {
				t.Fatal("no entry expected")
				return nil
			})
			require.NoError(t, err)
			require.False(t, readable)

			// A callback error drains the listing and keeps the session usable.
			boom := errors.New("boom")
			_, err = s.List("/storage/emulated/0/Download", func(adb.Dirent) error { return boom })
			require.ErrorIs(t, err, boom)
			_, err = s.Lstat("/storage/emulated/0")
			require.NoError(t, err)
			require.False(t, s.Broken())
		})
	}
}

func TestSyncRecv(t *testing.T) {
	_, dev, client := fakeDevice(t, nil)
	populate(dev)
	ctx := context.Background()
	f := adb.ParseFeatures(strings.Join(adbtest.DefaultFeatures, ","))

	s, err := client.Sync(ctx, "FAKE0001", f)
	require.NoError(t, err)
	defer func() { s.Close() }() // s is reassigned below

	r, err := s.Recv("/storage/emulated/0/DCIM/Camera/IMG_0001.jpg")
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Len(t, data, 200*1024+17)
	require.True(t, r.Complete())
	require.EqualValues(t, len(data), r.Bytes())
	require.NoError(t, r.Close())

	r, err = s.Recv("/storage/emulated/0/Download/empty.txt")
	require.NoError(t, err)
	data, err = io.ReadAll(r)
	require.NoError(t, err)
	require.Empty(t, data)
	require.True(t, r.Complete())

	// Partial reads: tiny buffer, many Read calls.
	r, err = s.Recv("/storage/emulated/0/Download/fichier é 日本 'quote'.txt")
	require.NoError(t, err)
	var got []byte
	buf := make([]byte, 3)
	for {
		n, err := r.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}
	require.Equal(t, "unicode", string(got))

	// A missing file fails; adbd ends the session on any FAIL, so it
	// is reported broken and must not be reused.
	r, err = s.Recv("/storage/emulated/0/nope")
	require.NoError(t, err)
	_, err = io.ReadAll(r)
	var aerr *adb.Error
	require.ErrorAs(t, err, &aerr)
	require.Contains(t, aerr.Message, "open failed")
	require.True(t, s.Broken())
	require.NoError(t, s.Close())

	// A read error on the device, same.
	s, err = client.Sync(ctx, "FAKE0001", f)
	require.NoError(t, err)
	n, _ := dev.FS.Lstat("/storage/emulated/0/Download/with,comma and space.bin")
	n.ReadError = "Input/output error"
	r, err = s.Recv("/storage/emulated/0/Download/with,comma and space.bin")
	require.NoError(t, err)
	_, err = io.ReadAll(r)
	require.ErrorAs(t, err, &aerr)
	require.Contains(t, aerr.Message, "read failed")
	require.True(t, s.Broken())
	require.NoError(t, s.Close())

	// Abandoning a transfer breaks the session.
	s, err = client.Sync(ctx, "FAKE0001", f)
	require.NoError(t, err)
	r, err = s.Recv("/storage/emulated/0/DCIM/Camera/IMG_0001.jpg")
	require.NoError(t, err)
	_, err = r.Read(buf)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.True(t, s.Broken())
	_, err = s.Lstat("/")
	require.ErrorIs(t, err, adb.ErrBroken)
}

func TestSyncRecvDisconnect(t *testing.T) {
	_, dev, client := fakeDevice(t, nil)
	populate(dev)
	dev.DropRecvAfter = map[string]int64{"/storage/emulated/0/DCIM/Camera/IMG_0001.jpg": 70000}
	f := adb.ParseFeatures(strings.Join(adbtest.DefaultFeatures, ","))

	s, err := client.Sync(context.Background(), "FAKE0001", f)
	require.NoError(t, err)
	defer s.Close()

	r, err := s.Recv("/storage/emulated/0/DCIM/Camera/IMG_0001.jpg")
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.Error(t, err)
	require.False(t, r.Complete())
	require.Less(t, len(data), 200*1024)
	require.True(t, s.Broken())
}

func TestSyncSend(t *testing.T) {
	_, dev, client := fakeDevice(t, nil)
	f := adb.ParseFeatures(strings.Join(adbtest.DefaultFeatures, ","))
	s, err := client.Sync(context.Background(), "FAKE0001", f)
	require.NoError(t, err)
	defer func() { s.Close() }() // s is reassigned below

	mtime := time.Unix(1600000000, 0)
	big := bytes.Repeat([]byte("x"), 3*adb.SyncDataMax+5)
	require.NoError(t, s.Send("/sdcard/restore/a/b/big.bin", adb.S_IFREG|0o644, mtime, bytes.NewReader(big)))
	got, ok := dev.FS.ReadFile("/sdcard/restore/a/b/big.bin")
	require.True(t, ok)
	require.Equal(t, big, got)
	n, _ := dev.FS.Lstat("/sdcard/restore/a/b/big.bin")
	require.Equal(t, mtime.Unix(), n.ModTime.Unix())
	require.EqualValues(t, adb.S_IFREG|0o644, n.Mode)

	require.NoError(t, s.Send("/sdcard/restore/empty", adb.S_IFREG|0o600, mtime, bytes.NewReader(nil)))
	got, ok = dev.FS.ReadFile("/sdcard/restore/empty")
	require.True(t, ok)
	require.Empty(t, got)

	// Replacing an existing file.
	require.NoError(t, s.Send("/sdcard/restore/empty", adb.S_IFREG|0o600, mtime, strings.NewReader("new")))
	got, _ = dev.FS.ReadFile("/sdcard/restore/empty")
	require.Equal(t, "new", string(got))

	// A FAIL from the device ends the session, as adbd does.
	dev.FailSend = map[string]string{"/sdcard/restore/denied": "couldn't create file: Permission denied"}
	err = s.Send("/sdcard/restore/denied", adb.S_IFREG|0o600, mtime, strings.NewReader("x"))
	var aerr *adb.Error
	require.ErrorAs(t, err, &aerr)
	require.Contains(t, aerr.Message, "Permission denied")
	require.True(t, s.Broken())
	require.ErrorIs(t, s.Send("/sdcard/restore/again", adb.S_IFREG|0o600, mtime, strings.NewReader("x")), adb.ErrBroken)
	require.NoError(t, s.Close())

	// Symlinks on emulated storage are refused by the device.
	s, err = client.Sync(context.Background(), "FAKE0001", f)
	require.NoError(t, err)
	dev.FS.NoSymlinks = true
	err = s.Send("/sdcard/restore/lnk", adb.S_IFLNK|0o777, mtime, strings.NewReader("big.bin"))
	require.ErrorAs(t, err, &aerr)
	require.Contains(t, aerr.Message, "symlink failed")
	require.True(t, s.Broken())
	require.NoError(t, s.Close())

	// A reader failure aborts the session: the device is left waiting.
	s, err = client.Sync(context.Background(), "FAKE0001", f)
	require.NoError(t, err)
	err = s.Send("/sdcard/restore/short", adb.S_IFREG|0o600, mtime, io.MultiReader(strings.NewReader("abc"), failingReader{}))
	require.Error(t, err)
	require.True(t, s.Broken())
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("source read failed") }

func TestShell(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []string
	}{
		{"v2", adbtest.DefaultFeatures},
		{"v1", adbtest.LegacyFeatures},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, dev, client := fakeDevice(t, tc.features)
			populate(dev)
			ctx := context.Background()
			f := adb.ParseFeatures(strings.Join(tc.features, ","))

			res, err := client.Shell(ctx, "FAKE0001", f, "readlink", "--", "/storage/emulated/0/link")
			require.NoError(t, err)
			require.Equal(t, 0, res.ExitCode)
			require.Equal(t, "DCIM\n", string(res.Stdout))

			res, err = client.Shell(ctx, "FAKE0001", f, "readlink", "--", "/storage/emulated/0/nope")
			require.NoError(t, err)
			require.Equal(t, 1, res.ExitCode)

			evil := "/sdcard/x'; rm -rf /; echo '"
			res, err = client.Shell(ctx, "FAKE0001", f, "mkdir", "-p", "--", evil)
			require.NoError(t, err)
			require.Equal(t, 0, res.ExitCode)
			_, e := dev.FS.Lstat(evil)
			require.Zero(t, e, "the directory must be created with its literal name")
			cmds := srv.Counters.ShellCommands()
			require.Contains(t, cmds[len(cmds)-1], adb.Quote(evil))

			res, err = client.Shell(ctx, "FAKE0001", f, "nosuchcmd")
			require.NoError(t, err)
			require.Equal(t, 127, res.ExitCode)

			_, err = client.Shell(ctx, "FAKE0001", f)
			require.Error(t, err)
			_, err = client.Shell(ctx, "FAKE0001", f, "echo", "a\x00b")
			require.Error(t, err)
		})
	}
}

// rawServer runs handler on every accepted connection, for malformed
// reply tests.
func rawServer(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				handler(c)
			}()
		}
	}()
	return ln.Addr().String()
}

func readHostRequest(c net.Conn) string {
	var hdr [4]byte
	io.ReadFull(c, hdr[:])
	var n int
	for _, b := range hdr {
		n = n*16 + int(strings.IndexByte("0123456789abcdef", b))
	}
	buf := make([]byte, n)
	io.ReadFull(c, buf)
	return string(buf)
}

func TestMalformedReplies(t *testing.T) {
	ctx := context.Background()

	t.Run("bad status", func(t *testing.T) {
		addr := rawServer(t, func(c net.Conn) {
			readHostRequest(c)
			io.WriteString(c, "WHAT")
		})
		_, err := adb.NewClient(addr).Version(ctx)
		require.ErrorIs(t, err, adb.ErrProtocol)
	})

	t.Run("bad length", func(t *testing.T) {
		addr := rawServer(t, func(c net.Conn) {
			readHostRequest(c)
			io.WriteString(c, "OKAYzzzz")
		})
		_, err := adb.NewClient(addr).Version(ctx)
		require.ErrorIs(t, err, adb.ErrProtocol)
	})

	t.Run("truncated", func(t *testing.T) {
		addr := rawServer(t, func(c net.Conn) {
			readHostRequest(c)
			io.WriteString(c, "OKAY0010abc")
		})
		_, err := adb.NewClient(addr).Version(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "connection closed")
	})

	t.Run("oversized data chunk", func(t *testing.T) {
		addr := rawServer(t, func(c net.Conn) {
			readHostRequest(c) // transport
			io.WriteString(c, "OKAY")
			readHostRequest(c) // sync:
			io.WriteString(c, "OKAY")
			var hdr [8]byte
			io.ReadFull(c, hdr[:]) // RECV header
			n := binary.LittleEndian.Uint32(hdr[4:])
			io.ReadFull(c, make([]byte, n))
			var reply [8]byte
			copy(reply[:], "DATA")
			binary.LittleEndian.PutUint32(reply[4:], adb.SyncDataMax+1)
			c.Write(reply[:])
		})
		s, err := adb.NewClient(addr).Sync(ctx, "X", nil)
		require.NoError(t, err)
		r, err := s.Recv("/f")
		require.NoError(t, err)
		_, err = io.ReadAll(r)
		require.ErrorIs(t, err, adb.ErrProtocol)
		require.True(t, s.Broken())
	})

	t.Run("unexpected id in listing", func(t *testing.T) {
		addr := rawServer(t, func(c net.Conn) {
			readHostRequest(c)
			io.WriteString(c, "OKAY")
			readHostRequest(c)
			io.WriteString(c, "OKAY")
			var hdr [8]byte
			io.ReadFull(c, hdr[:])
			n := binary.LittleEndian.Uint32(hdr[4:])
			io.ReadFull(c, make([]byte, n))
			io.WriteString(c, "JUNKJUNK")
		})
		s, err := adb.NewClient(addr).Sync(ctx, "X", nil)
		require.NoError(t, err)
		_, err = s.List("/", func(adb.Dirent) error { return nil })
		require.ErrorIs(t, err, adb.ErrProtocol)
	})

	t.Run("entry name with slash", func(t *testing.T) {
		addr := rawServer(t, func(c net.Conn) {
			readHostRequest(c)
			io.WriteString(c, "OKAY")
			readHostRequest(c)
			io.WriteString(c, "OKAY")
			var hdr [8]byte
			io.ReadFull(c, hdr[:])
			n := binary.LittleEndian.Uint32(hdr[4:])
			io.ReadFull(c, make([]byte, n))
			name := "../escape"
			dent := make([]byte, 20)
			copy(dent, "DENT")
			binary.LittleEndian.PutUint32(dent[4:], adb.S_IFREG|0o644)
			binary.LittleEndian.PutUint32(dent[16:], uint32(len(name)))
			c.Write(dent)
			io.WriteString(c, name)
			var done [8]byte
			copy(done[:], "DONE")
			c.Write(done[:])
		})
		s, err := adb.NewClient(addr).Sync(ctx, "X", nil)
		require.NoError(t, err)
		var entries []adb.Dirent
		_, err = s.List("/", func(d adb.Dirent) error {
			entries = append(entries, d)
			return nil
		})
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.ErrorIs(t, entries[0].Err, adb.ErrProtocol)
	})
}

func TestContextCancellation(t *testing.T) {
	addr := rawServer(t, func(c net.Conn) {
		readHostRequest(c)
		// Never answer; wait for the client to go away.
		io.ReadAll(c)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := adb.NewClient(addr).Version(ctx)
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second)

	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, err = adb.NewClient(addr).Version(cancelled)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPathLimits(t *testing.T) {
	_, _, client := fakeDevice(t, nil)
	f := adb.ParseFeatures(strings.Join(adbtest.DefaultFeatures, ","))
	s, err := client.Sync(context.Background(), "FAKE0001", f)
	require.NoError(t, err)
	defer s.Close()

	_, err = s.Lstat("/" + strings.Repeat("a", adb.MaxPathLength))
	require.ErrorIs(t, err, adb.ErrNameTooLong)
	_, err = s.Lstat("/a\x00b")
	require.Error(t, err)
	require.False(t, s.Broken())
}

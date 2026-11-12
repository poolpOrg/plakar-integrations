/*
 * Copyright (c) 2026 Gilles Chehade <gilles@plakar.io>
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
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/poolpOrg/plakar-integrations/iphone/common"
	"github.com/poolpOrg/plakar-integrations/iphone/internal/faketools"
	"github.com/stretchr/testify/require"
	"howett.net/plist"
)

const (
	udidOld = "0123456789abcdef0123456789abcdef01234567"
	udidNew = "00008030-001A2B3C4D5E6F78"
)

func TestValidateUDID(t *testing.T) {
	for _, ok := range []string{udidOld, udidNew, "00008030-001a2b3c4d5e6f78"} {
		require.NoError(t, common.ValidateUDID(ok), ok)
	}
	for _, bad := range []string{"", "phone", "../x", udidOld + "0", "00008030-001A2B3C4D5E6F7", "00008030_001A2B3C4D5E6F78", "0123456789abcdef0123456789abcdef0123456g"} {
		require.Error(t, common.ValidateUDID(bad), bad)
	}
}

func TestSelectDevice(t *testing.T) {
	tests := []struct {
		name      string
		connected []string
		want      string
		udid      string
		err       error
	}{
		{name: "none", err: common.ErrNoDevice},
		{name: "one", connected: []string{udidOld}, udid: udidOld},
		{name: "many", connected: []string{udidOld, udidNew}, err: &common.MultipleDevicesError{}},
		{name: "picked", connected: []string{udidOld, udidNew}, want: udidNew, udid: udidNew},
		{name: "picked case-insensitive", connected: []string{udidNew}, want: "00008030-001a2b3c4d5e6f78", udid: udidNew},
		{name: "picked absent", connected: []string{udidOld}, want: udidNew, err: &common.DeviceNotFoundError{}},
		{name: "picked none connected", want: udidNew, err: &common.DeviceNotFoundError{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			udid, err := common.SelectDevice(tt.connected, tt.want)
			if tt.err != nil {
				require.Error(t, err)
				switch want := tt.err.(type) {
				case *common.MultipleDevicesError:
					var got *common.MultipleDevicesError
					require.ErrorAs(t, err, &got)
					require.Equal(t, tt.connected, got.UDIDs)
				case *common.DeviceNotFoundError:
					var got *common.DeviceNotFoundError
					require.ErrorAs(t, err, &got)
					require.Equal(t, tt.want, got.UDID)
				default:
					require.ErrorIs(t, err, want)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.udid, udid)
		})
	}
}

func TestParseLocation(t *testing.T) {
	tests := []struct {
		location string
		udid     string
		err      bool
	}{
		{location: "iphone://", udid: ""},
		{location: "iphone:///", udid: ""},
		{location: "iphone://" + udidOld, udid: udidOld},
		{location: "iphone://" + udidNew + "/", udid: udidNew},
		{location: "iphone:" + udidNew, udid: udidNew},
		{location: "iphone://myphone", err: true},
		{location: "iphone://" + udidOld + "/Manifest.db", err: true},
		{location: "iphone://../etc", err: true},
	}
	for _, tt := range tests {
		t.Run(tt.location, func(t *testing.T) {
			udid, err := common.ParseLocation("iphone", tt.location)
			if tt.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.udid, udid)
		})
	}
}

func TestResolveDeviceID(t *testing.T) {
	udid, err := common.ResolveDeviceID("", "")
	require.NoError(t, err)
	require.Empty(t, udid)

	udid, err = common.ResolveDeviceID(udidOld, "")
	require.NoError(t, err)
	require.Equal(t, udidOld, udid)

	udid, err = common.ResolveDeviceID("", udidNew)
	require.NoError(t, err)
	require.Equal(t, udidNew, udid)

	udid, err = common.ResolveDeviceID(udidNew, "00008030-001a2b3c4d5e6f78")
	require.NoError(t, err)
	require.Equal(t, "00008030-001a2b3c4d5e6f78", udid)

	_, err = common.ResolveDeviceID(udidOld, udidNew)
	require.Error(t, err)

	_, err = common.ResolveDeviceID("", "nope")
	require.Error(t, err)
}

func TestParsePairingTimeout(t *testing.T) {
	d, err := common.ParsePairingTimeout("")
	require.NoError(t, err)
	require.Equal(t, common.DefaultPairingTimeout, d)

	d, err = common.ParsePairingTimeout("5m")
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, d)

	_, err = common.ParsePairingTimeout("soon")
	require.Error(t, err)
	_, err = common.ParsePairingTimeout("-1s")
	require.Error(t, err)
}

func TestLookupToolsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := common.LookupTools()
	var missing *common.MissingToolsError
	require.ErrorAs(t, err, &missing)
	require.ElementsMatch(t, []string{common.IdeviceIDTool, common.IdevicePairTool, common.IdeviceBackupTool}, missing.Missing)
	require.Contains(t, err.Error(), "libimobiledevice")
}

func TestLookupToolsFound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bits")
	}
	bin := t.TempDir()
	for _, name := range []string{common.IdeviceIDTool, common.IdevicePairTool, common.IdeviceBackupTool} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0755))
	}
	t.Setenv("PATH", bin)
	tools, err := common.LookupTools()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(bin, common.IdeviceBackupTool), tools.Path(common.IdeviceBackupTool))
}

func TestValidateBackup(t *testing.T) {
	t.Run("finished", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, faketools.WriteBackup(root, udidNew, faketools.Layout{Full: true, DeviceName: "Pixie"}))
		info, err := common.ValidateBackup(root, udidNew)
		require.NoError(t, err)
		require.Equal(t, udidNew, info.UDID)
		require.Equal(t, filepath.Join(root, udidNew), info.Dir)
		require.Equal(t, common.SnapshotFinished, info.SnapshotState)
		require.False(t, info.Encrypted)
		require.True(t, info.IsFullBackup)
		require.Equal(t, "Pixie", info.DeviceName)
		require.Equal(t, "iPhone14,2", info.ProductType)
		require.Equal(t, "17.5.1", info.ProductVersion)
		require.Equal(t, "10.0", info.Version)
		require.WithinDuration(t, time.Now(), info.Date, time.Minute)
	})

	t.Run("encrypted", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, faketools.WriteBackup(root, udidOld, faketools.Layout{Encrypted: true}))
		info, err := common.ValidateBackup(root, udidOld)
		require.NoError(t, err)
		require.True(t, info.Encrypted)
	})

	t.Run("incomplete", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, faketools.WriteBackup(root, udidNew, faketools.Layout{State: "uploading"}))
		_, err := common.ValidateBackup(root, udidNew)
		var incomplete *common.IncompleteBackupError
		require.ErrorAs(t, err, &incomplete)
		require.Equal(t, "uploading", incomplete.State)
	})

	t.Run("missing status", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, faketools.WriteBackup(root, udidNew, faketools.Layout{}))
		require.NoError(t, os.Remove(filepath.Join(root, udidNew, common.StatusPlist)))
		_, err := common.ValidateBackup(root, udidNew)
		require.ErrorContains(t, err, common.StatusPlist)
	})

	t.Run("missing manifest db", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, faketools.WriteBackup(root, udidNew, faketools.Layout{}))
		require.NoError(t, os.Remove(filepath.Join(root, udidNew, common.ManifestDB)))
		_, err := common.ValidateBackup(root, udidNew)
		require.ErrorContains(t, err, common.ManifestDB)

		// the pre-iOS 10 index is accepted in its place
		require.NoError(t, os.WriteFile(filepath.Join(root, udidNew, common.ManifestMBDB), []byte("mbdb"), 0644))
		_, err = common.ValidateBackup(root, udidNew)
		require.NoError(t, err)
	})

	t.Run("other device", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, faketools.WriteBackup(root, udidNew, faketools.Layout{}))
		require.NoError(t, os.Rename(filepath.Join(root, udidNew), filepath.Join(root, udidOld)))
		_, err := common.ValidateBackup(root, udidOld)
		require.ErrorContains(t, err, "belongs to device")
	})

	t.Run("xml status", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, faketools.WriteBackup(root, udidNew, faketools.Layout{}))
		data, err := plist.Marshal(map[string]any{"SnapshotState": "finished"}, plist.XMLFormat)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(root, udidNew, common.StatusPlist), data, 0644))
		_, err = common.ValidateBackup(root, udidNew)
		require.NoError(t, err)
	})

	t.Run("garbage status", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, faketools.WriteBackup(root, udidNew, faketools.Layout{}))
		require.NoError(t, os.WriteFile(filepath.Join(root, udidNew, common.StatusPlist), []byte("not a plist"), 0644))
		_, err := common.ValidateBackup(root, udidNew)
		require.Error(t, err)
	})

	t.Run("absent", func(t *testing.T) {
		_, err := common.ValidateBackup(t.TempDir(), udidNew)
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("bad udid", func(t *testing.T) {
		_, err := common.ValidateBackup(t.TempDir(), "../..")
		require.Error(t, err)
	})
}

func TestBackupUDID(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, faketools.WriteBackup(root, udidNew, faketools.Layout{}))

	udid, err := common.BackupUDID(filepath.Join(root, udidNew))
	require.NoError(t, err)
	require.Equal(t, udidNew, udid)

	_, err = common.BackupUDID(root)
	require.ErrorIs(t, err, os.ErrNotExist)

	data, err := plist.Marshal(map[string]any{"Target Identifier": "../../etc"}, plist.XMLFormat)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, udidNew, common.InfoPlist), data, 0644))
	_, err = common.BackupUDID(filepath.Join(root, udidNew))
	require.ErrorContains(t, err, "Target Identifier")
}

func TestFindBackups(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, faketools.WriteBackup(root, udidNew, faketools.Layout{}))
	require.NoError(t, faketools.WriteBackup(root, udidOld, faketools.Layout{}))
	require.NoError(t, os.Mkdir(filepath.Join(root, "restore"), 0700))
	require.NoError(t, os.Mkdir(filepath.Join(root, "ffffffffffffffffffffffffffffffffffffffff"), 0700)) // no Info.plist
	require.NoError(t, os.WriteFile(filepath.Join(root, ".lock"), nil, 0600))

	udids, err := common.FindBackups(root)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{udidNew, udidOld}, udids)
}

func TestLock(t *testing.T) {
	path := common.LockPath(t.TempDir(), udidNew)

	l1, err := common.AcquireLock(path)
	require.NoError(t, err)

	_, err = common.AcquireLock(path)
	require.ErrorIs(t, err, common.ErrLocked)

	require.NoError(t, l1.Release())

	l2, err := common.AcquireLock(path)
	require.NoError(t, err)
	require.NoError(t, l2.Release())
	require.NoError(t, l2.Release(), "double release is harmless")

	var nilLock *common.Lock
	require.NoError(t, nilLock.Release())
}

func TestEnsurePaired(t *testing.T) {
	t.Run("already paired", func(t *testing.T) {
		fake := faketools.New(udidNew)
		require.NoError(t, fake.Tools().EnsurePaired(context.Background(), udidNew, time.Second, os.Stderr))
		require.Len(t, fake.Calls(common.IdevicePairTool), 1)
	})

	t.Run("denied", func(t *testing.T) {
		fake := faketools.New(udidNew)
		fake.Paired = nil
		fake.PairDenied = true
		err := fake.Tools().EnsurePaired(context.Background(), udidNew, 0, os.Stderr)
		var perr *common.PairingError
		require.ErrorAs(t, err, &perr)
		require.Contains(t, perr.Message, "denied the trust dialog")
		require.True(t, perr.Denied())
		require.Equal(t, udidNew, perr.UDID)
	})

	t.Run("accepted after a while", func(t *testing.T) {
		fake := faketools.New(udidNew)
		fake.Paired = nil
		fake.PairAttemptsPending = 1
		require.NoError(t, fake.Tools().EnsurePaired(context.Background(), udidNew, time.Minute, os.Stderr))
		var cmds []string
		for _, c := range fake.Calls(common.IdevicePairTool) {
			cmds = append(cmds, c.Args[len(c.Args)-1])
		}
		require.Equal(t, []string{"validate", "pair", "pair", "validate"}, cmds)
	})

	t.Run("timeout", func(t *testing.T) {
		fake := faketools.New(udidNew)
		fake.Paired = nil
		fake.PairAttemptsPending = 100
		err := fake.Tools().EnsurePaired(context.Background(), udidNew, 0, os.Stderr)
		var perr *common.PairingError
		require.ErrorAs(t, err, &perr)
		require.Contains(t, perr.Message, "accept the trust dialog")
		require.False(t, perr.Denied())
	})

	t.Run("not connected", func(t *testing.T) {
		fake := faketools.New(udidNew)
		err := fake.Tools().EnsurePaired(context.Background(), udidOld, 0, os.Stderr)
		var perr *common.PairingError
		require.ErrorAs(t, err, &perr)
	})

	t.Run("cancelled while waiting", func(t *testing.T) {
		fake := faketools.New(udidNew)
		fake.Paired = nil
		fake.PairAttemptsPending = 100
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := fake.Tools().EnsurePaired(ctx, udidNew, time.Minute, os.Stderr)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestListDevices(t *testing.T) {
	fake := faketools.New(udidNew, udidOld)
	udids, err := fake.Tools().ListDevices(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{udidNew, udidOld}, udids)

	fake = faketools.New()
	udids, err = fake.Tools().ListDevices(context.Background())
	require.NoError(t, err)
	require.Empty(t, udids)
}

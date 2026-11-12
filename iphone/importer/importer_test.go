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

package importer

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/poolpOrg/plakar-integrations/iphone/common"
	"github.com/poolpOrg/plakar-integrations/iphone/internal/faketools"
	"github.com/stretchr/testify/require"
)

const (
	udidA = "00008030-001A2B3C4D5E6F78"
	udidB = "0123456789abcdef0123456789abcdef01234567"
)

func newImporter(t *testing.T, fake *faketools.Fake, config map[string]string) (*Importer, *bytes.Buffer) {
	t.Helper()
	if config == nil {
		config = map[string]string{}
	}
	if _, ok := config["location"]; !ok {
		config["location"] = "iphone://"
	}
	if _, ok := config[common.ConfigWorkingDirectory]; !ok {
		config[common.ConfigWorkingDirectory] = t.TempDir()
	}
	stderr := &bytes.Buffer{}
	opts := &integration.Options{Hostname: "mac", MaxConcurrency: 2, Stderr: stderr}
	imp, err := New(context.Background(), opts, "iphone", config, fake.Tools())
	require.NoError(t, err)
	return imp, stderr
}

// drain runs Import and returns the records by pathname.
func drain(t *testing.T, ctx context.Context, imp *Importer) (map[string]*integration.Record, error) {
	t.Helper()
	records := make(chan *integration.Record, 16)
	done := make(chan error, 1)
	go func() { done <- imp.Import(ctx, records, nil) }()

	out := map[string]*integration.Record{}
	for rec := range records {
		out[rec.Pathname] = rec
	}
	return out, <-done
}

func TestNew(t *testing.T) {
	t.Run("single device auto-selected", func(t *testing.T) {
		imp, _ := newImporter(t, faketools.New(udidA), nil)
		require.Equal(t, udidA, imp.UDID())
		require.Equal(t, "/", imp.Root())
		require.Equal(t, udidA, imp.Origin())
		require.Equal(t, "iphone", imp.Type())
		require.Equal(t, location.FLAG_STREAM, imp.Flags())
	})

	t.Run("device from location", func(t *testing.T) {
		imp, _ := newImporter(t, faketools.New(udidA, udidB), map[string]string{"location": "iphone://" + udidB})
		require.Equal(t, udidB, imp.UDID())
	})

	t.Run("device from config", func(t *testing.T) {
		imp, _ := newImporter(t, faketools.New(udidA, udidB), map[string]string{common.ConfigDeviceID: udidB})
		require.Equal(t, udidB, imp.UDID())
	})

	t.Run("working directory is created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "nested", "staging")
		imp, _ := newImporter(t, faketools.New(udidA), map[string]string{common.ConfigWorkingDirectory: dir})
		require.Equal(t, dir, imp.Workdir())
		require.DirExists(t, dir)
	})

	fails := []struct {
		name   string
		fake   *faketools.Fake
		config map[string]string
		check  func(t *testing.T, err error)
	}{
		{
			name: "no device",
			fake: faketools.New(),
			check: func(t *testing.T, err error) {
				require.ErrorIs(t, err, common.ErrNoDevice)
			},
		},
		{
			name: "several devices",
			fake: faketools.New(udidA, udidB),
			check: func(t *testing.T, err error) {
				var e *common.MultipleDevicesError
				require.ErrorAs(t, err, &e)
				require.Contains(t, err.Error(), udidB)
			},
		},
		{
			name:   "selected device absent",
			fake:   faketools.New(udidA),
			config: map[string]string{"location": "iphone://" + udidB},
			check: func(t *testing.T, err error) {
				var e *common.DeviceNotFoundError
				require.ErrorAs(t, err, &e)
			},
		},
		{
			name:   "bad location",
			fake:   faketools.New(udidA),
			config: map[string]string{"location": "iphone://my-phone"},
			check:  func(t *testing.T, err error) { require.ErrorContains(t, err, "invalid device identifier") },
		},
		{
			name:   "conflicting device ids",
			fake:   faketools.New(udidA, udidB),
			config: map[string]string{"location": "iphone://" + udidA, common.ConfigDeviceID: udidB},
			check:  func(t *testing.T, err error) { require.ErrorContains(t, err, common.ConfigDeviceID) },
		},
		{
			name:   "bad full_backup",
			fake:   faketools.New(udidA),
			config: map[string]string{ConfigFullBackup: "maybe"},
			check:  func(t *testing.T, err error) { require.ErrorContains(t, err, ConfigFullBackup) },
		},
		{
			name:   "bad pairing_timeout",
			fake:   faketools.New(udidA),
			config: map[string]string{common.ConfigPairingTimeout: "later"},
			check:  func(t *testing.T, err error) { require.ErrorContains(t, err, common.ConfigPairingTimeout) },
		},
	}
	for _, tt := range fails {
		t.Run(tt.name, func(t *testing.T) {
			config := map[string]string{"location": "iphone://", common.ConfigWorkingDirectory: t.TempDir()}
			for k, v := range tt.config {
				config[k] = v
			}
			_, err := New(context.Background(), &integration.Options{}, "iphone", config, tt.fake.Tools())
			require.Error(t, err)
			tt.check(t, err)
		})
	}
}

func TestNewImporterMissingTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := NewImporter(context.Background(), &integration.Options{}, "iphone", map[string]string{"location": "iphone://"})
	var missing *common.MissingToolsError
	require.ErrorAs(t, err, &missing)
}

func TestImport(t *testing.T) {
	fake := faketools.New(udidA)
	imp, stderr := newImporter(t, fake, nil)

	records, err := drain(t, context.Background(), imp)
	require.NoError(t, err)

	for _, p := range []string{"/", "/" + udidA, "/" + udidA + "/Info.plist", "/" + udidA + "/Manifest.plist", "/" + udidA + "/Status.plist", "/" + udidA + "/Manifest.db", "/" + udidA + "/ab"} {
		require.Contains(t, records, p)
		require.NoError(t, records[p].Err)
	}
	require.True(t, records["/"].FileInfo.IsDir())
	require.True(t, records["/"+udidA].FileInfo.IsDir())
	require.True(t, records["/"+udidA+"/ab"].FileInfo.IsDir())

	for name, content := range faketools.Files {
		rec := records["/"+udidA+"/"+name]
		require.NotNil(t, rec, name)
		require.True(t, rec.FileInfo.Mode().IsRegular())
		require.Equal(t, int64(len(content)), rec.FileInfo.Size())
		data, err := io.ReadAll(rec.Reader)
		require.NoError(t, err)
		require.NoError(t, rec.Close())
		require.Equal(t, content, string(data))
	}

	backups := fake.Calls(common.IdeviceBackupTool)
	require.Len(t, backups, 1)
	require.Equal(t, []string{"-u", udidA, "backup", imp.Workdir()}, backups[0].Args)
	require.Contains(t, stderr.String(), "Backup Successful.")
	require.Contains(t, stderr.String(), "encrypted: false")

	// the working directory stays locked until Close, then is free again
	_, err = common.AcquireLock(common.LockPath(imp.Workdir(), udidA))
	require.ErrorIs(t, err, common.ErrLocked)
	require.NoError(t, imp.Close(context.Background()))
	l, err := common.AcquireLock(common.LockPath(imp.Workdir(), udidA))
	require.NoError(t, err)
	require.NoError(t, l.Release())
}

func TestImportMetadataPreserved(t *testing.T) {
	fake := faketools.New(udidA)
	imp, _ := newImporter(t, fake, nil)
	records, err := drain(t, context.Background(), imp)
	require.NoError(t, err)
	defer imp.Close(context.Background())

	for name := range faketools.Files {
		path := filepath.Join(imp.Workdir(), udidA, filepath.FromSlash(name))
		st, err := os.Stat(path)
		require.NoError(t, err)
		rec := records["/"+udidA+"/"+name]
		require.Equal(t, st.Size(), rec.FileInfo.Size())
		require.True(t, st.ModTime().Equal(rec.FileInfo.ModTime()), name)
		require.Equal(t, st.Mode().Perm(), rec.FileInfo.Mode().Perm())
		require.Equal(t, filepath.Base(name), rec.FileInfo.Name())
	}
}

func TestImportIncremental(t *testing.T) {
	fake := faketools.New(udidA)
	workdir := t.TempDir()

	imp1, _ := newImporter(t, fake, map[string]string{common.ConfigWorkingDirectory: workdir})
	_, err := drain(t, context.Background(), imp1)
	require.NoError(t, err)
	require.NoError(t, imp1.Close(context.Background()))
	info, err := common.ValidateBackup(workdir, udidA)
	require.NoError(t, err)
	require.True(t, info.IsFullBackup, "first run has nothing to build on")

	// a second run reuses the same directory and the tool is not asked for
	// a full backup
	imp2, _ := newImporter(t, fake, map[string]string{common.ConfigWorkingDirectory: workdir})
	_, err = drain(t, context.Background(), imp2)
	require.NoError(t, err)
	require.NoError(t, imp2.Close(context.Background()))
	info, err = common.ValidateBackup(workdir, udidA)
	require.NoError(t, err)
	require.False(t, info.IsFullBackup)

	// full_backup forces it
	imp3, _ := newImporter(t, fake, map[string]string{common.ConfigWorkingDirectory: workdir, ConfigFullBackup: "true"})
	_, err = drain(t, context.Background(), imp3)
	require.NoError(t, err)
	require.NoError(t, imp3.Close(context.Background()))
	info, err = common.ValidateBackup(workdir, udidA)
	require.NoError(t, err)
	require.True(t, info.IsFullBackup)

	calls := fake.Calls(common.IdeviceBackupTool)
	require.Len(t, calls, 3)
	require.NotContains(t, calls[0].Args, "--full")
	require.NotContains(t, calls[1].Args, "--full")
	require.Contains(t, calls[2].Args, "--full")
	for _, c := range calls {
		require.Equal(t, workdir, c.Args[len(c.Args)-1])
	}
}

func TestImportPairingDenied(t *testing.T) {
	fake := faketools.New(udidA)
	fake.Paired = nil
	fake.PairDenied = true
	imp, stderr := newImporter(t, fake, nil)

	records, err := drain(t, context.Background(), imp)
	var perr *common.PairingError
	require.ErrorAs(t, err, &perr)
	require.Contains(t, err.Error(), "denied the trust dialog")
	require.Empty(t, records)
	require.Empty(t, fake.Calls(common.IdeviceBackupTool), "no backup is attempted on an untrusted host")
	require.Contains(t, stderr.String(), "accept the Trust dialog")
	require.NoError(t, imp.Close(context.Background()))
}

func TestImportPairingAccepted(t *testing.T) {
	fake := faketools.New(udidA)
	fake.Paired = nil
	fake.PairAttemptsPending = 1
	imp, _ := newImporter(t, fake, map[string]string{common.ConfigPairingTimeout: "30s"})

	_, err := drain(t, context.Background(), imp)
	require.NoError(t, err)
	require.NoError(t, imp.Close(context.Background()))

	var cmds []string
	for _, c := range fake.Calls(common.IdevicePairTool) {
		cmds = append(cmds, c.Args[len(c.Args)-1])
	}
	require.Equal(t, []string{"validate", "pair", "pair", "validate"}, cmds)
	require.Len(t, fake.Calls(common.IdeviceBackupTool), 1)
}

func TestImportBackupFailure(t *testing.T) {
	fake := faketools.New(udidA)
	fake.BackupExit = 255
	imp, _ := newImporter(t, fake, nil)

	records, err := drain(t, context.Background(), imp)
	var terr *common.ToolError
	require.ErrorAs(t, err, &terr)
	require.Equal(t, 255, terr.ExitCode)
	require.Contains(t, err.Error(), "Backup Failed")
	require.Empty(t, records, "nothing is exposed from a failed backup")
	require.NoError(t, imp.Close(context.Background()))
}

func TestImportIncompleteBackup(t *testing.T) {
	fake := faketools.New(udidA)
	fake.BackupState = "uploading"
	imp, _ := newImporter(t, fake, nil)

	records, err := drain(t, context.Background(), imp)
	var incomplete *common.IncompleteBackupError
	require.ErrorAs(t, err, &incomplete)
	require.Empty(t, records, "an unfinished backup is never exposed")
	require.NoError(t, imp.Close(context.Background()))
}

func TestImportCancellation(t *testing.T) {
	fake := faketools.New(udidA)
	fake.BackupBlocks = true
	imp, _ := newImporter(t, fake, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	records, err := drain(t, ctx, imp)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, records)
	require.NoError(t, imp.Close(context.Background()))
}

func TestImportTwice(t *testing.T) {
	imp, _ := newImporter(t, faketools.New(udidA), nil)
	_, err := drain(t, context.Background(), imp)
	require.NoError(t, err)
	_, err = drain(t, context.Background(), imp)
	require.ErrorContains(t, err, "twice")
	require.NoError(t, imp.Close(context.Background()))
}

func TestImportConcurrent(t *testing.T) {
	fake := faketools.New(udidA)
	workdir := t.TempDir()
	config := func() map[string]string { return map[string]string{common.ConfigWorkingDirectory: workdir} }

	first, _ := newImporter(t, fake, config())
	_, err := drain(t, context.Background(), first)
	require.NoError(t, err)

	second, _ := newImporter(t, fake, config())
	_, err = drain(t, context.Background(), second)
	require.ErrorIs(t, err, common.ErrLocked)
	require.Len(t, fake.Calls(common.IdeviceBackupTool), 1, "the second run never reached the device")
	require.NoError(t, second.Close(context.Background()))

	require.NoError(t, first.Close(context.Background()))

	third, _ := newImporter(t, fake, config())
	_, err = drain(t, context.Background(), third)
	require.NoError(t, err)
	require.NoError(t, third.Close(context.Background()))
}

func TestImportSkipsOddFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges")
	}
	fake := faketools.New(udidA)
	workdir := t.TempDir()
	// a leftover from a previous run that is not a regular file
	require.NoError(t, faketools.WriteBackup(workdir, udidA, faketools.Layout{}))
	require.NoError(t, os.Symlink("Manifest.db", filepath.Join(workdir, udidA, "link")))

	imp, _ := newImporter(t, fake, map[string]string{common.ConfigWorkingDirectory: workdir})
	records, err := drain(t, context.Background(), imp)
	require.NoError(t, err)
	require.NoError(t, imp.Close(context.Background()))

	rec := records["/"+udidA+"/link"]
	require.NotNil(t, rec)
	require.Error(t, rec.Err)
}

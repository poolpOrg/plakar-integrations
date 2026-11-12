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

package exporter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/poolpOrg/plakar-integrations/iphone/common"
	"github.com/poolpOrg/plakar-integrations/iphone/internal/faketools"
	"github.com/stretchr/testify/require"
)

const (
	udidA = "00008030-001A2B3C4D5E6F78" // target device
	udidB = "0123456789abcdef0123456789abcdef01234567"
)

// snapshotRecords turns a backup directory on disk into the record stream the
// importer would have produced for it: "/" then "/<udid>/...".
func snapshotRecords(t *testing.T, root, udid string) []*integration.Record {
	t.Helper()
	var records []*integration.Record
	records = append(records, integration.NewRecord("/", "", integration.FileInfo{Lname: "/", Lmode: os.ModeDir | 0700, LmodTime: time.Now()}, nil, nil))

	dir := filepath.Join(root, udid)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		rel, err := filepath.Rel(dir, path)
		require.NoError(t, err)
		pathname := "/" + udid
		if rel != "." {
			pathname += "/" + filepath.ToSlash(rel)
		}
		info, err := d.Info()
		require.NoError(t, err)
		fi := integration.FileInfoFromStat(info)
		if info.IsDir() {
			records = append(records, integration.NewRecord(pathname, "", fi, nil, nil))
		} else {
			records = append(records, integration.NewRecord(pathname, "", fi, nil, func() (io.ReadCloser, error) {
				return os.Open(path)
			}))
		}
		return nil
	})
	require.NoError(t, err)
	return records
}

func newExporter(t *testing.T, fake *faketools.Fake, config map[string]string) (*Exporter, *bytes.Buffer) {
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
	var tools *common.Tools
	if fake != nil {
		tools = fake.Tools()
	}
	exp, err := New(context.Background(), opts, "iphone", config, tools)
	require.NoError(t, err)
	// Tests that stop halfway never reach Close; the staging root must still
	// be closed before the temporary directory is removed, or Windows
	// refuses the removal.
	t.Cleanup(func() { exp.Close(context.Background()) })
	return exp, stderr
}

// export feeds the records through Export and returns the results by
// pathname.
func export(t *testing.T, ctx context.Context, exp *Exporter, records []*integration.Record) (map[string]*integration.Result, error) {
	t.Helper()
	in := make(chan *integration.Record)
	out := make(chan *integration.Result, len(records)+1)
	done := make(chan error, 1)
	go func() { done <- exp.Export(ctx, in, out) }()
	for _, r := range records {
		in <- r
	}
	close(in)
	err := <-done
	results := map[string]*integration.Result{}
	for res := range out {
		results[res.Record.Pathname] = res
	}
	return results, err
}

func backupDir(t *testing.T, udid string, layout faketools.Layout) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, faketools.WriteBackup(root, udid, layout))
	return root
}

func TestExportWritesDirectory(t *testing.T) {
	source := backupDir(t, udidB, faketools.Layout{})
	records := snapshotRecords(t, source, udidB)

	exp, stderr := newExporter(t, nil, map[string]string{"location": "iphone://" + udidA})
	require.Equal(t, "iphone", exp.Type())
	require.Equal(t, "mac", exp.Origin())
	require.Equal(t, exp.Staging(), exp.Root())
	require.NoError(t, exp.Ping(context.Background()))

	results, err := export(t, context.Background(), exp, records)
	require.NoError(t, err)
	require.Len(t, results, len(records))
	for p, res := range results {
		require.NoError(t, res.Err, p)
	}
	require.NoError(t, exp.Close(context.Background()))

	// same layout, same content, same mtimes
	info, err := common.ValidateBackup(exp.Staging(), udidB)
	require.NoError(t, err)
	require.Equal(t, udidB, info.UDID)
	for name, content := range faketools.Files {
		got, err := os.ReadFile(filepath.Join(exp.Staging(), udidB, filepath.FromSlash(name)))
		require.NoError(t, err)
		require.Equal(t, content, string(got))
		want, err := os.Stat(filepath.Join(source, udidB, filepath.FromSlash(name)))
		require.NoError(t, err)
		st, err := os.Stat(filepath.Join(exp.Staging(), udidB, filepath.FromSlash(name)))
		require.NoError(t, err)
		require.True(t, want.ModTime().Equal(st.ModTime()), name)
	}
	entries, err := os.ReadDir(filepath.Join(exp.Staging(), udidB, "ab"))
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasPrefix(e.Name(), ".plakar-"), "no temporary file left behind")
	}

	// without confirm nothing touches a device, the user gets the command
	require.Contains(t, stderr.String(), "nothing was sent to a device")
	require.Contains(t, stderr.String(), "idevicebackup2 -u "+udidA+" -s "+udidB+" restore "+exp.Staging())
	require.Contains(t, stderr.String(), "-o confirm=true")
}

func TestExportConfirmRestores(t *testing.T) {
	source := backupDir(t, udidB, faketools.Layout{Encrypted: true})
	records := snapshotRecords(t, source, udidB)

	fake := faketools.New(udidA)
	exp, stderr := newExporter(t, fake, map[string]string{
		ConfigConfirm:         "true",
		ConfigBackupPassword:  "hunter2-secret",
		ConfigRestoreSettings: "true",
		ConfigNoReboot:        "true",
	})
	staging := exp.Staging()

	_, err := export(t, context.Background(), exp, records)
	require.NoError(t, err)
	require.NoError(t, exp.Close(context.Background()))

	restores := fake.Calls(common.IdeviceBackupTool)
	require.Len(t, restores, 1)
	require.Equal(t, []string{"-u", udidA, "-s", udidB, "restore", "--settings", "--no-reboot", staging}, restores[0].Args)
	require.Equal(t, []string{"BACKUP_PASSWORD=hunter2-secret"}, restores[0].Env)

	// the secret shows up nowhere but the tool's environment
	require.NotContains(t, strings.Join(restores[0].Args, " "), "hunter2-secret")
	require.NotContains(t, stderr.String(), "hunter2-secret")

	require.Contains(t, stderr.String(), "Restore Successful.")
	require.NoDirExists(t, staging, "the materialized copy is removed once it is on the device")
}

func TestExportPasswordFromEnvironment(t *testing.T) {
	t.Setenv(PasswordEnv, "from-env-secret")
	source := backupDir(t, udidB, faketools.Layout{Encrypted: true})

	fake := faketools.New(udidA)
	exp, stderr := newExporter(t, fake, map[string]string{ConfigConfirm: "true"})
	_, err := export(t, context.Background(), exp, snapshotRecords(t, source, udidB))
	require.NoError(t, err)
	require.NoError(t, exp.Close(context.Background()))

	restores := fake.Calls(common.IdeviceBackupTool)
	require.Len(t, restores, 1)
	require.Equal(t, []string{"BACKUP_PASSWORD=from-env-secret"}, restores[0].Env)
	require.NotContains(t, stderr.String(), "from-env-secret")
}

func TestExportEncryptedNeedsPassword(t *testing.T) {
	source := backupDir(t, udidB, faketools.Layout{Encrypted: true})
	fake := faketools.New(udidA)
	exp, _ := newExporter(t, fake, map[string]string{ConfigConfirm: "true"})

	_, err := export(t, context.Background(), exp, snapshotRecords(t, source, udidB))
	require.ErrorContains(t, err, ConfigBackupPassword)
	require.NoError(t, exp.Close(context.Background()))
	require.Empty(t, fake.Calls(common.IdeviceBackupTool))
	require.DirExists(t, exp.Staging(), "kept for the user to restore by hand")
}

// A restore of "<snapshot>:/<udid>" arrives without the <udid> directory;
// the files are put back under it before anything else happens.
func TestExportRelocatesStrippedSnapshot(t *testing.T) {
	source := backupDir(t, udidB, faketools.Layout{})
	fake := faketools.New(udidA)
	exp, _ := newExporter(t, fake, map[string]string{ConfigConfirm: "true"})
	staging := exp.Staging()

	var records []*integration.Record
	for _, r := range snapshotRecords(t, source, udidB) {
		if r.Pathname == "/" {
			continue
		}
		r.Pathname = strings.TrimPrefix(r.Pathname, "/"+udidB)
		if r.Pathname == "" {
			r.Pathname = "/"
		}
		records = append(records, r)
	}

	_, err := export(t, context.Background(), exp, records)
	require.NoError(t, err)
	require.NoError(t, exp.Close(context.Background()))

	restores := fake.Calls(common.IdeviceBackupTool)
	require.Len(t, restores, 1)
	require.Equal(t, []string{"-u", udidA, "-s", udidB, "restore", staging}, restores[0].Args)
}

func TestExportRestoreFailure(t *testing.T) {
	source := backupDir(t, udidB, faketools.Layout{})
	fake := faketools.New(udidA)
	fake.RestoreExit = 1
	exp, _ := newExporter(t, fake, map[string]string{ConfigConfirm: "true", ConfigBackupPassword: "hunter2-secret"})

	_, err := export(t, context.Background(), exp, snapshotRecords(t, source, udidB))
	var terr *common.ToolError
	require.ErrorAs(t, err, &terr)
	require.Contains(t, err.Error(), "Restore Failed")
	require.NotContains(t, err.Error(), "hunter2-secret")
	require.NoError(t, exp.Close(context.Background()))
	require.DirExists(t, exp.Staging())
}

func TestExportRefusesIncompleteBackup(t *testing.T) {
	source := backupDir(t, udidB, faketools.Layout{State: "uploading"})
	fake := faketools.New(udidA)
	exp, _ := newExporter(t, fake, map[string]string{ConfigConfirm: "true"})

	_, err := export(t, context.Background(), exp, snapshotRecords(t, source, udidB))
	var incomplete *common.IncompleteBackupError
	require.ErrorAs(t, err, &incomplete)
	require.NoError(t, exp.Close(context.Background()))
	require.Empty(t, fake.Calls(common.IdeviceBackupTool))
}

func TestExportRefusesPartialSnapshot(t *testing.T) {
	source := backupDir(t, udidB, faketools.Layout{})
	fake := faketools.New(udidA)
	exp, _ := newExporter(t, fake, map[string]string{ConfigConfirm: "true"})

	// a restore of "<snap>:/<udid>/ab" arrives stripped, without the manifests
	var records []*integration.Record
	for _, r := range snapshotRecords(t, source, udidB) {
		if r.Pathname == "/" || strings.HasPrefix(r.Pathname, "/"+udidB+"/ab") {
			r.Pathname = strings.TrimPrefix(r.Pathname, "/"+udidB)
			records = append(records, r)
		}
	}
	_, err := export(t, context.Background(), exp, records)
	require.ErrorContains(t, err, "exactly one device backup")
	require.NoError(t, exp.Close(context.Background()))
	require.Empty(t, fake.Calls(common.IdeviceBackupTool))
}

func TestExportRecordFailureBlocksRestore(t *testing.T) {
	source := backupDir(t, udidB, faketools.Layout{})
	fake := faketools.New(udidA)
	exp, _ := newExporter(t, fake, map[string]string{ConfigConfirm: "true"})

	records := snapshotRecords(t, source, udidB)
	broken := integration.NewRecord("/"+udidB+"/ab/broken", "", integration.FileInfo{Lname: "broken", Lmode: 0644}, nil, func() (io.ReadCloser, error) {
		return nil, errors.New("read failed")
	})
	records = append(records, broken)

	results, err := export(t, context.Background(), exp, records)
	require.ErrorContains(t, err, "1 records failed")
	require.Error(t, results["/"+udidB+"/ab/broken"].Err)
	require.NoFileExists(t, filepath.Join(exp.Staging(), udidB, "ab", "broken"))
	require.NoError(t, exp.Close(context.Background()))
	require.Empty(t, fake.Calls(common.IdeviceBackupTool))
}

func TestExportRejectsUnexpectedRecords(t *testing.T) {
	exp, _ := newExporter(t, nil, nil)
	records := []*integration.Record{
		integration.NewRecord("/"+udidB+"/link", "Manifest.db", integration.FileInfo{Lname: "link", Lmode: os.ModeSymlink | 0777}, nil, nil),
		integration.NewRecord("/"+udidB+"/../escape", "", integration.FileInfo{Lname: "escape", Lmode: 0644}, nil, func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("x")), nil
		}),
		integration.NewError("/"+udidB+"/missing", errors.New("source error")),
		integration.NewXattr("/"+udidB+"/Manifest.db", "com.apple.quarantine", integration.AttributeExtended, nil),
	}
	results, err := export(t, context.Background(), exp, records)
	require.ErrorContains(t, err, "2 records failed")
	require.Error(t, results["/"+udidB+"/link"].Err)
	require.Error(t, results["/"+udidB+"/../escape"].Err)
	require.NoError(t, results["/"+udidB+"/missing"].Err, "source errors are acknowledged, plakar already reported them")
	require.NoError(t, results["/"+udidB+"/Manifest.db"].Err, "xattrs are ignored")
	require.NoFileExists(t, filepath.Join(filepath.Dir(exp.Staging()), "escape"))
}

func TestExportCancellation(t *testing.T) {
	exp, _ := newExporter(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := make(chan *integration.Record)
	out := make(chan *integration.Result, 1)
	err := exp.Export(ctx, in, out)
	require.ErrorIs(t, err, context.Canceled)
}

func TestExportCloseWithoutExport(t *testing.T) {
	exp, _ := newExporter(t, nil, nil)
	staging := exp.Staging()
	require.NoError(t, exp.Close(context.Background()))
	require.NoDirExists(t, staging)
}

func TestNewConfirmNeedsDevice(t *testing.T) {
	config := map[string]string{"location": "iphone://", common.ConfigWorkingDirectory: t.TempDir(), ConfigConfirm: "true"}
	_, err := New(context.Background(), &integration.Options{}, "iphone", config, faketools.New().Tools())
	require.ErrorIs(t, err, common.ErrNoDevice)

	config["location"] = "iphone://" + udidB
	_, err = New(context.Background(), &integration.Options{}, "iphone", config, faketools.New(udidA).Tools())
	var notFound *common.DeviceNotFoundError
	require.ErrorAs(t, err, &notFound)

	t.Setenv("PATH", t.TempDir())
	_, err = NewExporter(context.Background(), &integration.Options{}, "iphone", config)
	var missing *common.MissingToolsError
	require.ErrorAs(t, err, &missing)
}

func TestNewBadConfig(t *testing.T) {
	for key, value := range map[string]string{
		ConfigConfirm:               "sure",
		ConfigNoReboot:              "later",
		common.ConfigPairingTimeout: "never",
		"location":                  "iphone://this-one",
		common.ConfigDeviceID:       "nope",
	} {
		config := map[string]string{"location": "iphone://", common.ConfigWorkingDirectory: t.TempDir(), key: value}
		_, err := New(context.Background(), &integration.Options{}, "iphone", config, nil)
		require.Error(t, err, key)
	}
}

func TestNewWithoutConfirmNeedsNoTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	config := map[string]string{"location": "iphone://" + udidA, common.ConfigWorkingDirectory: t.TempDir()}
	exp, err := NewExporter(context.Background(), &integration.Options{}, "iphone", config)
	require.NoError(t, err)
	require.NoError(t, exp.Close(context.Background()))
}

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

package common

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testUDID = "00008030-001A2B3C4D5E6F78"

// scripted is a RunFunc that records invocations and answers with a fixed
// result.
type scripted struct {
	invocations []*Invocation
	result      *Result
	err         error
}

func (s *scripted) run(ctx context.Context, inv *Invocation) (*Result, error) {
	s.invocations = append(s.invocations, inv)
	return s.result, s.err
}

func newScriptedTools(s *scripted) *Tools {
	return NewTools(map[string]string{
		IdeviceIDTool:     "/t/idevice_id",
		IdevicePairTool:   "/t/idevicepair",
		IdeviceBackupTool: "/t/idevicebackup2",
	}, s.run)
}

func TestBackupInvocation(t *testing.T) {
	s := &scripted{result: &Result{}}
	tools := newScriptedTools(s)

	require.NoError(t, tools.Backup(context.Background(), testUDID, "/work", false, nil))
	require.NoError(t, tools.Backup(context.Background(), testUDID, "/work", true, nil))

	require.Len(t, s.invocations, 2)
	require.Equal(t, "/t/idevicebackup2", s.invocations[0].Path)
	require.Equal(t, []string{"-u", testUDID, "backup", "/work"}, s.invocations[0].Args)
	require.Equal(t, []string{"-u", testUDID, "backup", "--full", "/work"}, s.invocations[1].Args)
	require.Empty(t, s.invocations[0].Env)
}

func TestBackupFailure(t *testing.T) {
	s := &scripted{result: &Result{ExitCode: 255, Output: []byte("Backup directory is \"/work\"\n\x1b[2K\rBackup     [....] 0%\nBackup Failed (Error Code 1).\n")}}
	err := newScriptedTools(s).Backup(context.Background(), testUDID, "/work", false, nil)
	var terr *ToolError
	require.ErrorAs(t, err, &terr)
	require.Equal(t, 255, terr.ExitCode)
	require.Equal(t, IdeviceBackupTool, terr.Tool)
	require.Contains(t, terr.Output, "Backup Failed (Error Code 1).")
	require.NotContains(t, terr.Output, "\x1b")
}

// The password reaches idevicebackup2 through its environment only: never in
// the arguments, never in an error.
func TestRestoreInvocationKeepsPasswordOutOfArgs(t *testing.T) {
	s := &scripted{result: &Result{ExitCode: 1, Output: []byte("Restore Failed (Error Code 1).\n")}}
	tools := newScriptedTools(s)

	opts := RestoreOptions{
		TargetUDID: testUDID,
		SourceUDID: "0123456789abcdef0123456789abcdef01234567",
		Dir:        "/work/restore-1",
		Password:   "hunter2-secret",
		System:     true,
		Settings:   true,
		Remove:     true,
		NoReboot:   true,
		SkipApps:   true,
	}
	err := tools.Restore(context.Background(), opts, nil)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "hunter2-secret")

	require.Len(t, s.invocations, 1)
	inv := s.invocations[0]
	require.Equal(t, []string{
		"-u", testUDID, "-s", opts.SourceUDID, "restore",
		"--system", "--settings", "--remove", "--no-reboot", "--skip-apps",
		"/work/restore-1",
	}, inv.Args)
	require.Equal(t, []string{"BACKUP_PASSWORD=hunter2-secret"}, inv.Env)
	require.NotContains(t, strings.Join(inv.Args, " "), "hunter2-secret")

	s.invocations = nil
	s.result = &Result{}
	opts.Password = ""
	require.NoError(t, tools.Restore(context.Background(), opts, nil))
	require.Empty(t, s.invocations[0].Env)
}

func TestMissingToolInvocation(t *testing.T) {
	tools := NewTools(map[string]string{}, (&scripted{result: &Result{}}).run)
	_, err := tools.ListDevices(context.Background())
	var missing *MissingToolsError
	require.ErrorAs(t, err, &missing)
	require.Equal(t, []string{IdeviceIDTool}, missing.Missing)
}

func TestLastLines(t *testing.T) {
	out := "first\n\x1b[s\x1b[1A\x1b[2K\rBackup     [###...] 42%\r\n\nStatus     Receiving files\nlast\n"
	require.Equal(t, "Backup     [###...] 42%; Status     Receiving files; last", lastLines([]byte(out), 3))
	require.Equal(t, "", lastLines(nil, 3))
}

func TestTailWriter(t *testing.T) {
	w := &tailWriter{max: 4}
	w.Write([]byte("abc"))
	w.Write([]byte("defgh"))
	require.Equal(t, "efgh", string(w.buf))
}

func TestExecRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	sh, err := exec.LookPath("sh")
	require.NoError(t, err)

	t.Run("captured", func(t *testing.T) {
		res, err := execRun(context.Background(), &Invocation{
			Path: sh,
			Args: []string{"-c", "echo out; echo err >&2; echo $PLAKAR_TEST_ENV; exit 3"},
			Env:  []string{"PLAKAR_TEST_ENV=fromenv"},
		})
		require.NoError(t, err)
		require.Equal(t, 3, res.ExitCode)
		require.Equal(t, "out\nfromenv\n", string(res.Stdout))
		require.Contains(t, string(res.Output), "err\n")
		require.Contains(t, string(res.Output), "out\n")
	})

	t.Run("streamed", func(t *testing.T) {
		var stream bytes.Buffer
		res, err := execRun(context.Background(), &Invocation{
			Path:   sh,
			Args:   []string{"-c", "echo progress; echo warn >&2"},
			Stream: &stream,
		})
		require.NoError(t, err)
		require.Equal(t, 0, res.ExitCode)
		require.Empty(t, res.Stdout)
		require.Contains(t, stream.String(), "progress\n")
		require.Contains(t, stream.String(), "warn\n")
		require.Contains(t, string(res.Output), "progress\n")
	})

	t.Run("not found", func(t *testing.T) {
		_, err := execRun(context.Background(), &Invocation{Path: "/nonexistent/idevicebackup2"})
		require.Error(t, err)
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := execRun(ctx, &Invocation{Path: sh, Args: []string{"-c", "exec sleep 30"}})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, time.Since(start), 5*time.Second)
	})
}

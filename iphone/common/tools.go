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

// Package common drives the libimobiledevice command line tools that the
// iphone importer and exporter rely on, and validates the backup directories
// they produce.
//
// Everything that talks to a device goes through [Tools], which resolves the
// executables once and runs them through a [RunFunc]. Tests substitute their
// own RunFunc so no device, and no libimobiledevice install, is needed.
package common

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// The libimobiledevice executables this integration depends on.
const (
	IdeviceIDTool     = "idevice_id"
	IdevicePairTool   = "idevicepair"
	IdeviceBackupTool = "idevicebackup2"
)

var requiredTools = []string{IdeviceIDTool, IdevicePairTool, IdeviceBackupTool}

// outputTail is how much of a tool's combined output is kept for error
// messages when the output is being streamed rather than captured.
const outputTail = 16 * 1024

// waitDelay bounds how long a cancelled tool gets to exit after being
// interrupted before it is killed.
const waitDelay = 10 * time.Second

// DefaultPairPollInterval is how often EnsurePaired retries the pairing while
// the user is answering the trust dialog on the device.
const DefaultPairPollInterval = 2 * time.Second

// Invocation describes one run of a libimobiledevice tool.
type Invocation struct {
	// Path is the executable to run.
	Path string
	// Args are the arguments, without the program name.
	Args []string
	// Env holds extra KEY=VALUE entries appended to the inherited
	// environment. It is how secrets reach the tool: it is never logged and
	// never part of an error.
	Env []string
	// Stream, when set, receives the tool's stdout and stderr as they are
	// produced so the user sees progress and prompts (passcode, trust dialog).
	Stream io.Writer
}

// Result is what a tool left behind once it exited.
type Result struct {
	ExitCode int
	// Stdout is the complete standard output when the invocation was not
	// streamed; it is empty for streamed invocations.
	Stdout []byte
	// Output is the tail of the combined stdout and stderr, kept for error
	// messages.
	Output []byte
}

// RunFunc runs an invocation to completion. It returns an error only when the
// tool could not be started or the context ended; a tool that ran and failed
// is reported through Result.ExitCode.
type RunFunc func(context.Context, *Invocation) (*Result, error)

// Tools runs the libimobiledevice tools.
type Tools struct {
	paths map[string]string
	run   RunFunc

	// PairPollInterval overrides DefaultPairPollInterval when positive.
	PairPollInterval time.Duration
}

// MissingToolsError reports which executables could not be found in PATH.
type MissingToolsError struct {
	Missing []string
}

func (e *MissingToolsError) Error() string {
	return fmt.Sprintf("libimobiledevice tools not found in PATH: %s (install libimobiledevice, e.g. `brew install libimobiledevice` or `apt install libimobiledevice-utils`)",
		strings.Join(e.Missing, ", "))
}

// ToolError is a tool that ran and exited with a non-zero status.
type ToolError struct {
	Tool     string
	Args     []string
	ExitCode int
	Output   string
}

func (e *ToolError) Error() string {
	msg := fmt.Sprintf("%s %s: exit status %d", e.Tool, strings.Join(e.Args, " "), e.ExitCode)
	if e.Output != "" {
		msg += ": " + e.Output
	}
	return msg
}

// LookupTools resolves every required executable through PATH.
func LookupTools() (*Tools, error) {
	paths := make(map[string]string, len(requiredTools))
	var missing []string
	for _, name := range requiredTools {
		p, err := exec.LookPath(name)
		if err != nil {
			missing = append(missing, name)
			continue
		}
		paths[name] = p
	}
	if len(missing) > 0 {
		return nil, &MissingToolsError{Missing: missing}
	}
	return &Tools{paths: paths, run: execRun}, nil
}

// NewTools builds a Tools over explicit executable paths and a custom runner.
// It exists for tests; production code uses [LookupTools].
func NewTools(paths map[string]string, run RunFunc) *Tools {
	return &Tools{paths: paths, run: run}
}

// Path returns the resolved executable for a tool name.
func (t *Tools) Path(tool string) string {
	return t.paths[tool]
}

func (t *Tools) invoke(ctx context.Context, tool string, args, env []string, stream io.Writer) (*Result, error) {
	path, ok := t.paths[tool]
	if !ok {
		return nil, &MissingToolsError{Missing: []string{tool}}
	}
	res, err := t.run(ctx, &Invocation{Path: path, Args: args, Env: env, Stream: stream})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tool, err)
	}
	return res, nil
}

// check turns a non-zero exit into a ToolError.
func check(tool string, args []string, res *Result) error {
	if res.ExitCode == 0 {
		return nil
	}
	return &ToolError{
		Tool:     tool,
		Args:     args,
		ExitCode: res.ExitCode,
		Output:   lastLines(res.Output, 5),
	}
}

// lastLines returns the last n non-empty lines of out with terminal control
// sequences and carriage returns removed, joined by "; ".
func lastLines(out []byte, n int) string {
	text := strings.ReplaceAll(string(out), "\r", "\n")
	text = stripANSI(text)
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// stripANSI drops CSI escape sequences such as the cursor movements
// idevicebackup2 uses for its progress display.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// tailWriter keeps the last max bytes written to it. Writes are serialized:
// stdout and stderr land here from separate goroutines.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-w.max:]...)
	}
	return len(p), nil
}

func (w *tailWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf
}

// lockedWriter serializes writes to a writer that is not safe for concurrent
// use, such as the caller's progress stream.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// execRun is the RunFunc backed by os/exec.
func execRun(ctx context.Context, inv *Invocation) (*Result, error) {
	cmd := exec.CommandContext(ctx, inv.Path, inv.Args...)
	cmd.Env = append(os.Environ(), inv.Env...)
	// Never interactive: a tool that wants a password or a confirmation
	// must fail rather than hang on a terminal nobody is watching.
	cmd.Stdin = nil
	cmd.WaitDelay = waitDelay
	cmd.Cancel = func() error { return cancel(cmd) }

	tail := &tailWriter{max: outputTail}
	var stdout bytes.Buffer
	if inv.Stream != nil {
		// One writer for both: os/exec then shares a single pipe, which
		// keeps the tool's output in order.
		w := &lockedWriter{w: io.MultiWriter(inv.Stream, tail)}
		cmd.Stdout = w
		cmd.Stderr = w
	} else {
		cmd.Stdout = io.MultiWriter(&stdout, tail)
		cmd.Stderr = tail
	}

	err := cmd.Run()
	res := &Result{Stdout: stdout.Bytes(), Output: tail.bytes()}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
		return res, nil
	default:
		return nil, err
	}
}

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

package adb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxShellOutput bounds what a shell command may print.  The commands run
// by this package (readlink, mkdir) print a path at most.
const maxShellOutput = 1 << 20

// Shell v2 packet ids, from adb's shell_protocol.h.
const (
	shellStdin      = 0
	shellStdout     = 1
	shellStderr     = 2
	shellExit       = 3
	shellCloseStdin = 4
)

// v1ExitMarker tags the exit status appended to the output of a v1 shell
// command, which has no exit channel of its own.
const v1ExitMarker = "__plakar_exit__"

// Quote wraps s in single quotes so that the device shell takes it
// literally, whatever it contains.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellResult is the outcome of a command run on the device.
type ShellResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Shell runs a command on the device and waits for it to finish.  Every
// argument is quoted; the command is never built from device data.
//
// With the shell_v2 feature the exit status and stderr come from the
// protocol.  Older devices run the command through the legacy pty shell
// and the exit status is recovered from a marker printed at the end.
func (c *Client) Shell(ctx context.Context, serial string, features Features, args ...string) (*ShellResult, error) {
	if len(args) == 0 {
		return nil, errors.New("adb: empty shell command")
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		if strings.IndexByte(a, 0) >= 0 {
			return nil, errors.New("adb: shell argument contains a NUL byte")
		}
		quoted[i] = Quote(a)
	}
	cmd := strings.Join(quoted, " ")
	if features.Has(FeatureShellV2) {
		return c.shellV2(ctx, serial, cmd)
	}
	return c.shellV1(ctx, serial, cmd)
}

func (c *Client) shellV2(ctx context.Context, serial, cmd string) (*ShellResult, error) {
	conn, err := c.Transport(ctx, serial, "shell,v2,raw:"+cmd)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	res := &ShellResult{ExitCode: -1}
	var hdr [5]byte
	for {
		if err := conn.readFull(hdr[:]); err != nil {
			return nil, err
		}
		n := binary.LittleEndian.Uint32(hdr[1:5])
		if n > maxShellOutput {
			return nil, fmt.Errorf("%w: oversized shell packet", ErrProtocol)
		}
		payload := make([]byte, n)
		if err := conn.readFull(payload); err != nil {
			return nil, err
		}
		switch hdr[0] {
		case shellStdout:
			if len(res.Stdout)+len(payload) > maxShellOutput {
				return nil, fmt.Errorf("adb: shell output too large")
			}
			res.Stdout = append(res.Stdout, payload...)
		case shellStderr:
			if len(res.Stderr)+len(payload) > maxShellOutput {
				return nil, fmt.Errorf("adb: shell output too large")
			}
			res.Stderr = append(res.Stderr, payload...)
		case shellExit:
			if len(payload) != 1 {
				return nil, fmt.Errorf("%w: malformed shell exit packet", ErrProtocol)
			}
			res.ExitCode = int(payload[0])
			return res, nil
		case shellStdin, shellCloseStdin:
			// Never sent by the device; ignore.
		default:
			return nil, fmt.Errorf("%w: unknown shell packet id %d", ErrProtocol, hdr[0])
		}
	}
}

func (c *Client) shellV1(ctx context.Context, serial, cmd string) (*ShellResult, error) {
	full := cmd + ` 2>&1 </dev/null; echo "` + v1ExitMarker + `$?"`
	conn, err := c.Transport(ctx, serial, "shell:"+full)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	out, err := io.ReadAll(io.LimitReader(conn.rd, maxShellOutput+1))
	if err != nil {
		return nil, fmt.Errorf("adb: read: %w", err)
	}
	if len(out) > maxShellOutput {
		return nil, fmt.Errorf("adb: shell output too large")
	}
	// The legacy shell runs in a pty, which turns "\n" into "\r\n".
	out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n"))
	i := bytes.LastIndex(out, []byte(v1ExitMarker))
	if i < 0 {
		return nil, fmt.Errorf("%w: shell exit status not found", ErrProtocol)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(out[i+len(v1ExitMarker):])))
	if err != nil {
		return nil, fmt.Errorf("%w: malformed shell exit status", ErrProtocol)
	}
	return &ShellResult{Stdout: out[:i], ExitCode: code}, nil
}

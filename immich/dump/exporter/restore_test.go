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

package exporter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/stretchr/testify/require"
)

// step is one answer of the status endpoint: a status, or an error.
type step struct {
	st  *api.MaintenanceStatus
	err error
}

func active(action, task string, progress float64) step {
	return step{st: &api.MaintenanceStatus{Active: true, Action: action, Task: task, Progress: progress}}
}

var (
	inactive = step{st: &api.MaintenanceStatus{Active: false, Action: api.ActionEnd}}
	gap      = step{err: errors.New("dial tcp: connection refused")}
)

func scripted(t *testing.T, steps ...step) StatusFn {
	i := 0
	return func(context.Context) (*api.MaintenanceStatus, error) {
		require.Less(t, i, len(steps), "polled past the end of the script")
		s := steps[i]
		i++
		return s.st, s.err
	}
}

func TestWaitRestore(t *testing.T) {
	tests := []struct {
		name  string
		steps []step
		err   string
		log   []string
	}{
		{
			// The server proper still answers before it hands over to
			// the worker; the restore is over once the worker was seen
			// and the server is back.
			name: "configured server",
			steps: []step{
				inactive, inactive,
				gap, gap,
				active(api.ActionRestoreDatabase, "ready", 0),
				active(api.ActionRestoreDatabase, "restore", 0.5),
				active(api.ActionRestoreDatabase, "migrations", 0),
				active(api.ActionEnd, "", 0),
				gap,
				inactive,
			},
			log: []string{
				"immich: waiting for the server to come back",
				"immich: restore_database: ready 0%",
				"immich: restore_database: restore 50%",
				"immich: restore_database: migrations 0%",
				"immich: end",
			},
		},
		{
			// A fresh server is already in its worker.
			name: "fresh server",
			steps: []step{
				active(api.ActionRestoreDatabase, "restore", 0.9),
				gap,
				inactive,
			},
		},
		{
			// The hand-over happened between two polls.
			name: "gap missed",
			steps: []step{
				active(api.ActionRestoreDatabase, "restore", 0.9),
				inactive,
			},
		},
		{
			// Only a gap was seen: the worker came and went between
			// two polls, which is still a finished restore.
			name:  "worker missed",
			steps: []step{inactive, gap, inactive},
		},
		{
			name: "worker reports an error",
			steps: []step{
				active(api.ActionRestoreDatabase, "restore", 0.3),
				{st: &api.MaintenanceStatus{Active: true, Action: api.ActionRestoreDatabase, Task: api.TaskError, Error: "pg_restore: exit 1"}},
			},
			err: "the server reported an error: pg_restore: exit 1",
		},
		{
			name: "worker reports an error without a message",
			steps: []step{
				{st: &api.MaintenanceStatus{Active: true, Action: api.ActionRestoreDatabase, Task: api.TaskError}},
			},
			err: "the server reported an error: see the server log",
		},
		{
			name:  "proxy errors are a gap",
			steps: []step{{err: &api.Error{Method: "GET", Path: "/admin/maintenance/status", Status: http.StatusBadGateway}}, inactive},
		},
		{
			name:  "a refusal is final",
			steps: []step{{err: &api.Error{Method: "GET", Path: "/admin/maintenance/status", Status: http.StatusForbidden, Message: "no"}}},
			err:   "maintenance status: GET /admin/maintenance/status: HTTP 403: no",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer
			err := WaitRestore(context.Background(), scripted(t, tt.steps...), time.Millisecond, time.Minute, &log)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			if tt.log != nil {
				var lines []string
				for _, l := range bytes.Split(bytes.TrimSpace(log.Bytes()), []byte("\n")) {
					lines = append(lines, string(l))
				}
				require.Equal(t, tt.log, lines)
			}
		})
	}
}

func TestWaitRestoreTimesOut(t *testing.T) {
	forever := func(context.Context) (*api.MaintenanceStatus, error) {
		return &api.MaintenanceStatus{Active: true, Action: api.ActionRestoreDatabase, Task: "restore"}, nil
	}
	err := WaitRestore(context.Background(), forever, time.Millisecond, 20*time.Millisecond, io.Discard)
	require.ErrorContains(t, err, "not finished after 20ms")
}

func TestWaitRestoreStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	polls := 0
	forever := func(context.Context) (*api.MaintenanceStatus, error) {
		polls++
		if polls == 3 {
			cancel()
		}
		return &api.MaintenanceStatus{Active: false, Action: api.ActionEnd}, nil
	}
	err := WaitRestore(ctx, forever, time.Millisecond, time.Minute, io.Discard)
	require.ErrorIs(t, err, context.Canceled)
}

func TestTransient(t *testing.T) {
	require.True(t, transient(errors.New("connection refused")))
	require.True(t, transient(&api.Error{Status: http.StatusServiceUnavailable}))
	require.False(t, transient(&api.Error{Status: http.StatusUnauthorized}))
	require.False(t, transient(&api.Error{Status: http.StatusNotFound}))
}

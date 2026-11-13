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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/poolpOrg/plakar-integrations/immich/api"
)

// StatusFn reads the maintenance status.
type StatusFn func(context.Context) (*api.MaintenanceStatus, error)

// WaitRestore polls the maintenance status until the restore is over.
//
// The server proper always answers inactive, only its maintenance worker
// reports progress, and each hands the port to the other with a gap of a
// few seconds: a restore is over once the status is inactive after the
// worker was seen, or after a gap.  A worker reporting an error task ends
// the wait with that error.
func WaitRestore(ctx context.Context, status StatusFn, interval, timeout time.Duration, log io.Writer) error {
	deadline := time.Now().Add(timeout)
	seenWorker, seenGap := false, false
	last := ""

	for {
		st, err := status(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !transient(err) {
				return fmt.Errorf("maintenance status: %w", err)
			}
			if !seenGap {
				logf(log, "immich: waiting for the server to come back\n")
			}
			seenGap = true

		case st.Active:
			if st.Task == api.TaskError || st.Error != "" {
				msg := st.Error
				if msg == "" {
					msg = "see the server log"
				}
				return fmt.Errorf("the server reported an error: %s", msg)
			}
			seenWorker = true
			if s := st.String(); s != last {
				logf(log, "immich: %s\n", s)
				last = s
			}

		case seenWorker || seenGap:
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("not finished after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// logf writes a progress line; the log is stderr, whose failure is nothing
// the restore should stop for.
func logf(log io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(log, format, args...)
}

// transient tells a server that is away (connection refused or dropped, a
// proxy's 502/503/504) from an answer that will not change.
func transient(err error) bool {
	var e *api.Error
	if errors.As(err, &e) {
		switch e.Status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	return true
}

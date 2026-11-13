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
	"sync/atomic"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/poolpOrg/plakar-integrations/immich/dump"
)

// bridge runs the fs exporter on the records under /upload_location: the
// records go in with that prefix stripped, the results come back with it
// put back, so that plakar matches them with what it sent.
type bridge struct {
	in      chan *connectors.Record
	done    chan error
	fwd     chan struct{}
	written atomic.Int64
}

func newBridge(ctx context.Context, fs exporter.Exporter, meta map[string]string, results chan<- *connectors.Result, size int) *bridge {
	b := &bridge{
		in:   make(chan *connectors.Record, size),
		done: make(chan error, 1),
		fwd:  make(chan struct{}),
	}
	out := make(chan *connectors.Result, size)

	go func() {
		b.done <- fs.Export(ctx, meta, b.in, out)
	}()
	go func() {
		defer close(b.fwd)
		for res := range out {
			if res.Err == nil && !res.Record.IsXattr && res.Record.FileInfo.Lmode.IsRegular() {
				b.written.Add(1)
			}
			res.Record.Pathname = fromFS(res.Record.Pathname)
			results <- res
		}
	}()
	return b
}

func (b *bridge) send(ctx context.Context, r *connectors.Record) error {
	r.Pathname = toFS(r.Pathname)
	select {
	case b.in <- r:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finish ends the stream and waits for the fs exporter, which sets the
// directory permissions on its way out.
func (b *bridge) finish() error {
	close(b.in)
	err := <-b.done
	<-b.fwd
	return err
}

func toFS(p string) string {
	if p == dump.UploadDir {
		return "/"
	}
	return p[len(dump.UploadDir):]
}

func fromFS(p string) string {
	if p == "/" {
		return dump.UploadDir
	}
	return dump.UploadDir + p
}

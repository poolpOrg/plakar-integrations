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

package common

import (
	"context"
	"errors"
	"sync"

	"github.com/poolpOrg/plakar-integrations/android/adb"
)

// ErrPoolClosed is returned by Acquire once the pool is closed.
var ErrPoolClosed = errors.New("android: connection pool closed")

// Pool bounds the number of sync sessions open to a device and reuses
// healthy ones.  A session is one TCP connection to the adb server and
// one stream over the USB link; a handful is plenty, more just competes
// for the same cable.
//
// The pool keeps the context it was created with: the record readers
// handed to kloset are opened later, through the SDK's context-less
// open callback, and the SDK keeps Import's context alive until the
// stream is consumed.  Closing the pool when the connector closes is
// what releases the sessions.
type Pool struct {
	ctx  context.Context
	dev  *Device
	sem  chan struct{}
	mu   sync.Mutex
	idle []*adb.Sync
	done bool
}

// NewPool creates a pool of at most size sessions.  Sessions are opened
// under ctx: cancelling it closes every stream, which unblocks the readers
// that use them.
func NewPool(ctx context.Context, dev *Device, size int) *Pool {
	if size < 1 {
		size = 1
	}
	return &Pool{
		ctx: ctx,
		dev: dev,
		sem: make(chan struct{}, size),
	}
}

// Acquire returns a session, waiting for one to be released when all are
// in use.  Release must be called when done with it.
func (p *Pool) Acquire(ctx context.Context) (*adb.Sync, error) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ctx.Done():
		return nil, p.ctx.Err()
	}

	p.mu.Lock()
	if p.done {
		p.mu.Unlock()
		<-p.sem
		return nil, ErrPoolClosed
	}
	if n := len(p.idle); n > 0 {
		s := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()

	s, err := p.dev.Client.Sync(p.ctx, p.dev.Serial, p.dev.Features)
	if err != nil {
		<-p.sem
		return nil, err
	}
	return s, nil
}

// Release gives a session back.  Broken sessions are closed, not reused.
func (p *Pool) Release(s *adb.Sync) {
	p.mu.Lock()
	if s.Broken() || p.done {
		p.mu.Unlock()
		s.Close()
	} else {
		p.idle = append(p.idle, s)
		p.mu.Unlock()
	}
	<-p.sem
}

// Close closes the idle sessions; sessions still in use are closed when
// released.
func (p *Pool) Close() {
	p.mu.Lock()
	p.done = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, s := range idle {
		s.Close()
	}
}

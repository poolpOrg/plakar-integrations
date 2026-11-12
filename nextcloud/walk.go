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

package nextcloud

import (
	"context"
	"path"
	"sync"
)

// walkFn is called for every resource below the walked collection, the
// collection included, possibly from several goroutines at once.  When a
// collection cannot be listed err is set and e names that collection.
type walkFn func(e entry, err error)

// walker lists collections with a bounded number of workers.  Pending
// collections wait in a slice rather than in blocked goroutines, so a tree
// with a million directories costs strings, not stacks.
type walker struct {
	c  *client
	fn walkFn

	mu          sync.Mutex
	cond        *sync.Cond
	queue       []string
	outstanding int
}

// walk lists root and everything below it.  Only a failure to list root
// itself is returned; deeper failures go to fn.
func (c *client) walk(ctx context.Context, root string, concurrency int, fn walkFn) error {
	entries, err := c.propfind(ctx, root, 1)
	if err != nil {
		return err
	}

	w := &walker{c: c, fn: fn}
	w.cond = sync.NewCond(&w.mu)
	w.handle(root, entries, true)

	var wg sync.WaitGroup
	for range max(concurrency, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.run(ctx)
		}()
	}
	wg.Wait()
	return nil
}

// handle reports the entries of dir and queues its sub-collections.  A
// depth-1 listing includes dir itself, which the parent listing already
// reported unless dir is the root.
func (w *walker) handle(dir string, entries []entry, reportSelf bool) {
	dir = path.Clean(dir)
	for _, e := range entries {
		self := path.Clean(e.href) == dir
		if self && !reportSelf {
			continue
		}
		w.fn(e, nil)
		if e.isDir && !self {
			w.enqueue(e.href)
		}
	}
}

func (w *walker) enqueue(dir string) {
	w.mu.Lock()
	w.queue = append(w.queue, dir)
	w.outstanding++
	w.mu.Unlock()
	w.cond.Signal()
}

// next blocks until a collection is pending or none is left anywhere.
func (w *walker) next() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.queue) == 0 && w.outstanding > 0 {
		w.cond.Wait()
	}
	if len(w.queue) == 0 {
		return "", false
	}
	dir := w.queue[len(w.queue)-1]
	w.queue = w.queue[:len(w.queue)-1]
	return dir, true
}

func (w *walker) done() {
	w.mu.Lock()
	w.outstanding--
	finished := w.outstanding == 0
	w.mu.Unlock()
	if finished {
		w.cond.Broadcast()
	}
}

func (w *walker) run(ctx context.Context) {
	for {
		dir, ok := w.next()
		if !ok {
			return
		}
		// once cancelled, drain the queue without reporting: the
		// consumer of the records is gone.
		if ctx.Err() == nil {
			entries, err := w.c.propfind(ctx, dir, 1)
			if err != nil {
				w.fn(entry{href: dir, isDir: true}, err)
			} else {
				w.handle(dir, entries, false)
			}
		}
		w.done()
	}
}

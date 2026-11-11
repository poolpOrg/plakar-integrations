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

package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	fsimporter "github.com/PlakarKorp/integrations/fs/importer"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/poolpOrg/plakar-integrations/minecraft/rcon"
	"github.com/poolpOrg/plakar-integrations/minecraft/server"
)

// DefaultTimeout bounds each exchange with the console.  save-all
// flush writes every loaded chunk to disk before it answers, which
// takes a while on a large world.
const DefaultTimeout = 5 * time.Minute

// Importer backs up a server directory like the fs importer does, with
// the world saves paused for the duration: save-off and save-all flush
// over RCON before the first file is read, save-on once the last one
// has been.  A stopped server is backed up as is.
type Importer struct {
	opts    *integration.Options
	srv     *server.Server
	fs      importer.Importer
	quiesce bool
	rcon    string
	timeout time.Duration

	mu     sync.Mutex
	frozen bool // save-off was sent and save-on is owed

	stop sync.Once
	done chan struct{}
}

func init() {
	_ = importer.Register("minecraft", location.FLAG_LOCALFS|location.FLAG_NEEDACK, NewImporter)
}

// NewImporter opens the server directory named by config["location"].
func NewImporter(ctx context.Context, opts *integration.Options, name string, config map[string]string) (importer.Importer, error) {
	dir := strings.TrimPrefix(config["location"], name+"://")
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("not an absolute path %s", config["location"])
	}
	dir = filepath.Clean(dir)

	srv, err := server.Open(dir)
	if err != nil {
		return nil, err
	}

	quiesce := true
	if v, ok := config["quiesce"]; ok {
		if quiesce, err = strconv.ParseBool(v); err != nil {
			return nil, fmt.Errorf("invalid value for quiesce: %w", err)
		}
	}

	timeout := DefaultTimeout
	if v, ok := config["timeout"]; ok {
		if timeout, err = time.ParseDuration(v); err != nil || timeout <= 0 {
			return nil, fmt.Errorf("invalid value for timeout: %q", v)
		}
	}

	fsconfig := maps.Clone(config)
	fsconfig["location"] = "fs://" + dir
	fs, err := fsimporter.NewFSImporter(ctx, opts, "fs", fsconfig)
	if err != nil {
		return nil, err
	}

	return &Importer{
		opts:    opts,
		srv:     srv,
		fs:      fs,
		quiesce: quiesce,
		rcon:    config["rcon"],
		timeout: timeout,
		done:    make(chan struct{}),
	}, nil
}

func (p *Importer) Origin() string                 { return p.opts.Hostname }
func (p *Importer) Type() string                   { return "minecraft" }
func (p *Importer) Root() string                   { return p.fs.Root() }
func (p *Importer) Flags() location.Flags          { return location.FLAG_LOCALFS | location.FLAG_NEEDACK }
func (p *Importer) Ping(ctx context.Context) error { return p.fs.Ping(ctx) }

func (p *Importer) Close(ctx context.Context) error {
	p.stop.Do(func() { close(p.done) })
	return errors.Join(p.thaw(), p.fs.Close(ctx))
}

func (p *Importer) Import(ctx context.Context, records chan<- *integration.Record, results <-chan *integration.Result) (map[string]string, error) {
	if p.quiesce {
		if err := p.freeze(ctx); err != nil {
			close(records)
			return nil, err
		}
	}

	// a record is acknowledged once its content has been read, and
	// kloset closes results after the last one: that is when saving
	// may resume.  Without acknowledgments Close resumes it.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		if results != nil {
			for range results {
			}
		}
	}()

	meta, err := p.fs.Import(ctx, records, nil)
	<-drained
	if results != nil {
		err = errors.Join(err, p.thaw())
	}
	return meta, err
}

// freeze pauses the world saves of a running server and flushes what
// is loaded to disk.  It refuses to go on when the server runs but
// cannot be told to.
func (p *Importer) freeze(ctx context.Context) error {
	running, err := p.srv.Running()
	if err != nil {
		return err
	}

	addr, ok := p.srv.RconAddr(p.rcon)
	if !ok {
		if running {
			return fmt.Errorf("%s: the server is running and rcon is disabled in server.properties, enable it or back up with quiesce=false", p.srv.Dir)
		}
		return nil
	}

	conn, err := rcon.Dial(ctx, addr, p.srv.RconPassword(), p.timeout)
	if err != nil {
		if errors.Is(err, rcon.ErrAuth) || running {
			return fmt.Errorf("%s: cannot pause saving over rcon at %s: %w", p.srv.Dir, addr, err)
		}
		// nothing answers and no world is locked: the server is
		// down and its files are at rest
		return nil
	}
	defer conn.Close()

	// the replies are checked: a console that does not know the
	// command, or is not the one of this server, must not be mistaken
	// for a paused one
	reply, err := conn.Command("save-off")
	if err != nil {
		return fmt.Errorf("save-off: %w", err)
	}
	switch {
	case strings.Contains(reply, "already"):
		// saving was already off, it is not ours to turn back on
	case strings.Contains(reply, "disabled"):
		p.mu.Lock()
		p.frozen = true
		p.mu.Unlock()
	default:
		return fmt.Errorf("save-off: unexpected reply %q", reply)
	}

	reply, err = conn.Command("save-all flush")
	if err != nil {
		return errors.Join(fmt.Errorf("save-all flush: %w", err), p.thaw())
	}
	if !strings.Contains(reply, "Saved") {
		return errors.Join(fmt.Errorf("save-all flush: unexpected reply %q", reply), p.thaw())
	}

	go p.watchdog(ctx)
	return nil
}

// watchdog resumes saving when the backup is cancelled before Import
// or Close got to it.
func (p *Importer) watchdog(ctx context.Context) {
	select {
	case <-ctx.Done():
		// nobody is left to return this error to
		if err := p.thaw(); err != nil {
			fmt.Fprintln(p.stderr(), "minecraft:", err)
		}
	case <-p.done:
	}
}

// thaw turns saving back on.  It is safe to call more than once and
// from several goroutines.
func (p *Importer) thaw() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.frozen {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	addr, _ := p.srv.RconAddr(p.rcon)
	conn, err := rcon.Dial(ctx, addr, p.srv.RconPassword(), p.timeout)
	if err == nil {
		var reply string
		reply, err = conn.Command("save-on")
		conn.Close()
		if err == nil && !strings.Contains(reply, "enabled") && !strings.Contains(reply, "already") {
			err = fmt.Errorf("unexpected reply %q", reply)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: saving is still off, run save-on on the server console: %w", p.srv.Dir, err)
	}

	p.frozen = false
	return nil
}

func (p *Importer) stderr() io.Writer {
	if p.opts.Stderr != nil {
		return p.opts.Stderr
	}
	return os.Stderr
}

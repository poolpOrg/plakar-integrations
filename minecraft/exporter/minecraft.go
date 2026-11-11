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
	"io/fs"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	fsexporter "github.com/PlakarKorp/integrations/fs/exporter"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/poolpOrg/plakar-integrations/minecraft/rcon"
	"github.com/poolpOrg/plakar-integrations/minecraft/server"
)

// DefaultTimeout bounds each exchange with the console, which only has
// to answer the login here.
const DefaultTimeout = 30 * time.Second

// Exporter restores into a server directory like the fs exporter does,
// after making sure no server runs there: a world written under a
// running server is a corrupt world.
type Exporter struct {
	fs exporter.Exporter
}

func init() {
	_ = exporter.Register("minecraft", location.FLAG_LOCALFS, NewExporter)
}

// NewExporter prepares the server directory named by config["location"]
// for a restore, creating it when it does not exist.
func NewExporter(ctx context.Context, opts *integration.Options, name string, config map[string]string) (exporter.Exporter, error) {
	dir := strings.TrimPrefix(config["location"], name+"://")
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("not an absolute path %s", config["location"])
	}
	dir = filepath.Clean(dir)

	allowRunning := false
	if v, ok := config["allow_running"]; ok {
		var err error
		if allowRunning, err = strconv.ParseBool(v); err != nil {
			return nil, fmt.Errorf("invalid value for allow_running: %w", err)
		}
	}

	timeout := DefaultTimeout
	if v, ok := config["timeout"]; ok {
		var err error
		if timeout, err = time.ParseDuration(v); err != nil || timeout <= 0 {
			return nil, fmt.Errorf("invalid value for timeout: %q", v)
		}
	}

	srv, err := server.Open(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// nothing there yet, the fs exporter creates it
	case err != nil:
		return nil, err
	case !allowRunning:
		running, err := isRunning(ctx, srv, config["rcon"], timeout)
		if err != nil {
			return nil, err
		}
		if running {
			return nil, fmt.Errorf("%s: the server is running, stop it before restoring or restore with allow_running=true", dir)
		}
	}

	fsconfig := maps.Clone(config)
	fsconfig["location"] = "fs://" + dir
	fs, err := fsexporter.NewFSExporter(ctx, opts, "fs", fsconfig)
	if err != nil {
		return nil, err
	}

	return &Exporter{fs: fs}, nil
}

// isRunning tells from the world locks first, then from whether
// something answers on the console.
func isRunning(ctx context.Context, srv *server.Server, override string, timeout time.Duration) (bool, error) {
	running, err := srv.Running()
	if err != nil || running {
		return running, err
	}

	addr, ok := srv.RconAddr(override)
	if !ok {
		return false, nil
	}

	conn, err := rcon.Dial(ctx, addr, srv.RconPassword(), timeout)
	if err == nil {
		conn.Close()
		return true, nil
	}
	// a refused password is still an answer
	return errors.Is(err, rcon.ErrAuth), nil
}

func (p *Exporter) Origin() string        { return p.fs.Origin() }
func (p *Exporter) Type() string          { return "minecraft" }
func (p *Exporter) Root() string          { return p.fs.Root() }
func (p *Exporter) Flags() location.Flags { return location.FLAG_LOCALFS }

func (p *Exporter) Ping(ctx context.Context) error  { return p.fs.Ping(ctx) }
func (p *Exporter) Close(ctx context.Context) error { return p.fs.Close(ctx) }

func (p *Exporter) Export(ctx context.Context, meta map[string]string, records <-chan *integration.Record, results chan<- *integration.Result) error {
	return p.fs.Export(ctx, meta, records, results)
}

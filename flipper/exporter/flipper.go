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
	"os"
	"path"
	"strings"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/poolpOrg/plakar-integrations/flipper/device"
)

type Exporter struct {
	opts *integration.Options
	loc  device.Location
	conn device.Conn

	// directories known to exist on the device, mkdir is a round-trip
	dirs map[string]bool
}

func init() {
	exporter.Register("flipper", 0, NewExporter)
}

func NewExporter(ctx context.Context, opts *integration.Options, name string, config map[string]string) (exporter.Exporter, error) {
	loc, err := device.ParseLocation(name, config["location"])
	if err != nil {
		return nil, err
	}

	conn, err := device.Open(ctx, loc.Name, config["port"])
	if err != nil {
		return nil, err
	}

	return New(opts, loc, conn), nil
}

func New(opts *integration.Options, loc device.Location, conn device.Conn) *Exporter {
	return &Exporter{
		opts: opts,
		loc:  loc,
		conn: conn,
		dirs: map[string]bool{"/": true},
	}
}

func (p *Exporter) Origin() string {
	if name := p.conn.Name(); name != "" {
		return name
	}
	return "flipper"
}

func (p *Exporter) Type() string          { return "flipper" }
func (p *Exporter) Root() string          { return p.loc.Path }
func (p *Exporter) Flags() location.Flags { return 0 }

func (p *Exporter) Ping(ctx context.Context) error {
	_, err := p.conn.Stat(path.Dir(p.loc.Path))
	return err
}

func (p *Exporter) Close(ctx context.Context) error {
	return p.conn.Close()
}

func (p *Exporter) target(pathname string) (string, error) {
	if path.Clean("/"+pathname) != pathname {
		return "", fmt.Errorf("record path is not clean %q", pathname)
	}
	return path.Join(p.loc.Path, pathname), nil
}

func (p *Exporter) mkdirAll(dir string) error {
	if p.dirs[dir] {
		return nil
	}
	if parent := path.Dir(dir); parent != dir {
		if err := p.mkdirAll(parent); err != nil {
			return err
		}
	}
	if err := p.conn.Mkdir(dir); err != nil && !errors.Is(err, device.ErrExist) {
		return err
	}
	p.dirs[dir] = true
	return nil
}

func (p *Exporter) Export(ctx context.Context, records <-chan *integration.Record, results chan<- *integration.Result) error {
	defer close(results)

	if err := p.mkdirAll(p.loc.Path); err != nil {
		return fmt.Errorf("failed to create restore root %s: %w", p.loc.Path, err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case record, ok := <-records:
			if !ok {
				if a, ok := p.conn.(interface{ Alert() error }); ok {
					_ = a.Alert()
				}
				return nil
			}

			if record.Err != nil || record.IsXattr {
				results <- record.Ok()
				continue
			}

			if err := p.export(record); err != nil {
				results <- record.Error(err)
			} else {
				results <- record.Ok()
			}
		}
	}
}

func (p *Exporter) export(record *integration.Record) error {
	target, err := p.target(record.Pathname)
	if err != nil {
		return err
	}

	mode := record.FileInfo.Mode()
	switch {
	case mode.IsDir():
		return p.mkdirAll(target)

	case mode&os.ModeSymlink != 0:
		return fmt.Errorf("%s: symlinks are not supported on the device", record.Pathname)

	case mode.IsRegular():
		if err := p.mkdirAll(path.Dir(target)); err != nil {
			return err
		}
		data, err := io.ReadAll(record.Reader)
		if err != nil {
			return err
		}
		return p.conn.Write(target, data)
	}

	// sockets, fifos, devices: nothing the device could hold
	return nil
}

// String is handy in error messages and logs.
func (p *Exporter) String() string {
	return strings.TrimSuffix(p.loc.String(), "/")
}

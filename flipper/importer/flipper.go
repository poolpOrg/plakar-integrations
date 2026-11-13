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
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/PlakarKorp/kloset/exclude"
	"github.com/poolpOrg/plakar-integrations/flipper/device"
)

// The device reports neither modification times nor permissions.  With
// md5 enabled (the default) every file gets a stable epoch mtime and its
// on-device MD5 split across the dev and ino fields, so that the backup
// cache skips files whose content did not change.  Without it, files
// get the time of the backup and are always re-read.
var epoch = time.Unix(0, 0).UTC()

type Importer struct {
	opts     *integration.Options
	loc      device.Location
	conn     device.Conn
	excludes *exclude.RuleSet
	md5      bool
	started  time.Time
}

func init() {
	importer.Register("flipper", 0, NewImporter)
}

func NewImporter(ctx context.Context, opts *integration.Options, name string, config map[string]string) (importer.Importer, error) {
	loc, err := device.ParseLocation(name, config["location"])
	if err != nil {
		return nil, err
	}

	md5 := true
	if v, ok := config["md5"]; ok {
		md5, err = strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid value for md5: %w", err)
		}
	}

	excludes := exclude.NewRuleSet()
	if err := excludes.AddRulesFromArray(opts.Excludes); err != nil {
		return nil, fmt.Errorf("failed to setup exclude rules: %w", err)
	}

	conn, err := device.Open(ctx, loc.Name, config["port"])
	if err != nil {
		return nil, err
	}

	return New(opts, loc, conn, excludes, md5), nil
}

func New(opts *integration.Options, loc device.Location, conn device.Conn, excludes *exclude.RuleSet, md5 bool) *Importer {
	if excludes == nil {
		excludes = exclude.NewRuleSet()
	}
	return &Importer{
		opts:     opts,
		loc:      loc,
		conn:     conn,
		excludes: excludes,
		md5:      md5,
	}
}

func (p *Importer) Origin() string {
	if name := p.conn.Name(); name != "" {
		return name
	}
	return "flipper"
}

func (p *Importer) Type() string          { return "flipper" }
func (p *Importer) Root() string          { return p.loc.Path }
func (p *Importer) Flags() location.Flags { return 0 }

func (p *Importer) Ping(ctx context.Context) error {
	_, err := p.conn.Stat(p.loc.Path)
	return err
}

func (p *Importer) Close(ctx context.Context) error {
	return p.conn.Close()
}

func (p *Importer) Import(ctx context.Context, records chan<- *integration.Record, _ <-chan *integration.Result) error {
	defer close(records)
	p.started = time.Now()

	for _, dir := range ancestors(p.loc.Path) {
		records <- integration.NewRecord(dir, "", dirInfo(dir), nil, nil)
	}

	st, err := p.conn.Stat(p.loc.Path)
	if err != nil {
		return fmt.Errorf("%s: %w", p.loc.Path, err)
	}

	if !st.IsDir {
		p.file(records, p.loc.Path, st)
		return nil
	}

	records <- integration.NewRecord(p.loc.Path, "", dirInfo(p.loc.Path), nil, nil)
	if err := p.walk(ctx, records, p.loc.Path); err != nil {
		return err
	}

	if a, ok := p.conn.(interface{ Alert() error }); ok {
		_ = a.Alert()
	}
	return nil
}

func (p *Importer) walk(ctx context.Context, records chan<- *integration.Record, dir string) error {
	entries, err := p.conn.List(dir)
	if err != nil {
		records <- integration.NewError(dir, err)
		return nil
	}

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}

		full := path.Join(dir, e.Name)
		if p.excludes.IsExcluded(full, e.IsDir) {
			continue
		}

		if e.IsDir {
			records <- integration.NewRecord(full, "", dirInfo(full), nil, nil)
			if err := p.walk(ctx, records, full); err != nil {
				return err
			}
			continue
		}

		p.file(records, full, e)
	}
	return nil
}

func (p *Importer) file(records chan<- *integration.Record, full string, e *device.File) {
	info := integration.NewFileInfo(e.Name, e.Size, 0644, p.started, 0, 0, 0, 0, 1)

	if p.md5 {
		// recent firmwares hand the md5 out with the listing, older
		// ones cost one round-trip per file
		sum := e.Md5
		if sum == "" {
			var err error
			if sum, err = p.conn.Md5(full); err != nil {
				records <- integration.NewError(full, err)
				return
			}
		}
		if dev, ino, err := splitMd5(sum); err == nil {
			info.LmodTime = epoch
			info.Ldev = dev
			info.Lino = ino
		}
	}

	records <- integration.NewRecord(full, "", info, nil, func() (io.ReadCloser, error) {
		data, err := p.conn.Read(full)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	})
}

func dirInfo(p string) integration.FileInfo {
	return integration.NewFileInfo(path.Base(p), 0, os.ModeDir|0755, epoch, 0, 0, 0, 0, 1)
}

// ancestors lists the directories above p, top-down, so that the
// snapshot has a complete tree down to the root being backed up.
func ancestors(p string) []string {
	var dirs []string
	for p != "/" {
		p = path.Dir(p)
		dirs = append([]string{p}, dirs...)
	}
	return dirs
}

func splitMd5(sum string) (dev, ino uint64, err error) {
	raw, err := hex.DecodeString(sum)
	if err != nil {
		return 0, 0, err
	}
	if len(raw) != 16 {
		return 0, 0, fmt.Errorf("unexpected md5 length %d", len(raw))
	}
	return binary.BigEndian.Uint64(raw[8:]), binary.BigEndian.Uint64(raw[:8]), nil
}

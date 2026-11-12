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
	"archive/tar"
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
	"github.com/poolpOrg/plakar-integrations/homeassistant/archive"
	"github.com/poolpOrg/plakar-integrations/homeassistant/hass"
)

// maxMetadata bounds backup.json coming out of a snapshot.
const maxMetadata = 16 << 20

// Config are the exporter options.
type Config struct {
	Agent    string // backup agent to upload to, the local one when empty
	Password string // password of protected members, for the restore
	Restore  bool   // ask the instance to restore the backup once uploaded
}

type Exporter struct {
	opts   *integration.Options
	loc    hass.Location
	client *hass.Client
	cfg    Config

	builder *archive.Builder
	failed  int // records that could not be staged
}

func init() {
	exporter.Register("homeassistant", 0, NewExporter)
}

func NewExporter(ctx context.Context, opts *integration.Options, name string, config map[string]string) (exporter.Exporter, error) {
	tls, err := hass.ParseBool(config, "tls", false)
	if err != nil {
		return nil, err
	}
	loc, err := hass.ParseLocation(name, config["location"], tls)
	if err != nil {
		return nil, err
	}
	token, err := hass.Token(config)
	if err != nil {
		return nil, err
	}

	cfg := Config{Agent: config["agent"], Password: config["password"]}
	if cfg.Restore, err = hass.ParseBool(config, "restore", false); err != nil {
		return nil, err
	}

	return New(opts, loc, hass.NewClient(loc, token), cfg)
}

func New(opts *integration.Options, loc hass.Location, client *hass.Client, cfg Config) (*Exporter, error) {
	b, err := archive.NewBuilder()
	if err != nil {
		return nil, err
	}
	return &Exporter{opts: opts, loc: loc, client: client, cfg: cfg, builder: b}, nil
}

func (p *Exporter) Origin() string                 { return p.loc.Host }
func (p *Exporter) Type() string                   { return "homeassistant" }
func (p *Exporter) Root() string                   { return "/" }
func (p *Exporter) Flags() location.Flags          { return 0 }
func (p *Exporter) Ping(ctx context.Context) error { return p.client.Ping(ctx) }

func (p *Exporter) Close(ctx context.Context) error {
	if p.builder == nil {
		return nil
	}
	err := p.builder.Close()
	p.builder = nil
	return err
}

// Export stages the records into a backup archive, uploads it once
// they are all in and, when asked, has the instance restore it.  A
// record that cannot be staged fails the whole export: the instance
// must never be handed a backup with pieces missing.
func (p *Exporter) Export(ctx context.Context, _ map[string]string, records <-chan *integration.Record, results chan<- *integration.Result) error {
	defer close(results)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case record, ok := <-records:
			if !ok {
				if p.failed > 0 {
					return fmt.Errorf("%d records could not be staged, not uploading an incomplete backup", p.failed)
				}
				return p.finish(ctx)
			}
			if record.IsXattr {
				results <- record.Ok()
				continue
			}
			if record.Err != nil {
				p.failed++
				results <- record.Ok()
				continue
			}
			if err := p.stage(record); err != nil {
				p.failed++
				results <- record.Error(err)
			} else {
				results <- record.Ok()
			}
		}
	}
}

// stage files one record into the builder.  The root holds backup.json
// and the opaque members, every directory below it is an inner tar.
func (p *Exporter) stage(record *integration.Record) error {
	pathname := record.Pathname
	if path.Clean("/"+pathname) != pathname {
		return fmt.Errorf("record path is not clean %q", pathname)
	}
	if pathname == "/" {
		return nil
	}

	rel := pathname[1:]
	mode := record.FileInfo.Mode()
	stem, rest, nested := strings.Cut(rel, "/")

	if !nested {
		switch {
		case rel == archive.MetadataName:
			if record.FileInfo.Size() > maxMetadata {
				return fmt.Errorf("%s is too large (%d bytes)", rel, record.FileInfo.Size())
			}
			raw, err := io.ReadAll(io.LimitReader(record.Reader, maxMetadata))
			if err != nil {
				return err
			}
			return p.builder.SetMetadata(raw)
		case mode.IsDir():
			return nil
		case mode.IsRegular():
			return p.builder.AddBlob(rel, record.Reader)
		}
		return fmt.Errorf("%s: not a regular file", pathname)
	}

	hdr := &tar.Header{
		Name:    rest,
		Mode:    int64(mode.Perm()),
		ModTime: record.FileInfo.ModTime(),
		Uid:     int(record.FileInfo.Uid()),
		Gid:     int(record.FileInfo.Gid()),
	}
	var body io.Reader
	switch {
	case mode.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name += "/"
	case mode&os.ModeSymlink != 0:
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = record.Target
	case mode.IsRegular():
		hdr.Typeflag = tar.TypeReg
		hdr.Size = record.FileInfo.Size()
		body = record.Reader
	default:
		// sockets, fifos, devices: nothing a backup could hold
		return nil
	}
	return p.builder.AddEntry(stem, hdr, body)
}

// finish uploads the archive and optionally restores it.
func (p *Exporter) finish(ctx context.Context) error {
	md := p.builder.Metadata()
	if md == nil {
		return archive.ErrNoMetadata
	}

	sess, err := p.client.Connect(ctx)
	if err != nil {
		return err
	}
	defer sess.Close()

	agent := p.cfg.Agent
	if agent == "" {
		if agent, err = sess.LocalAgent(ctx); err != nil {
			return err
		}
	}

	pr, pw := io.Pipe()
	written := make(chan error, 1)
	go func() {
		_, err := p.builder.WriteTo(pw)
		pw.CloseWithError(err)
		written <- err
	}()

	filename := "backup.tar"
	if md.Slug != "" {
		filename = md.Slug + ".tar"
	}
	backupID, err := p.client.Upload(ctx, agent, filename, pr)
	pr.Close()
	if werr := <-written; err == nil && werr != nil {
		err = werr
	}
	if err != nil {
		return err
	}

	if !p.cfg.Restore {
		return nil
	}

	opts := hass.RestoreOptions{BackupID: backupID, AgentID: agent, Password: p.cfg.Password}
	supervised, err := sess.Supervised(ctx)
	if err != nil {
		return err
	}
	if supervised {
		opts.Addons = md.AddonSlugs()
		opts.Folders = md.Folders
	}
	err = sess.Restore(ctx, opts)
	if errors.Is(err, hass.ErrRestoreUnconfirmed) {
		if p.opts != nil && p.opts.Stderr != nil {
			fmt.Fprintf(p.opts.Stderr, "homeassistant: the instance went away while restoring %s, check it once it is back\n", backupID)
		}
		return nil
	}
	return err
}

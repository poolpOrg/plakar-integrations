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
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/poolpOrg/plakar-integrations/homeassistant/archive"
	"github.com/poolpOrg/plakar-integrations/homeassistant/hass"
)

// defaultFolders are the shared folders a supervised backup includes.
var defaultFolders = []string{"media", "share", "ssl", "addons/local"}

// Config are the importer options.
type Config struct {
	Agent    string // backup agent, the local one when empty
	Backup   string // import this existing backup instead of making one
	Name     string // name of the backup being made
	Password string // encryption password, for protected backups
	Database bool   // include the recorder database
	Addons   bool   // include add-ons, supervised installs only
	Folders  []string
	Delete   bool // remove the backup from the instance once imported
}

type Importer struct {
	opts   *integration.Options
	loc    hass.Location
	client *hass.Client
	cfg    Config

	arch *archive.Archive
}

func init() {
	importer.Register("homeassistant", 0, NewImporter)
}

func NewImporter(ctx context.Context, opts *integration.Options, name string, config map[string]string) (importer.Importer, error) {
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

	cfg := Config{
		Agent:    config["agent"],
		Backup:   config["backup"],
		Name:     config["name"],
		Password: config["password"],
	}
	if cfg.Database, err = hass.ParseBool(config, "database", true); err != nil {
		return nil, err
	}
	if cfg.Addons, err = hass.ParseBool(config, "addons", true); err != nil {
		return nil, err
	}
	if cfg.Delete, err = hass.ParseBool(config, "delete", false); err != nil {
		return nil, err
	}
	if v, ok := config["folders"]; ok {
		for f := range strings.SplitSeq(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				cfg.Folders = append(cfg.Folders, f)
			}
		}
	} else {
		cfg.Folders = defaultFolders
	}
	if cfg.Backup != "" && cfg.Delete {
		return nil, fmt.Errorf("delete only applies to backups the importer makes, not to backup=%s", cfg.Backup)
	}

	return New(opts, loc, hass.NewClient(loc, token), cfg), nil
}

func New(opts *integration.Options, loc hass.Location, client *hass.Client, cfg Config) *Importer {
	return &Importer{opts: opts, loc: loc, client: client, cfg: cfg}
}

func (p *Importer) Origin() string                 { return p.loc.Host }
func (p *Importer) Type() string                   { return "homeassistant" }
func (p *Importer) Root() string                   { return "/" }
func (p *Importer) Flags() location.Flags          { return 0 }
func (p *Importer) Ping(ctx context.Context) error { return p.client.Ping(ctx) }

func (p *Importer) Close(ctx context.Context) error {
	if p.arch == nil {
		return nil
	}
	err := p.arch.Close()
	p.arch = nil
	return err
}

// Import makes a backup on the instance (or picks an existing one),
// downloads it and hands its content over file by file.
func (p *Importer) Import(ctx context.Context, records chan<- *integration.Record, _ <-chan *integration.Result) (map[string]string, error) {
	defer close(records)

	sess, err := p.client.Connect(ctx)
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	agent := p.cfg.Agent
	if agent == "" {
		if agent, err = sess.LocalAgent(ctx); err != nil {
			return nil, err
		}
	}

	backup, generated, err := p.pick(ctx, sess, agent)
	if err != nil {
		return nil, err
	}

	password := ""
	if backup.ProtectedOn(agent) {
		if p.cfg.Password == "" {
			return nil, fmt.Errorf("backup %s is encrypted, set the password option to read it", backup.ID)
		}
		password = p.cfg.Password
	}

	spool, err := p.download(ctx, backup.ID, agent, password)
	if err != nil {
		return nil, err
	}
	arch, err := archive.Open(spool)
	if err != nil {
		os.Remove(spool)
		return nil, fmt.Errorf("backup %s: %w", backup.ID, err)
	}
	p.arch = arch

	when := time.Now()
	if t, err := time.Parse(time.RFC3339, backup.Date); err == nil {
		when = t
	}
	if err := emit(ctx, records, arch, when); err != nil {
		return nil, err
	}

	if generated && p.cfg.Delete {
		if err := sess.Delete(ctx, backup.ID); err != nil {
			return nil, fmt.Errorf("the backup was imported but could not be removed from the instance: %w", err)
		}
	}

	return map[string]string{
		"backup": backup.ID,
		"name":   backup.Name,
		"date":   backup.Date,
		"agent":  agent,
	}, nil
}

// pick returns the backup to import, making one unless the
// configuration names an existing one.
func (p *Importer) pick(ctx context.Context, sess *hass.Session, agent string) (*hass.Backup, bool, error) {
	if p.cfg.Backup != "" {
		b, err := sess.Get(ctx, p.cfg.Backup)
		if err != nil {
			return nil, false, err
		}
		if b == nil {
			return nil, false, fmt.Errorf("the instance has no backup %s", p.cfg.Backup)
		}
		if _, held := b.Agents[agent]; len(b.Agents) > 0 && !held {
			return nil, false, fmt.Errorf("backup %s is not held by agent %s", p.cfg.Backup, agent)
		}
		return b, false, nil
	}

	if err := sess.Subscribe(ctx); err != nil {
		return nil, false, err
	}

	name := p.cfg.Name
	if name == "" {
		name = "plakar " + time.Now().UTC().Format(time.RFC3339)
	}
	gen := hass.GenerateOptions{
		AgentID:         agent,
		Name:            name,
		Password:        p.cfg.Password,
		IncludeDatabase: p.cfg.Database,
	}
	supervised, err := sess.Supervised(ctx)
	if err != nil {
		return nil, false, err
	}
	if supervised {
		gen.IncludeAddons = p.cfg.Addons
		gen.IncludeFolders = p.cfg.Folders
	}

	job, err := sess.Generate(ctx, gen)
	if err != nil {
		return nil, false, err
	}
	if err := sess.WaitCreated(ctx); err != nil {
		return nil, false, err
	}
	b, err := sess.Find(ctx, job, name)
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// download spools the backup to a temporary file, the archive is read
// at random and the instance only streams it.
func (p *Importer) download(ctx context.Context, backupID, agent, password string) (string, error) {
	body, _, err := p.client.Download(ctx, backupID, agent, password)
	if err != nil {
		return "", err
	}
	defer body.Close()

	f, err := os.CreateTemp("", "plakar-homeassistant-*.tar")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("download %s: %w", backupID, err)
	}
	return f.Name(), nil
}

// emit sends the archive as a file tree: backup.json and the blobs at
// the root, one directory per exploded inner tar.
func emit(ctx context.Context, records chan<- *integration.Record, arch *archive.Archive, when time.Time) error {
	seen := map[string]bool{}

	send := func(r *integration.Record) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case records <- r:
			return nil
		}
	}
	dir := func(pathname string, mode os.FileMode, mtime time.Time, uid, gid uint64) error {
		if seen[pathname] {
			return nil
		}
		seen[pathname] = true
		info := integration.NewFileInfo(path.Base(pathname), 0, os.ModeDir|mode, mtime, 0, 0, uid, gid, 1)
		return send(integration.NewRecord(pathname, "", info, nil, nil))
	}
	parents := func(pathname string) error {
		var missing []string
		for d := path.Dir(pathname); !seen[d]; d = path.Dir(d) {
			missing = append(missing, d)
		}
		for i := len(missing) - 1; i >= 0; i-- {
			if err := dir(missing[i], 0755, when, 0, 0); err != nil {
				return err
			}
		}
		return nil
	}

	if err := dir("/", 0755, when, 0, 0); err != nil {
		return err
	}

	meta := arch.Metadata
	info := integration.NewFileInfo(archive.MetadataName, int64(len(meta)), 0644, when, 0, 0, 0, 0, 1)
	if err := send(integration.NewRecord("/"+archive.MetadataName, "", info, nil, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(meta)), nil
	})); err != nil {
		return err
	}

	for _, m := range arch.Members {
		if m.Blob != nil {
			if err := send(fileRecord("/"+m.Name, m.Blob, when)); err != nil {
				return err
			}
			continue
		}

		root := "/" + m.Stem
		if err := dir(root, 0755, when, 0, 0); err != nil {
			return err
		}
		for _, e := range m.Entries {
			pathname := path.Join(root, e.Name)
			if err := parents(pathname); err != nil {
				return err
			}
			hdr := e.Header
			switch hdr.Typeflag {
			case tar.TypeDir:
				if err := dir(pathname, hdr.FileInfo().Mode().Perm(), hdr.ModTime, uint64(hdr.Uid), uint64(hdr.Gid)); err != nil {
					return err
				}
			case tar.TypeSymlink:
				if seen[pathname] {
					continue
				}
				seen[pathname] = true
				info := integration.NewFileInfo(path.Base(pathname), 0, os.ModeSymlink|0777, hdr.ModTime, 0, 0, uint64(hdr.Uid), uint64(hdr.Gid), 1)
				if err := send(integration.NewRecord(pathname, hdr.Linkname, info, nil, nil)); err != nil {
					return err
				}
			default:
				if seen[pathname] {
					continue
				}
				seen[pathname] = true
				if err := send(fileRecord(pathname, e, hdr.ModTime)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func fileRecord(pathname string, e *archive.Entry, mtime time.Time) *integration.Record {
	hdr := e.Header
	mode := hdr.FileInfo().Mode().Perm()
	if mode == 0 {
		mode = 0644
	}
	info := integration.NewFileInfo(path.Base(pathname), hdr.Size, mode, mtime, 0, 0, uint64(hdr.Uid), uint64(hdr.Gid), 1)
	return integration.NewRecord(pathname, "", info, nil, e.Open)
}

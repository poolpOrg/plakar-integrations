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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/poolpOrg/plakar-integrations/kindle/kindle"
)

type KindleImporter struct {
	opts *integration.Options

	volume    *kindle.Volume
	selection kindle.Selection
	origin    string
}

func init() {
	importer.Register("kindle", 0, NewKindleImporter)
}

func NewKindleImporter(ctx context.Context, opts *integration.Options, name string, config map[string]string) (importer.Importer, error) {
	volume, err := kindle.Open(config["location"])
	if err != nil {
		return nil, err
	}

	if err := volume.Validate(); err != nil {
		return nil, err
	}

	selection, err := parseSelection(config)
	if err != nil {
		return nil, err
	}

	// the serial identifies the device across remounts, the mount point
	// is whatever plugged it in last decided.
	origin := volume.Serial()
	if origin == "" {
		origin = opts.Hostname + ":" + volume.Root
	}

	return &KindleImporter{
		opts:      opts,
		volume:    volume,
		selection: selection,
		origin:    origin,
	}, nil
}

func parseSelection(config map[string]string) (kindle.Selection, error) {
	sel := kindle.DefaultSelection()

	for key, field := range map[string]*bool{
		"documents":   &sel.Documents,
		"annotations": &sel.Annotations,
		"system":      &sel.System,
		"everything":  &sel.Everything,
	} {
		value, ok := config[key]
		if !ok {
			continue
		}
		parsed, err := parseBool(value)
		if err != nil {
			return sel, fmt.Errorf("invalid %s value %q: want true or false", key, value)
		}
		*field = parsed
	}

	if !sel.Everything && !sel.Documents && !sel.Annotations && !sel.System {
		return sel, fmt.Errorf("nothing selected to back up: " +
			"enable at least one of documents, annotations or system")
	}

	return sel, nil
}

func (p *KindleImporter) Root() string          { return "/" }
func (p *KindleImporter) Origin() string        { return p.origin }
func (p *KindleImporter) Type() string          { return "kindle" }
func (p *KindleImporter) Flags() location.Flags { return 0 }

// Ping re-checks the device is still there: a Kindle can be unplugged between
// configuration and backup, and failing here beats failing mid-walk.
func (p *KindleImporter) Ping(ctx context.Context) error {
	return p.volume.Validate()
}

func (p *KindleImporter) Import(ctx context.Context, records chan<- *integration.Record, results <-chan *integration.Result) (map[string]string, error) {
	defer close(records)

	root := p.volume.Root

	err := filepath.WalkDir(root, func(abspath string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		rel, relErr := filepath.Rel(root, abspath)
		if relErr != nil {
			rel = abspath
		}

		if err != nil {
			// an unreadable book shouldn't cost the rest of the library.
			records <- integration.NewError(toPathname(rel), err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if p.selection.Skip(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			records <- integration.NewError(toPathname(rel), err)
			return nil
		}

		pathname := toPathname(rel)

		switch {
		case d.IsDir():
			records <- integration.NewRecord(pathname, "",
				fileInfo(fi, pathname), nil, nil)

		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(abspath)
			if err != nil {
				records <- integration.NewError(pathname, err)
				return nil
			}
			records <- integration.NewRecord(pathname, target,
				fileInfo(fi, pathname), nil, nil)

		case fi.Mode().IsRegular():
			records <- integration.NewRecord(pathname, "",
				fileInfo(fi, pathname), nil, func() (io.ReadCloser, error) {
					return os.Open(abspath)
				})

		default:
			// sockets and devices have no business being on a Kindle.
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return p.metadata(), nil
}

// metadata names the device the snapshot was taken from.
func (p *KindleImporter) metadata() map[string]string {
	if serial := p.volume.Serial(); serial != "" {
		return map[string]string{"serial": serial}
	}
	return nil
}

// toPathname turns a path relative to the volume root into the pathname the
// snapshot is addressed by.
func toPathname(rel string) string {
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "/"
	}
	return "/" + rel
}

func fileInfo(fi fs.FileInfo, pathname string) integration.FileInfo {
	name := path.Base(pathname)
	if pathname == "/" {
		name = "/"
	}

	out := integration.FileInfo{
		Lname:    name,
		Lsize:    fi.Size(),
		Lmode:    fi.Mode(),
		LmodTime: fi.ModTime(),
		Ldev:     1,
		Lnlink:   1,
	}

	// FAT carries no ownership or permissions, and what the OS synthesises
	// depends on the mount options.  normalise so snapshots of one device
	// stay comparable across machines.
	if fi.IsDir() {
		out.Lmode = fi.Mode().Type() | 0o755
	} else if fi.Mode().IsRegular() {
		out.Lmode = 0o644
	}

	return out
}

func (p *KindleImporter) Close(ctx context.Context) error {
	return nil
}

func parseBool(s string) (bool, error) {
	switch s {
	case "true", "yes", "on", "1":
		return true, nil
	case "false", "no", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean")
}

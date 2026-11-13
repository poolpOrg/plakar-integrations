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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/poolpOrg/plakar-integrations/kindle/kindle"
)

type KindleExporter struct {
	opts *integration.Options

	volume *kindle.Volume

	// off by default: those files describe one device, and writing another
	// Kindle's settings over them is a good way to confuse the firmware.
	system bool
}

func init() {
	exporter.Register("kindle", 0, NewKindleExporter)
}

func NewKindleExporter(ctx context.Context, opts *integration.Options, name string, config map[string]string) (exporter.Exporter, error) {
	volume, err := kindle.Open(config["location"])
	if err != nil {
		return nil, err
	}

	// otherwise a restore scatters books across the user's home directory.
	if err := volume.Validate(); err != nil {
		return nil, err
	}

	system := false
	if value, ok := config["system"]; ok {
		system, err = parseBool(value)
		if err != nil {
			return nil, fmt.Errorf("invalid system value %q: want true or false", value)
		}
	}

	return &KindleExporter{
		opts:   opts,
		volume: volume,
		system: system,
	}, nil
}

func (p *KindleExporter) Root() string          { return "/" }
func (p *KindleExporter) Type() string          { return "kindle" }
func (p *KindleExporter) Flags() location.Flags { return 0 }

func (p *KindleExporter) Origin() string {
	if serial := p.volume.Serial(); serial != "" {
		return serial
	}
	return p.volume.Root
}

func (p *KindleExporter) Ping(ctx context.Context) error {
	return p.volume.Validate()
}

func (p *KindleExporter) Export(ctx context.Context, _ map[string]string, records <-chan *integration.Record, results chan<- *integration.Result) error {
	defer close(results)

	// sequential on purpose: it is a slow bus, and parallel writes to FAT
	// on flash fragment badly for no gain.
	for record := range records {
		if record.Err != nil || record.IsXattr {
			results <- record.Ok()
			continue
		}

		results <- record.Error(p.store(ctx, record))
	}

	return nil
}

func (p *KindleExporter) store(ctx context.Context, record *integration.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	abspath, err := p.resolve(record.Pathname)
	if err != nil {
		return err
	}

	mode := record.FileInfo.Lmode
	switch {
	case mode.IsDir():
		return os.MkdirAll(abspath, 0o755)

	case mode.IsRegular():
		if err := os.MkdirAll(filepath.Dir(abspath), 0o755); err != nil {
			return err
		}
		return writeFile(abspath, record.Reader, record.FileInfo.Lsize)

	default:
		// FAT cannot represent symlinks, devices or sockets, and that is
		// not worth failing a restore over.
		return nil
	}
}

// resolve maps a snapshot pathname onto the device, refusing anything that
// would write outside the volume or into a protected area.
func (p *KindleExporter) resolve(pathname string) (string, error) {
	clean := filepath.Clean("/" + filepath.FromSlash(pathname))
	rel := strings.TrimPrefix(filepath.ToSlash(clean), "/")

	if rel == "" || rel == "." {
		return p.volume.Root, nil
	}

	// Clean collapses ".." that tried to escape, but a leading one survives.
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("refusing to restore outside the device: %s", pathname)
	}

	if !p.system {
		top, _, _ := strings.Cut(rel, "/")
		if strings.EqualFold(top, kindle.SystemDir) {
			return "", fmt.Errorf("refusing to restore %s without system=true: "+
				"these files describe a specific device", pathname)
		}
	}

	return filepath.Join(p.volume.Root, filepath.FromSlash(rel)), nil
}

// writeFile writes through a temporary file so an interrupted restore leaves
// the previous book intact, not a truncated one the Kindle lists but cannot
// open.
func writeFile(abspath string, rd io.Reader, size int64) error {
	dir := filepath.Dir(abspath)

	tmp, err := os.CreateTemp(dir, ".plakar-*")
	if err != nil {
		return err
	}
	tmpname := tmp.Name()

	defer func() {
		tmp.Close()
		os.Remove(tmpname) // no-op once the rename succeeded
	}()

	written, err := io.Copy(tmp, rd)
	if err != nil {
		return err
	}
	if size >= 0 && written != size {
		return fmt.Errorf("short write: expected %d bytes, wrote %d", size, written)
	}

	// a Kindle gets yanked out the moment the transfer looks done.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpname, abspath)
}

func (p *KindleExporter) Close(ctx context.Context) error {
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

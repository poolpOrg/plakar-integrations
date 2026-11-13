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

// Package exporter restores an immich:// snapshot into an Immich user.
// Records are staged under a private temporary directory, because the
// relations can only be pushed once every asset has an id on the target;
// the push happens when the stream ends.
package exporter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/poolpOrg/plakar-integrations/immich/importer"
	"golang.org/x/sync/errgroup"
)

type Exporter struct {
	opts   *connectors.Options
	client *api.Client

	tempDir string
	root    *os.Root
}

func NewExporter(ctx context.Context, opts *connectors.Options, name string, config map[string]string) (exporter.Exporter, error) {
	cfg, err := importer.ClientConfig(config)
	if err != nil {
		return nil, err
	}
	client, err := api.New(cfg)
	if err != nil {
		return nil, err
	}

	tempDir, err := os.MkdirTemp("", "plakar-immich-restore-*")
	if err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	root, err := os.OpenRoot(tempDir)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("open staging directory: %w", err)
	}

	return &Exporter{opts: opts, client: client, tempDir: tempDir, root: root}, nil
}

func (e *Exporter) Origin() string        { return e.client.Host() }
func (e *Exporter) Type() string          { return "immich" }
func (e *Exporter) Root() string          { return "/" }
func (e *Exporter) Flags() location.Flags { return 0 }

func (e *Exporter) Ping(ctx context.Context) error {
	if _, err := e.client.Me(ctx); err != nil {
		return fmt.Errorf("immich: %w", err)
	}
	return nil
}

func (e *Exporter) Close(context.Context) error {
	err := e.root.Close()
	if rmErr := os.RemoveAll(e.tempDir); rmErr != nil && err == nil {
		err = rmErr
	}
	return err
}

// relative turns a record pathname into a path under the staging root.
// path.Clean absorbs "..", filepath.Localize rejects what is left that is
// not a plain relative path, and *os.Root keeps symlinks from escaping.
func relative(pathname string) (string, error) {
	p := strings.TrimPrefix(path.Clean("/"+pathname), "/")
	if p == "" {
		return ".", nil
	}
	return filepath.Localize(p)
}

func (e *Exporter) Export(ctx context.Context, _ map[string]string, records <-chan *connectors.Record, results chan<- *connectors.Result) (ret error) {
	defer close(results)

	concurrency := 1
	if e.opts != nil && e.opts.MaxConcurrency > 1 {
		concurrency = e.opts.MaxConcurrency
	}
	var g errgroup.Group
	g.SetLimit(concurrency)

loop:
	for {
		select {
		case <-ctx.Done():
			ret = ctx.Err()
			break loop

		case record, ok := <-records:
			if !ok {
				break loop
			}

			if record.Err != nil || record.IsXattr {
				results <- record.Ok()
				continue
			}

			pathname, err := relative(record.Pathname)
			if err != nil {
				results <- record.Error(err)
				continue
			}

			if record.FileInfo.Lmode.IsDir() {
				if err := e.root.Mkdir(pathname, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
					results <- record.Error(err)
				} else {
					results <- record.Ok()
				}
				continue
			}

			if !record.FileInfo.Lmode.IsRegular() {
				// Nothing in the layout is a symlink or a device.
				results <- record.Ok()
				continue
			}

			g.Go(func() error {
				if err := e.stage(pathname, record.Reader); err != nil {
					results <- record.Error(err)
				} else {
					results <- record.Ok()
				}
				return nil
			})
		}
	}

	_ = g.Wait()
	if ret != nil {
		return ret
	}

	if err := e.push(ctx); err != nil {
		return fmt.Errorf("immich: %w", err)
	}
	return nil
}

func (e *Exporter) stage(dest string, src io.Reader) error {
	if err := e.root.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}
	f, err := e.root.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}

	if _, err := io.Copy(f, src); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", dest, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	return nil
}

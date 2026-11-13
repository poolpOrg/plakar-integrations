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

// Package importer backs up one Immich user through the API: the state is
// folded in memory from the API-key endpoints, then emitted as the layout
// package describes, originals and sidecars streamed lazily.
package importer

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/poolpOrg/plakar-integrations/immich/api"
)

// EnvAPIKey is the environment variable the key is read from, ahead of the
// api_key config entry, so it never shows up on a command line.
const EnvAPIKey = "PLAKAR_IMMICH_API_KEY"

type Importer struct {
	opts   *connectors.Options
	client *api.Client
}

// ClientConfig builds the api.Config shared by the importer and the
// exporter from the connector config and the environment.
func ClientConfig(config map[string]string) (api.Config, error) {
	cfg := api.Config{Location: config["location"]}

	cfg.APIKey = os.Getenv(EnvAPIKey)
	if cfg.APIKey == "" {
		cfg.APIKey = config["api_key"]
	}

	if v, ok := config["insecure"]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid value for insecure: %w", err)
		}
		cfg.Insecure = b
	}
	if v, ok := config["plaintext"]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid value for plaintext: %w", err)
		}
		cfg.Plaintext = &b
	}
	return cfg, nil
}

func NewImporter(ctx context.Context, opts *connectors.Options, name string, config map[string]string) (importer.Importer, error) {
	cfg, err := ClientConfig(config)
	if err != nil {
		return nil, err
	}
	client, err := api.New(cfg)
	if err != nil {
		return nil, err
	}
	return &Importer{opts: opts, client: client}, nil
}

func (i *Importer) Origin() string              { return i.client.Host() }
func (i *Importer) Type() string                { return "immich" }
func (i *Importer) Root() string                { return "/" }
func (i *Importer) Flags() location.Flags       { return location.FLAG_STREAM }
func (i *Importer) Close(context.Context) error { return nil }

func (i *Importer) Ping(ctx context.Context) error {
	if _, err := i.client.Me(ctx); err != nil {
		return fmt.Errorf("immich: %w", err)
	}
	return nil
}

func (i *Importer) Import(ctx context.Context, records chan<- *connectors.Record, _ <-chan *connectors.Result) (map[string]string, error) {
	defer close(records)

	concurrency := 1
	if i.opts != nil && i.opts.MaxConcurrency > 1 {
		concurrency = i.opts.MaxConcurrency
	}

	state, err := Fold(ctx, i.client, concurrency)
	if err != nil {
		return nil, fmt.Errorf("immich: %w", err)
	}

	if err := Emit(ctx, i.client, state, records); err != nil {
		return nil, fmt.Errorf("immich: %w", err)
	}
	return map[string]string{}, nil
}

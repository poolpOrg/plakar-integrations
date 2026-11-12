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

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/poolpOrg/plakar-integrations/n8n/common"
)

func init() {
	importer.Register("n8n", 0, NewImporter)
}

// Importer backs up an n8n instance through its public REST API, laid out as
// a virtual filesystem of JSON documents.
type Importer struct {
	opts   *integration.Options
	cfg    *common.Config
	client *common.Client

	// executions captures every execution with its run data. Off by default:
	// execution logs are large and short-lived.
	executions bool
}

// NewImporter builds an Importer from the connector configuration.
func NewImporter(appCtx context.Context, opts *integration.Options, name string, config map[string]string) (importer.Importer, error) {
	cfg, err := common.ParseConfig(config)
	if err != nil {
		return nil, err
	}
	executions, err := common.ParseBool(config, "executions", false)
	if err != nil {
		return nil, err
	}

	return &Importer{
		opts:       opts,
		cfg:        cfg,
		client:     common.NewClient(cfg),
		executions: executions,
	}, nil
}

func (imp *Importer) Type() string {
	return "n8n"
}

func (imp *Importer) Origin() string {
	return imp.cfg.Origin()
}

func (imp *Importer) Root() string {
	return "/"
}

func (imp *Importer) Flags() location.Flags {
	return 0
}

func (imp *Importer) Ping(ctx context.Context) error {
	if err := imp.client.Ping(ctx); err != nil {
		return fmt.Errorf("n8n %s: %w", imp.cfg.Host, err)
	}
	return nil
}

func (imp *Importer) Import(ctx context.Context, records chan<- *integration.Record, results <-chan *integration.Result) error {
	defer close(records)
	return imp.walk(ctx, records)
}

func (imp *Importer) Close(ctx context.Context) error {
	return nil
}

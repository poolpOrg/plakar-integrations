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

// Package common holds the n8n-specific plumbing shared by the importer and
// the exporter: parsing the n8n:// location and options, and a small client
// over the n8n public REST API (/api/v1) using the standard library only.
package common

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultTimeout bounds each individual HTTP request.
const DefaultTimeout = 60 * time.Second

// Config is the parsed, normalized form of a connector configuration map.
type Config struct {
	// Host is the host[:port] of the n8n instance, taken from the location.
	// It identifies the instance in snapshot metadata and never carries the
	// API key.
	Host string
	// BaseURL is the public API root, e.g. https://n8n.example.com/api/v1.
	BaseURL string
	// APIKey authenticates every request (X-N8N-API-KEY header).
	APIKey string
	// Timeout bounds each individual HTTP request.
	Timeout time.Duration
}

// ParseConfig turns a connector config map into a Config.
//
// The location is n8n://host[:port][/prefix]. The instance is reached over
// https unless the "scheme" option says http; the optional path prefix is
// kept for instances served under a sub-path (n8n://example.com/n8n).
//
// The API key is read, in order of precedence, from:
//   - the "api_key" option
//   - the location userinfo (n8n://<key>@host)
//   - the N8N_API_KEY environment variable
func ParseConfig(config map[string]string) (*Config, error) {
	target := config["location"]
	if target == "" {
		return nil, fmt.Errorf("missing location")
	}

	parsed, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("invalid location %q: %w", target, err)
	}
	if parsed.Scheme != "n8n" {
		return nil, fmt.Errorf("location scheme must be n8n://, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("location %q has no host", target)
	}

	apiKey := config["api_key"]
	if parsed.User != nil {
		if u := parsed.User.Username(); u != "" {
			if apiKey != "" {
				return nil, fmt.Errorf("can not use <key>@host syntax and api_key parameter")
			}
			apiKey = u
		}
	}
	if apiKey == "" {
		apiKey = os.Getenv("N8N_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("missing n8n API key (set the api_key option, put it in the location userinfo, or export N8N_API_KEY)")
	}

	scheme := config["scheme"]
	switch scheme {
	case "":
		scheme = "https"
	case "http", "https":
	default:
		return nil, fmt.Errorf("invalid scheme %q: must be http or https", scheme)
	}

	timeout := DefaultTimeout
	if v := config["timeout"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid timeout %q: %w", v, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("invalid timeout %q: must be positive", v)
		}
		timeout = d
	}

	prefix := strings.TrimRight(parsed.Path, "/")

	return &Config{
		Host:    parsed.Host,
		BaseURL: scheme + "://" + parsed.Host + prefix + "/api/v1",
		APIKey:  apiKey,
		Timeout: timeout,
	}, nil
}

// Origin returns a stable identifier for the instance being backed up. It
// never contains the API key.
func (c *Config) Origin() string {
	return c.Host
}

// ParseBool reads an optional boolean option, defaulting to def when unset.
func ParseBool(config map[string]string, key string, def bool) (bool, error) {
	v, ok := config[key]
	if !ok || v == "" {
		return def, nil
	}
	switch strings.ToLower(v) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid value for %s: %q", key, v)
}

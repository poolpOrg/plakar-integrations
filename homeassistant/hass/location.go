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

// Package hass talks to the backup API of a Home Assistant instance:
// the websocket commands under backup/ and the HTTP download and
// upload endpoints.  It needs Home Assistant 2025.1 or later, where
// the backup manager is part of core and fronts the supervisor on
// Home Assistant OS and supervised installs.
package hass

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// DefaultPort is the port Home Assistant listens on out of the box.
const DefaultPort = "8123"

// Location is a parsed homeassistant://host[:port] string.
type Location struct {
	Host string // host:port, the port defaults to 8123
	TLS  bool
}

// ParseLocation parses the location handed over by plakar.  The scheme
// is the connector name, followed by the host and an optional port.
// Whether to use TLS is a separate option because the scheme is taken.
func ParseLocation(proto, location string, tls bool) (Location, error) {
	prefix := proto + "://"
	if !strings.HasPrefix(location, prefix) {
		return Location{}, fmt.Errorf("location %q does not start with %s", location, prefix)
	}

	u, err := url.Parse("http://" + strings.TrimPrefix(location, prefix))
	if err != nil {
		return Location{}, fmt.Errorf("invalid location %q: %w", location, err)
	}
	if u.Host == "" {
		return Location{}, fmt.Errorf("location %q has no host", location)
	}
	if u.Path != "" && u.Path != "/" {
		return Location{}, fmt.Errorf("location %q: a path is not supported", location)
	}
	if u.User != nil {
		return Location{}, fmt.Errorf("location %q: credentials belong in the token option, not the location", location)
	}

	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(u.Hostname(), DefaultPort)
	}

	return Location{Host: host, TLS: tls}, nil
}

// String renders the location as plakar received it.
func (l Location) String() string {
	return "homeassistant://" + l.Host
}

func (l Location) scheme(ws bool) string {
	switch {
	case ws && l.TLS:
		return "wss"
	case ws:
		return "ws"
	case l.TLS:
		return "https"
	}
	return "http"
}

// URL builds an absolute URL for a path on the instance.
func (l Location) URL(path string) string {
	return l.scheme(false) + "://" + l.Host + path
}

// WebsocketURL is the websocket API endpoint of the instance.
func (l Location) WebsocketURL() string {
	return l.scheme(true) + "://" + l.Host + "/api/websocket"
}

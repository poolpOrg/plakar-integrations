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

package device

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Location is a parsed flipper:// URL.  The host part, when present,
// names the device (the serial number printed by the USB descriptor,
// with or without its "flip_" prefix) so that several Flippers can be
// told apart.  The path is the on-device path, "/ext" being the SD
// card and "/int" the internal flash.
type Location struct {
	Name string
	Path string
}

const DefaultPath = "/ext"

func ParseLocation(proto, location string) (Location, error) {
	if !strings.HasPrefix(location, proto+"://") {
		return Location{}, fmt.Errorf("not a %s location: %s", proto, location)
	}

	u, err := url.Parse(location)
	if err != nil {
		return Location{}, err
	}

	loc := Location{Name: u.Host, Path: u.Path}

	// flipper://ext/nfc is a natural thing to type, treat the known
	// storage roots as the start of the path rather than a device name.
	switch strings.ToLower(u.Host) {
	case "ext", "int", "any":
		loc.Name = ""
		loc.Path = "/" + u.Host + u.Path
	}

	if loc.Path == "" {
		loc.Path = DefaultPath
	}
	loc.Path = path.Clean(loc.Path)
	if !strings.HasPrefix(loc.Path, "/") {
		return Location{}, fmt.Errorf("not an absolute device path: %s", loc.Path)
	}

	return loc, nil
}

func (l Location) String() string {
	return "flipper://" + l.Name + l.Path
}

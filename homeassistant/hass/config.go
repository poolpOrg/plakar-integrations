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

package hass

import (
	"fmt"
	"os"
	"strconv"
)

// TokenEnv is where the long-lived access token is read from when the
// configuration does not carry one.
const TokenEnv = "PLAKAR_HOMEASSISTANT_TOKEN"

// Token returns the access token from the configuration or the
// environment.
func Token(config map[string]string) (string, error) {
	if t := config["token"]; t != "" {
		return t, nil
	}
	if t := os.Getenv(TokenEnv); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("no access token: set the token option or %s", TokenEnv)
}

// ParseBool reads a boolean option, def when it is absent.
func ParseBool(config map[string]string, key string, def bool) (bool, error) {
	v, ok := config[key]
	if !ok {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid value for %s: %w", key, err)
	}
	return b, nil
}

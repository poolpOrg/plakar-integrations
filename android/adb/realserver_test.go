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

package adb_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/poolpOrg/plakar-integrations/android/adb"
	"github.com/stretchr/testify/require"
)

// TestRealServer talks to a real adb server (PLAKAR_ANDROID_TEST_SERVER
// set to its host:port, or "1" for the default address).  No device is
// needed: it checks that the host protocol framing matches the real
// implementation.
func TestRealServer(t *testing.T) {
	addr := os.Getenv("PLAKAR_ANDROID_TEST_SERVER")
	if addr == "" {
		t.Skip("set PLAKAR_ANDROID_TEST_SERVER to test against a running adb server")
	}
	if addr == "1" {
		addr = adb.DefaultServerAddr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := adb.NewClient(addr)
	version, err := client.Version(ctx)
	require.NoError(t, err)
	require.Len(t, version, 4)
	t.Logf("adb server version %s", version)

	devices, err := client.Devices(ctx)
	require.NoError(t, err)
	for _, d := range devices {
		t.Logf("device %s state %q model %q transport %s", d.Serial, d.State, d.Model, d.TransportID)
	}

	_, err = client.Features(ctx, "no-such-device-serial")
	var aerr *adb.Error
	require.ErrorAs(t, err, &aerr)
	t.Logf("unknown serial: %s", aerr.Message)
}

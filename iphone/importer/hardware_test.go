/*
 * Copyright (c) 2026 Gilles Chehade <gilles@plakar.io>
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
	"os"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/poolpOrg/plakar-integrations/iphone/common"
	"github.com/stretchr/testify/require"
)

// TestHardwareBackup backs up a real device. It runs only with
// PLAKAR_IPHONE_HARDWARE=1 and needs libimobiledevice installed and exactly
// one trusted device connected over USB (or PLAKAR_IPHONE_UDID set).
//
// The backup is staged in PLAKAR_IPHONE_WORKDIR when set, so a second run is
// incremental; otherwise in a temporary directory, which means a full backup.
func TestHardwareBackup(t *testing.T) {
	if os.Getenv("PLAKAR_IPHONE_HARDWARE") != "1" {
		t.Skip("set PLAKAR_IPHONE_HARDWARE=1 with a device connected to run")
	}

	tools, err := common.LookupTools()
	require.NoError(t, err)

	workdir := os.Getenv("PLAKAR_IPHONE_WORKDIR")
	if workdir == "" {
		workdir = t.TempDir()
	}
	config := map[string]string{
		"location":                    "iphone://" + os.Getenv("PLAKAR_IPHONE_UDID"),
		common.ConfigWorkingDirectory: workdir,
	}

	imp, err := New(context.Background(), &integration.Options{Hostname: "test", MaxConcurrency: 1, Stderr: os.Stderr}, "iphone", config, tools)
	require.NoError(t, err)
	defer imp.Close(context.Background())
	t.Logf("device %s, staging in %s", imp.UDID(), imp.Workdir())

	require.NoError(t, imp.Ping(context.Background()))

	records, err := drain(t, context.Background(), imp)
	require.NoError(t, err)

	info, err := common.ValidateBackup(imp.Workdir(), imp.UDID())
	require.NoError(t, err)
	t.Logf("%s (%s, iOS %s): %d records, encrypted=%t, full=%t", info.DeviceName, info.ProductType, info.ProductVersion, len(records), info.Encrypted, info.IsFullBackup)

	for _, name := range []string{common.InfoPlist, common.ManifestPlist, common.StatusPlist, common.ManifestDB} {
		require.Contains(t, records, "/"+imp.UDID()+"/"+name)
	}
	var errored int
	for _, rec := range records {
		if rec.Err != nil {
			errored++
			t.Logf("record error: %s: %v", rec.Pathname, rec.Err)
		}
	}
	require.Zero(t, errored)
}

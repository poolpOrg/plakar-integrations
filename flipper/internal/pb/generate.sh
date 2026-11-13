#!/bin/sh
#
# Copyright (c) 2026 Gilles Chehade <gilles@poolp.org>
#
# Permission to use, copy, modify, and distribute this software for any
# purpose with or without fee is hereby granted, provided that the above
# copyright notice and this permission notice appear in all copies.
#
# THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
# WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
# MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
# ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
# WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
# ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
# OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
#
# Regenerates the RPC bindings, run through go generate.
set -e
cd "$(dirname "$0")"
M=github.com/poolpOrg/plakar-integrations/flipper/internal/pb
protoc -I proto --go_out=../.. \
	--go_opt=module=github.com/poolpOrg/plakar-integrations/flipper \
	--go_opt=Mflipperzero/flipper.proto=$M/flipper \
	--go_opt=Mflipperzero/storage.proto=$M/storage \
	--go_opt=Mflipperzero/system.proto=$M/system \
	--go_opt=Mflipperzero/application.proto=$M/app \
	--go_opt=Mflipperzero/gui.proto=$M/gui \
	--go_opt=Mflipperzero/gpio.proto=$M/gpio \
	--go_opt=Mflipperzero/property.proto=$M/property \
	--go_opt=Mflipperzero/desktop.proto=$M/desktop \
	proto/flipperzero/*.proto

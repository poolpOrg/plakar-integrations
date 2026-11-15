/*
 * Copyright (c) 2026 Plakar
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
	"fmt"
	"io"
	"os/exec"
	"runtime"
)

// openBrowser hands a URL to the user's default browser. The plugin runs as
// a child of the plakar command on the user's own machine, so this is the
// most direct way to put the consent and picker pages in front of them:
// plakar's terminal UI shows a single, width-clamped line of plugin output,
// which is not enough to copy a long URL from.
var openBrowser = func(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// prompt shows the user a URL to act on: it is opened in the browser when
// possible and always printed, on one line, so that a renderer that only
// keeps the last line of plugin output still shows it.
func prompt(out io.Writer, what, url string) {
	if err := openBrowser(url); err == nil {
		fmt.Fprintf(out, "google photos: %s (opened in your browser): %s\n", what, url)
		return
	}
	fmt.Fprintf(out, "google photos: %s, open this URL in a browser: %s\n", what, url)
}

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

package nextcloud

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const userAgent = "plakar-nextcloud/1.0"

// client speaks the subset of WebDAV and OCS the connector needs.  The
// generic WebDAV clients we already use elsewhere cannot ask for the
// owncloud file id that the versions endpoint is keyed by, hence the
// hand-rolled PROPFIND.
type client struct {
	http     *http.Client
	base     *url.URL // scheme://host[:port]/<prefix>
	username string
	password string
}

// statusError is a non-2xx answer from the server.
type statusError struct {
	Method string
	Path   string
	Code   int
	Status string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.Method, e.Path, e.Status)
}

// entry is one resource as described by a PROPFIND answer.  href is the
// decoded path on the server, relative to the Nextcloud install prefix.
type entry struct {
	href    string
	isDir   bool
	size    int64
	modTime time.Time
	fileID  string
}

// url builds the request URL for the server path p.  Every character of p
// is part of the path, a "?" in a file name included.
func (c *client) url(p string) string {
	u := *c.base
	u.Path = path.Join(c.base.Path, p)
	if strings.HasSuffix(p, "/") && !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u.String()
}

func (c *client) do(ctx context.Context, method, p string, body io.Reader, size int64, hdr http.Header) (*http.Response, error) {
	return c.request(ctx, method, c.url(p), p, body, size, hdr)
}

func (c *client) request(ctx context.Context, method, url, p string, body io.Reader, size int64, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		// an explicit length avoids a chunked upload, which not every
		// reverse proxy in front of Nextcloud accepts.
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("User-Agent", userAgent)
	for k, v := range hdr {
		req.Header[k] = v
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return nil, &statusError{Method: method, Path: p, Code: resp.StatusCode, Status: resp.Status}
	}
	return resp, nil
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
 <d:prop>
  <d:resourcetype/>
  <d:getcontentlength/>
  <d:getlastmodified/>
  <oc:fileid/>
 </d:prop>
</d:propfind>`

type multistatus struct {
	Responses []response `xml:"DAV: response"`
}

type response struct {
	Href     string     `xml:"DAV: href"`
	Propstat []propstat `xml:"DAV: propstat"`
}

type propstat struct {
	Status string `xml:"DAV: status"`
	Prop   prop   `xml:"DAV: prop"`
}

type prop struct {
	ResourceType struct {
		Collection *struct{} `xml:"DAV: collection"`
	} `xml:"DAV: resourcetype"`
	ContentLength string `xml:"DAV: getcontentlength"`
	LastModified  string `xml:"DAV: getlastmodified"`
	FileID        string `xml:"http://owncloud.org/ns fileid"`
}

func (c *client) entry(r response) (entry, error) {
	u, err := url.Parse(r.Href)
	if err != nil {
		return entry{}, fmt.Errorf("href %q: %w", r.Href, err)
	}
	href := u.Path
	if c.base.Path != "/" {
		href = strings.TrimPrefix(href, c.base.Path)
	}
	e := entry{href: href}

	for _, ps := range r.Propstat {
		if !strings.Contains(ps.Status, " 200 ") {
			continue
		}
		p := ps.Prop
		e.isDir = p.ResourceType.Collection != nil
		e.fileID = p.FileID
		if p.ContentLength != "" {
			if e.size, err = strconv.ParseInt(p.ContentLength, 10, 64); err != nil {
				return entry{}, fmt.Errorf("%s: getcontentlength %q: %w", href, p.ContentLength, err)
			}
		}
		if p.LastModified != "" {
			if e.modTime, err = http.ParseTime(p.LastModified); err != nil {
				return entry{}, fmt.Errorf("%s: getlastmodified %q: %w", href, p.LastModified, err)
			}
		}
	}
	return e, nil
}

// maxPropfindAnswer bounds a directory listing.  Nextcloud spends about
// half a kilobyte per entry, so this is room for half a million entries in
// one directory; past it the answer is cut and the listing fails.
const maxPropfindAnswer = 256 << 20

// propfind lists p (depth 0) or p and its children (depth 1).  With depth 1
// the collection itself comes back as well.
func (c *client) propfind(ctx context.Context, p string, depth int) ([]entry, error) {
	hdr := http.Header{
		"Depth":        {strconv.Itoa(depth)},
		"Content-Type": {"application/xml; charset=utf-8"},
	}
	resp, err := c.do(ctx, "PROPFIND", p, strings.NewReader(propfindBody), int64(len(propfindBody)), hdr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var ms multistatus
	if err := xml.NewDecoder(io.LimitReader(resp.Body, maxPropfindAnswer)).Decode(&ms); err != nil {
		return nil, fmt.Errorf("PROPFIND %s: parse answer: %w", p, err)
	}

	entries := make([]entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		e, err := c.entry(r)
		if err != nil {
			return nil, fmt.Errorf("PROPFIND %s: %w", p, err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (c *client) get(ctx context.Context, p string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, p, nil, 0, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// put uploads size bytes from body to p.  Nextcloud takes the file's
// modification time from X-OC-Mtime; without it the upload time is used.
func (c *client) put(ctx context.Context, p string, body io.Reader, size int64, mtime time.Time) error {
	hdr := http.Header{}
	if !mtime.IsZero() {
		hdr.Set("X-Oc-Mtime", strconv.FormatInt(mtime.Unix(), 10))
	}
	resp, err := c.do(ctx, http.MethodPut, p, body, size, hdr)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// mkcol creates the collection p; an existing one is not an error.
func (c *client) mkcol(ctx context.Context, p string) error {
	resp, err := c.do(ctx, "MKCOL", p, nil, 0, nil)
	var se *statusError
	if errors.As(err, &se) && se.Code == http.StatusMethodNotAllowed {
		return nil
	}
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// userID asks OCS for the id of the authenticated user, which is what the
// DAV paths are built from.  It differs from the login name when the user
// signs in with an email address.
func (c *client) userID(ctx context.Context) (string, error) {
	hdr := http.Header{}
	hdr.Set("Ocs-Apirequest", "true")
	const p = "/ocs/v2.php/cloud/user"
	resp, err := c.request(ctx, http.MethodGet, c.url(p)+"?format=json", p, nil, 0, hdr)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var answer struct {
		OCS struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		} `json:"ocs"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer); err != nil {
		return "", fmt.Errorf("parse OCS user answer: %w", err)
	}
	if answer.OCS.Data.ID == "" {
		return "", errors.New("OCS user answer carries no user id")
	}
	return answer.OCS.Data.ID, nil
}

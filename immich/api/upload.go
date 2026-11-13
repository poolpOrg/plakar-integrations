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

package api

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
)

// Upload is one POST /assets: the original, its sidecar when there is one,
// and the fields the server takes at upload time.
type Upload struct {
	// Filename is sent as the original file name.
	Filename string

	// Open yields the original's bytes.
	Open func() (io.ReadCloser, error)

	// OpenSidecar yields the XMP sidecar; nil when there is none.
	OpenSidecar func() (io.ReadCloser, error)

	// ChecksumHex is the SHA1 of the original, sent as x-immich-checksum so
	// the server can answer "duplicate" before reading the body.
	ChecksumHex string

	DeviceAssetID  string
	DeviceID       string
	FileCreatedAt  string
	FileModifiedAt string
	IsFavorite     bool

	// Visibility is archive, timeline or locked; leave empty for the
	// default (and for hidden, which the server sets on live-photo videos).
	Visibility string

	// Duration is the video duration string, sent when not empty.
	Duration string

	// LivePhotoVideoID is the id of the already uploaded video of a live
	// photo.
	LivePhotoVideoID string
}

// UploadAsset streams one asset to POST /assets.  The multipart body is
// produced by a goroutine that this call owns and waits for.
func (c *Client) UploadAsset(ctx context.Context, up Upload) (*UploadResult, error) {
	if up.Open == nil || up.Filename == "" {
		return nil, fmt.Errorf("upload %q: nothing to upload", up.Filename)
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(writeMultipart(mw, up))
	}()

	// Whatever happens below, the writer must finish: closing the read end
	// unblocks it with ErrClosedPipe.
	defer func() {
		_ = pr.Close()
		<-done
	}()

	hdr := http.Header{}
	if up.ChecksumHex != "" {
		hdr.Set("X-Immich-Checksum", up.ChecksumHex)
	}

	resp, err := c.do(ctx, http.MethodPost, "/assets", nil, pr, mw.FormDataContentType(), hdr)
	if err != nil {
		return nil, fmt.Errorf("upload %q: %w", up.Filename, err)
	}
	defer closeQuietly(resp.Body)

	var out UploadResult
	if err := decode(resp, http.MethodPost, "/assets", &out); err != nil {
		return nil, fmt.Errorf("upload %q: %w", up.Filename, err)
	}
	if out.ID == "" {
		return nil, fmt.Errorf("upload %q: server returned no asset id", up.Filename)
	}
	return &out, nil
}

func writeMultipart(mw *multipart.Writer, up Upload) error {
	fields := [][2]string{
		{"deviceAssetId", up.DeviceAssetID},
		{"deviceId", up.DeviceID},
		{"fileCreatedAt", up.FileCreatedAt},
		{"fileModifiedAt", up.FileModifiedAt},
		{"isFavorite", boolString(up.IsFavorite)},
	}
	if up.Visibility != "" {
		fields = append(fields, [2]string{"visibility", up.Visibility})
	}
	if up.Duration != "" {
		fields = append(fields, [2]string{"duration", up.Duration})
	}
	if up.LivePhotoVideoID != "" {
		fields = append(fields, [2]string{"livePhotoVideoId", up.LivePhotoVideoID})
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return fmt.Errorf("write field %s: %w", f[0], err)
		}
	}

	if err := copyPart(mw, "assetData", up.Filename, up.Open); err != nil {
		return err
	}
	if up.OpenSidecar != nil {
		if err := copyPart(mw, "sidecarData", up.Filename+".xmp", up.OpenSidecar); err != nil {
			return err
		}
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("close multipart: %w", err)
	}
	return nil
}

func copyPart(mw *multipart.Writer, field, filename string, open func() (io.ReadCloser, error)) error {
	rc, err := open()
	if err != nil {
		return fmt.Errorf("open %s: %w", field, err)
	}
	defer closeQuietly(rc)

	w, err := mw.CreateFormFile(field, filename)
	if err != nil {
		return fmt.Errorf("create part %s: %w", field, err)
	}
	if _, err := io.Copy(w, rc); err != nil {
		return fmt.Errorf("copy %s: %w", field, err)
	}
	return nil
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

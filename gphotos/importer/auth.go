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
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// PickerScope is the only scope the Picker API needs: create sessions and
// read what the user picked in them.
const PickerScope = "https://www.googleapis.com/auth/photospicker.mediaitems.readonly"

// tokenSource builds the OAuth2 token source for the Picker API from the
// connector configuration. ctx must outlive the importer: the source uses
// it for every token refresh.
//
// Google Photos only supports user OAuth2 (service accounts are refused), so
// the caller supplies an OAuth2 client (credentials_file/credentials_json,
// the "Desktop app" JSON downloaded from the Google Cloud console) and a
// token holding at least a refresh_token. When token_file is given but does
// not exist yet, the consent flow is run once in the terminal and the token
// is saved there for later runs. The same happens when the saved refresh
// token is later revoked or expired.
//
// Supported configuration keys (see plugin/importer/gphotos.json):
//
//   - credentials_file / credentials_json: the OAuth2 client JSON
//   - token_file / token_json: a JSON oauth2.Token with a refresh_token
//   - refresh_token: a bare refresh token, used with the client credentials
//   - access_token: a bare access token, never refreshed
func tokenSource(ctx context.Context, params map[string]string, out io.Writer) (oauth2.TokenSource, error) {
	clientJSON, err := readInlineOrFile(params, "credentials_json", "credentials_file")
	if err != nil {
		return nil, err
	}

	tokenJSON, err := readInlineOrFile(params, "token_json", "token_file")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	tokenMissing := errors.Is(err, os.ErrNotExist) // token_file set but not there yet

	// Shortcut: a bare access token, no client credentials, no refresh.
	if len(clientJSON) == 0 && len(tokenJSON) == 0 {
		if at := params["access_token"]; at != "" {
			return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: at}), nil
		}
		return nil, errors.New("google photos: no credentials: set credentials_file (OAuth2 client JSON) and token_file")
	}

	if len(clientJSON) == 0 {
		return nil, errors.New("google photos: a token was given but no OAuth2 client credentials (credentials_file/credentials_json) to refresh it with")
	}

	cfg, err := google.ConfigFromJSON(clientJSON, PickerScope)
	if err != nil {
		return nil, fmt.Errorf("google photos: invalid OAuth2 client credentials: %w", err)
	}

	tokenFile := params["token_file"]
	tok := &oauth2.Token{}
	switch {
	case len(tokenJSON) != 0:
		if err := json.Unmarshal(tokenJSON, tok); err != nil {
			return nil, fmt.Errorf("google photos: invalid token JSON: %w", err)
		}
	case params["refresh_token"] != "":
		tok.RefreshToken = params["refresh_token"]
	case params["access_token"] != "":
		tok.AccessToken = params["access_token"]
	case tokenMissing:
		tok, err = consentAndSave(ctx, cfg, tokenFile, out)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("google photos: OAuth2 client credentials given but no token (token_file/token_json/refresh_token/access_token)")
	}

	if tok.AccessToken == "" && tok.RefreshToken == "" {
		return nil, errors.New("google photos: token has neither an access_token nor a refresh_token")
	}

	// cfg.TokenSource refreshes the access token transparently through the
	// refresh_token. With a token file to write to, a refresh token that
	// Google no longer accepts sends the user through the consent flow again
	// instead of failing every backup.
	src := &reauthSource{ctx: ctx, cfg: cfg, tokenFile: tokenFile, out: out, inner: cfg.TokenSource(ctx, tok)}
	return src, nil
}

// reauthSource wraps a refreshing token source and re-runs the consent flow
// when the refresh token is rejected (Google answers "invalid_grant" once a
// refresh token is revoked, or expired, which happens after 7 days for an
// OAuth client left in "Testing" in the Cloud console).
type reauthSource struct {
	ctx       context.Context
	cfg       *oauth2.Config
	tokenFile string
	out       io.Writer

	mu    sync.Mutex
	inner oauth2.TokenSource
}

func (s *reauthSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tok, err := s.inner.Token()
	if err == nil {
		return tok, nil
	}

	var rerr *oauth2.RetrieveError
	if s.tokenFile == "" || !errors.As(err, &rerr) || rerr.ErrorCode != "invalid_grant" {
		return nil, err
	}

	fmt.Fprintf(s.out, "google photos: the saved token is no longer accepted by Google (%s), authorization is needed again\n", rerr.ErrorCode)
	tok, err = consentAndSave(s.ctx, s.cfg, s.tokenFile, s.out)
	if err != nil {
		return nil, err
	}
	s.inner = s.cfg.TokenSource(s.ctx, tok)
	return tok, nil
}

func consentAndSave(ctx context.Context, cfg *oauth2.Config, tokenFile string, out io.Writer) (*oauth2.Token, error) {
	tok, err := consentFlow(ctx, cfg, out)
	if err != nil {
		return nil, err
	}
	if err := saveToken(tokenFile, tok); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "google photos: token saved to %s\n", tokenFile)
	return tok, nil
}

// consentFlow runs the OAuth2 authorization-code flow with PKCE and a
// loopback redirect: it prints the consent URL for the user to open, waits
// for Google to redirect the browser back to a local listener with the
// code, and exchanges that code for a token.
func consentFlow(ctx context.Context, cfg *oauth2.Config, out io.Writer) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("google photos: cannot listen for the OAuth2 redirect: %w", err)
	}
	defer ln.Close()

	// A copy: the redirect URL is specific to this listener.
	cfg = &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     cfg.Endpoint,
		Scopes:       cfg.Scopes,
		RedirectURL:  "http://" + ln.Addr().String(),
	}

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, err
	}
	state := hex.EncodeToString(stateBytes)
	verifier := oauth2.GenerateVerifier()

	type outcome struct {
		code string
		err  error
	}
	done := make(chan outcome, 1)

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
			done <- outcome{err: fmt.Errorf("google photos: authorization refused: %s", e)}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Google Photos access granted to plakar. You can close this window.")
		done <- outcome{code: code}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce, oauth2.S256ChallengeOption(verifier))
	prompt(out, "grant plakar access to your library", authURL)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case o := <-done:
		if o.err != nil {
			return nil, o.err
		}
		tok, err := cfg.Exchange(ctx, o.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("google photos: authorization code exchange rejected: %w", err)
		}
		return tok, nil
	}
}

func saveToken(path string, tok *oauth2.Token) error {
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("google photos: cannot save token to %q: %w", path, err)
	}
	return nil
}

// readInlineOrFile returns the value of inlineKey if set, otherwise the
// contents of the file named by fileKey, or nil if neither is set.
func readInlineOrFile(params map[string]string, inlineKey, fileKey string) ([]byte, error) {
	if v, ok := params[inlineKey]; ok && v != "" {
		return []byte(v), nil
	}
	if v, ok := params[fileKey]; ok && v != "" {
		data, err := os.ReadFile(v)
		if err != nil {
			return nil, fmt.Errorf("google photos: cannot read %s %q: %w", fileKey, v, err)
		}
		return data, nil
	}
	return nil, nil
}

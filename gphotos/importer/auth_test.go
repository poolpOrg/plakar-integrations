package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// promptBuffer collects what the consent flow prints for the user and lets
// a test wait for the consent URL without polling.
type promptBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	urls chan string
}

var consentURLRe = regexp.MustCompile(`https://accounts\.example/auth\?\S+`)

func newPromptBuffer() *promptBuffer {
	return &promptBuffer{urls: make(chan string, 8)}
}

func (b *promptBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if u := consentURLRe.FindString(string(p)); u != "" {
		b.urls <- u
	}
	return n, err
}

func (b *promptBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// consentURL blocks until the flow has printed a consent URL.
func (b *promptBuffer) consentURL(t *testing.T) *url.URL {
	t.Helper()
	select {
	case s := <-b.urls:
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return u
	case <-time.After(5 * time.Second):
		t.Fatalf("consent URL never printed:\n%s", b.String())
		return nil
	}
}

// grant plays the browser: it follows the loopback redirect with the given
// code and the state from the consent URL.
func grant(t *testing.T, u *url.URL, code string) *http.Response {
	t.Helper()
	q := u.Query()
	resp, err := http.Get(q.Get("redirect_uri") + "/?state=" + url.QueryEscape(q.Get("state")) + "&code=" + code)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// Tests never open the real browser: every prompt falls back to printing.
func TestMain(m *testing.M) {
	openBrowser = func(string) error { return errors.New("no browser in tests") }
	os.Exit(m.Run())
}

func clientJSON(tokenURL string) string {
	return `{"installed":{"client_id":"id","client_secret":"secret","auth_uri":"https://accounts.example/auth","token_uri":"` + tokenURL + `","redirect_uris":["http://localhost"]}}`
}

// fakeTokenEndpoint answers authorization-code exchanges with a fresh token
// and refreshes according to acceptRefresh.
func fakeTokenEndpoint(t *testing.T, acceptRefresh bool) (*httptest.Server, *[]url.Values) {
	var seen []url.Values
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		seen = append(seen, r.Form)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("grant_type") == "refresh_token" && !acceptRefresh {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "Token has been expired or revoked."})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-" + r.Form.Get("grant_type"),
			"refresh_token": "rt-new",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// TestConsentFlow drives the first-run consent flow the way a browser would:
// it reads the consent URL printed for the user, follows the redirect back to
// the loopback listener with an authorization code, and checks that the code
// is exchanged against the token endpoint and the token saved to token_file.
func TestConsentFlow(t *testing.T) {
	tokenSrv, seen := fakeTokenEndpoint(t, true)
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	cfg := map[string]string{
		"credentials_json": clientJSON(tokenSrv.URL),
		"token_file":       tokenFile,
	}

	out := newPromptBuffer()
	type result struct {
		ts  oauth2.TokenSource
		err error
	}
	done := make(chan result, 1)
	go func() {
		ts, err := tokenSource(context.Background(), cfg, out)
		done <- result{ts, err}
	}()

	u := out.consentURL(t)
	q := u.Query()
	if q.Get("access_type") != "offline" {
		t.Errorf("consent URL should request offline access: %s", u)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Errorf("consent URL should carry a PKCE challenge: %s", u)
	}

	if resp := grant(t, u, "the-code"); resp.StatusCode != http.StatusOK {
		t.Errorf("redirect handler returned %s", resp.Status)
	}

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(*seen) != 1 || (*seen)[0].Get("code") != "the-code" || (*seen)[0].Get("code_verifier") == "" {
		t.Errorf("token endpoint requests = %v", *seen)
	}

	tok, err := r.ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at-authorization_code" || tok.RefreshToken != "rt-new" {
		t.Errorf("unexpected token %+v", tok)
	}

	saved, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk oauth2.Token
	if err := json.Unmarshal(saved, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.RefreshToken != "rt-new" {
		t.Errorf("saved token = %s", saved)
	}
	if info, _ := os.Stat(tokenFile); info.Mode().Perm()&0077 != 0 {
		t.Errorf("token file should not be group/world readable: %v", info.Mode())
	}
}

// TestRevokedRefreshTokenReconsents covers the OAuth client left in
// "Testing" in the Cloud console: after 7 days Google rejects the refresh
// token with invalid_grant, and the importer must send the user through the
// consent flow again rather than fail every backup.
func TestRevokedRefreshTokenReconsents(t *testing.T) {
	tokenSrv, _ := fakeTokenEndpoint(t, false)
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	expired := `{"access_token":"stale","refresh_token":"rt-old","expiry":"2000-01-01T00:00:00Z"}`
	if err := os.WriteFile(tokenFile, []byte(expired), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]string{
		"credentials_json": clientJSON(tokenSrv.URL),
		"token_file":       tokenFile,
	}

	out := newPromptBuffer()
	ts, err := tokenSource(context.Background(), cfg, out)
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		tok *oauth2.Token
		err error
	}
	done := make(chan result, 1)
	go func() {
		tok, err := ts.Token()
		done <- result{tok, err}
	}()

	grant(t, out.consentURL(t), "again")

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.tok.AccessToken != "at-authorization_code" {
		t.Errorf("token after re-consent = %+v", r.tok)
	}
	saved, _ := os.ReadFile(tokenFile)
	if !bytes.Contains(saved, []byte("rt-new")) {
		t.Errorf("re-consented token not saved: %s", saved)
	}

	// The new token is cached: no second consent for the next request.
	if _, err := ts.Token(); err != nil {
		t.Fatal(err)
	}
	select {
	case u := <-out.urls:
		t.Errorf("unexpected second consent prompt: %s", u)
	default:
	}
}

func TestConsentFlowRejectsWrongState(t *testing.T) {
	cfg := map[string]string{
		"credentials_json": clientJSON("https://accounts.example/token"),
		"token_file":       filepath.Join(t.TempDir(), "token.json"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := newPromptBuffer()
	errc := make(chan error, 1)
	go func() {
		_, err := tokenSource(ctx, cfg, out)
		errc <- err
	}()

	u := out.consentURL(t)
	resp, err := http.Get(u.Query().Get("redirect_uri") + "/?state=forged&code=x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("forged state accepted: %s", resp.Status)
	}

	// The forged redirect must not have completed the flow: only the
	// cancellation ends it.
	cancel()
	if err := <-errc; err == nil || err == io.EOF {
		t.Errorf("cancelled flow should fail with the context error, got %v", err)
	}
}

func TestTokenSourceWithoutTokenFileDoesNotReconsent(t *testing.T) {
	tokenSrv, _ := fakeTokenEndpoint(t, false)
	cfg := map[string]string{
		"credentials_json": clientJSON(tokenSrv.URL),
		"refresh_token":    "rt-old",
	}
	out := newPromptBuffer()
	ts, err := tokenSource(context.Background(), cfg, out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Token(); err == nil {
		t.Error("rejected refresh token should surface as an error when there is no token_file to renew")
	}
	select {
	case u := <-out.urls:
		t.Errorf("consent flow should not run without a token_file: %s", u)
	default:
	}
}

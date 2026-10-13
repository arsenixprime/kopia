//go:build !no_extra_providers

package gdrive

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/internal/testutil"
)

// A syntactically valid but entirely fake service account key. google's
// JWTConfigFromJSON does not parse the PEM, so no real key material is needed
// and none is present.
const fakeServiceAccountJSON = `{
  "type": "service_account",
  "project_id": "kopia-test",
  "private_key_id": "0000000000000000000000000000000000000000",
  "private_key": "-----BEGIN PRIVATE KEY-----\nnot-a-real-key\n-----END PRIVATE KEY-----\n",
  "client_email": "kopia-test@kopia-test.iam.gserviceaccount.com",
  "client_id": "100000000000000000000",
  "token_uri": "https://oauth2.googleapis.com/token"
}`

const fakeAuthorizedUserJSON = `{
  "type": "authorized_user",
  "client_id": "cid.apps.googleusercontent.com",
  "client_secret": "not-a-real-secret",
  "refresh_token": "not-a-real-refresh-token"
}`

// The shape verified live on 2026-08-01: a valid authorized user document that
// simply has no "type" key.
const fakeAuthorizedUserNoTypeJSON = `{
  "client_id": "cid.apps.googleusercontent.com",
  "client_secret": "not-a-real-secret",
  "refresh_token": "not-a-real-refresh-token"
}`

const fakeInstalledClientJSON = `{
  "installed": {
    "client_id": "cid.apps.googleusercontent.com",
    "client_secret": "not-a-real-secret",
    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
    "token_uri": "https://oauth2.googleapis.com/token"
  }
}`

func TestSniffCredentialJSON(t *testing.T) {
	cases := []struct {
		name         string
		input        string
		wantKind     string
		wantInferred bool
		wantErrParts []string
	}{
		{
			name:     "service account",
			input:    fakeServiceAccountJSON,
			wantKind: credentialTypeServiceAccount,
		},
		{
			name:     "authorized user",
			input:    fakeAuthorizedUserJSON,
			wantKind: credentialTypeAuthorizedUser,
		},
		{
			name:         "authorized user without type",
			input:        fakeAuthorizedUserNoTypeJSON,
			wantKind:     credentialTypeAuthorizedUser,
			wantInferred: true,
		},
		{
			name:         "not json at all",
			input:        "this is not json",
			wantErrParts: []string{"service_account", "authorized_user", "client-credentials-file"},
		},
		{
			name:         "json but not a credential",
			input:        `{"hello":"world"}`,
			wantErrParts: []string{"service_account", "authorized_user", "refresh_token", "client-credentials-file"},
		},
		{
			name:         "missing type and missing refresh token",
			input:        `{"client_id":"a","client_secret":"b"}`,
			wantErrParts: []string{"refresh_token"},
		},
		{
			name:         "oauth client secret file passed as a credential",
			input:        fakeInstalledClientJSON,
			wantErrParts: []string{"installed", "client-credentials-file"},
		},
		{
			name:         "unknown type",
			input:        `{"type":"external_account"}`,
			wantErrParts: []string{"service_account", "authorized_user"},
		},
		{
			name:         "type is not a string",
			input:        `{"type":42}`,
			wantErrParts: []string{`"type" field`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc, err := sniffCredentialJSON([]byte(tc.input), "the test document")

			if len(tc.wantErrParts) > 0 {
				require.Error(t, err)

				for _, p := range tc.wantErrParts {
					require.Contains(t, err.Error(), p)
				}

				require.Contains(t, err.Error(), "the test document")

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantKind, pc.kind)
			require.Equal(t, tc.wantInferred, pc.typeInferred)

			// whatever we hand to the Google libraries must always carry a type.
			var m map[string]any

			require.NoError(t, json.Unmarshal(pc.data, &m))
			require.Equal(t, tc.wantKind, m["type"])
		})
	}
}

func TestCredentialTypeInjectionIsNotPersisted(t *testing.T) {
	ctx := testlogging.Context(t)
	fn := filepath.Join(t.TempDir(), "creds.json")
	original := []byte(fakeAuthorizedUserNoTypeJSON)

	require.NoError(t, os.WriteFile(fn, original, 0o600))

	ts, mode, err := newTokenSource(ctx, &Options{
		FolderID:                      "some-folder",
		ServiceAccountCredentialsFile: fn,
	}, drive.DriveFileScope)

	require.NoError(t, err)
	require.NotNil(t, ts)
	require.Equal(t, authModeAuthorizedUserInferred, mode)

	after, err := os.ReadFile(fn)

	require.NoError(t, err)
	require.Equal(t, string(original), string(after))
	require.NotContains(t, string(after), `"type"`)
}

func TestTokenSourceFromEmbeddedAuthorizedUser(t *testing.T) {
	ctx := testlogging.Context(t)

	ts, mode, err := newTokenSource(ctx, &Options{
		FolderID:                     "some-folder",
		ServiceAccountCredentialJSON: json.RawMessage(fakeAuthorizedUserJSON),
	}, drive.DriveFileScope)

	require.NoError(t, err)
	require.NotNil(t, ts)
	require.Equal(t, authModeAuthorizedUser, mode)
}

func TestTokenSourceFromBareServiceAccount(t *testing.T) {
	ctx := testlogging.Context(t)

	ts, mode, err := newTokenSource(ctx, &Options{
		FolderID:                     "some-folder",
		ServiceAccountCredentialJSON: json.RawMessage(fakeServiceAccountJSON),
	}, drive.DriveFileScope)

	require.NoError(t, err)
	require.NotNil(t, ts)
	require.Equal(t, authModeServiceAccount, mode)
}

func TestServiceAccountImpersonationSetsSubject(t *testing.T) {
	cfg, err := serviceAccountJWTConfig([]byte(fakeServiceAccountJSON), "", drive.DriveFileScope)

	require.NoError(t, err)
	require.Empty(t, cfg.Subject)
	require.Equal(t, []string{drive.DriveFileScope}, cfg.Scopes)

	cfg, err = serviceAccountJWTConfig([]byte(fakeServiceAccountJSON), "someone@example.com", drive.DriveFileScope)

	require.NoError(t, err)
	require.Equal(t, "someone@example.com", cfg.Subject)

	// the mode description must name the impersonated subject, and never the key.
	_, mode, err := serviceAccountTokenSource(testlogging.Context(t), []byte(fakeServiceAccountJSON), "someone@example.com", drive.DriveFileScope)

	require.NoError(t, err)
	require.Contains(t, mode, "someone@example.com")
	require.NotContains(t, mode, "PRIVATE KEY")
}

func TestImpersonationRejectedForNonServiceAccount(t *testing.T) {
	ctx := testlogging.Context(t)

	_, _, err := newTokenSource(ctx, &Options{
		FolderID:                     "some-folder",
		ServiceAccountCredentialJSON: json.RawMessage(fakeAuthorizedUserJSON),
		Impersonate:                  "someone@example.com",
	}, drive.DriveFileScope)

	require.ErrorContains(t, err, "service account")
}

func TestDriveScope(t *testing.T) {
	cases := []struct {
		name    string
		opt     Options
		want    string
		wantErr string
	}{
		{name: "default", opt: Options{}, want: drive.DriveFileScope},
		{name: "explicit drive.file", opt: Options{Scope: ScopeDriveFile}, want: drive.DriveFileScope},
		{name: "explicit drive", opt: Options{Scope: ScopeDrive}, want: drive.DriveScope},
		{name: "read only keeps legacy behavior", opt: Options{ReadOnly: true}, want: drive.DriveReadonlyScope},
		{name: "read only overrides scope", opt: Options{ReadOnly: true, Scope: ScopeDrive}, want: drive.DriveReadonlyScope},
		{name: "bad scope", opt: Options{Scope: "drive.readonly"}, wantErr: "unsupported Google Drive scope"},
		{name: "bad scope is validated even when read only", opt: Options{ReadOnly: true, Scope: "nonsense"}, wantErr: "unsupported Google Drive scope"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := driveScope(&tc.opt)

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestTokenCachePath(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "my-token.json")

	got, err := tokenCachePath(&Options{TokenCacheFile: explicit}, "cid")

	require.NoError(t, err)
	require.Equal(t, explicit, got)

	a, err := tokenCachePath(&Options{FolderID: "folder-a"}, "cid")
	require.NoError(t, err)

	b, err := tokenCachePath(&Options{FolderID: "folder-b"}, "cid")
	require.NoError(t, err)

	c, err := tokenCachePath(&Options{FolderID: "folder-a"}, "other-cid")
	require.NoError(t, err)

	require.NotEqual(t, a, b)
	require.NotEqual(t, a, c)
	require.Contains(t, filepath.Base(a), "gdrive-token-")
	require.Equal(t, ".json", filepath.Ext(a))

	// stable across calls.
	again, err := tokenCachePath(&Options{FolderID: "folder-a"}, "cid")

	require.NoError(t, err)
	require.Equal(t, a, again)
}

func TestOAuthClientCredentialsFileErrors(t *testing.T) {
	dir := t.TempDir()

	notJSON := filepath.Join(dir, "not-json.json")
	require.NoError(t, os.WriteFile(notJSON, []byte("nope"), 0o600))

	_, err := oauthConfigFromClientCredentialsFile(notJSON, drive.DriveFileScope)
	require.ErrorContains(t, err, `"installed"`)

	wrongShape := filepath.Join(dir, "wrong.json")
	require.NoError(t, os.WriteFile(wrongShape, []byte(fakeAuthorizedUserJSON), 0o600))

	_, err = oauthConfigFromClientCredentialsFile(wrongShape, drive.DriveFileScope)
	require.ErrorContains(t, err, "Desktop app")

	_, err = oauthConfigFromClientCredentialsFile(filepath.Join(dir, "missing.json"), drive.DriveFileScope)
	require.ErrorContains(t, err, "error reading OAuth client credentials file")
}

func TestDeviceFlowRejectsFullDriveScope(t *testing.T) {
	ctx := testlogging.Context(t)
	dir := t.TempDir()
	ccFile := filepath.Join(dir, "client.json")

	require.NoError(t, os.WriteFile(ccFile, []byte(fakeInstalledClientJSON), 0o600))

	_, _, err := newTokenSource(ctx, &Options{
		FolderID:              "some-folder",
		ClientCredentialsFile: ccFile,
		TokenCacheFile:        filepath.Join(dir, "token.json"),
		UseDeviceFlow:         true,
		Scope:                 ScopeDrive,
	}, drive.DriveScope)

	require.ErrorContains(t, err, "device flow only supports")
	require.ErrorContains(t, err, ScopeDriveFile)
}

// --- loopback flow, without Google -----------------------------------------

const (
	testClientID     = "test-client-id.apps.googleusercontent.com"
	testClientSecret = "test-client-secret"
	testAuthCode     = "test-authorization-code"
	testRefreshToken = "test-refresh-token"
)

// fakeGoogle is a minimal stand-in for Google's OAuth token endpoint that also
// verifies PKCE.
type fakeGoogle struct {
	*httptest.Server

	mu              sync.Mutex
	challenge       string
	challengeMethod string
	authURLParams   url.Values
	tokenCalls      int
	pkceVerified    bool
	lastGrantError  string
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()

	f := &fakeGoogle{}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", f.handleToken)

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)

	return f
}

func (f *fakeGoogle) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)

		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.tokenCalls++

	if r.Form.Get("code") != testAuthCode {
		f.lastGrantError = "unexpected authorization code"

		http.Error(w, "invalid_grant", http.StatusBadRequest)

		return
	}

	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if f.challenge == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
		f.lastGrantError = "PKCE verifier does not match the challenge"

		http.Error(w, "invalid_grant", http.StatusBadRequest)

		return
	}

	f.pkceVerified = true

	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"access_token":"test-access-token","token_type":"Bearer","refresh_token":"`+testRefreshToken+`","expires_in":3600}`) //nolint:errcheck
}

func (f *fakeGoogle) clientCredentialsFile(t *testing.T, dir string) string {
	t.Helper()

	fn := filepath.Join(dir, "client_secret.json")
	body, err := json.Marshal(oauthClientFile{
		Installed: &oauthClientSection{
			ClientID:     testClientID,
			ClientSecret: testClientSecret,
			AuthURI:      f.URL + "/auth",
			TokenURI:     f.URL + "/token",
		},
	})

	require.NoError(t, err)
	require.NoError(t, os.WriteFile(fn, body, 0o600))

	return fn
}

// fakeBrowser stands in for the human plus their browser: it reads the auth URL
// Kopia printed and performs the redirect back to the loopback listener.
type fakeBrowser struct {
	once        sync.Once
	done        chan struct{}
	writes      int
	mutateState func(string) string
	onAuthURL   func(u *url.URL)
}

func newFakeBrowser() *fakeBrowser {
	return &fakeBrowser{done: make(chan struct{})}
}

func (b *fakeBrowser) Write(p []byte) (int, error) {
	b.writes++

	b.once.Do(func() {
		authURL := findURLContaining(string(p), "response_type=code")

		go func() {
			defer close(b.done)

			b.visit(authURL)
		}()
	})

	return len(p), nil
}

func (b *fakeBrowser) visit(authURL string) {
	u, err := url.Parse(authURL)
	if err != nil {
		return
	}

	q := u.Query()

	if b.onAuthURL != nil {
		b.onAuthURL(u)
	}

	state := q.Get("state")
	if b.mutateState != nil {
		state = b.mutateState(state)
	}

	target := q.Get("redirect_uri") + "?state=" + url.QueryEscape(state) + "&code=" + url.QueryEscape(testAuthCode)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, http.NoBody)
	if err != nil {
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}

	defer resp.Body.Close() //nolint:errcheck

	io.Copy(io.Discard, resp.Body) //nolint:errcheck
}

func findURLContaining(s, marker string) string {
	for f := range strings.FieldsSeq(s) {
		if strings.HasPrefix(f, "http") && strings.Contains(f, marker) {
			return f
		}
	}

	return ""
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// useAuthPrompt installs w as the interactive prompt sink for the duration of
// the test.
func useAuthPrompt(t *testing.T, w io.Writer) {
	t.Helper()

	old := authPromptWriter
	authPromptWriter = w

	t.Cleanup(func() { authPromptWriter = old })
}

func TestLoopbackFlowAndTokenCache(t *testing.T) {
	ctx := testlogging.Context(t)
	g := newFakeGoogle(t)
	dir := t.TempDir()

	opt := &Options{
		FolderID:              "some-folder",
		ClientCredentialsFile: g.clientCredentialsFile(t, dir),
		TokenCacheFile:        filepath.Join(dir, "cache-subdir", "token.json"),
	}

	browser := newFakeBrowser()
	browser.onAuthURL = func(u *url.URL) {
		q := u.Query()

		g.mu.Lock()
		g.challenge = q.Get("code_challenge")
		g.challengeMethod = q.Get("code_challenge_method")
		g.authURLParams = q
		g.mu.Unlock()
	}

	useAuthPrompt(t, browser)

	ts, mode, err := newTokenSource(ctx, opt, drive.DriveFileScope)

	require.NoError(t, err)
	require.NotNil(t, ts)
	require.Equal(t, authModeLoopback, mode)

	<-browser.done

	g.mu.Lock()
	require.Equal(t, 1, g.tokenCalls)
	require.True(t, g.pkceVerified, "PKCE was not verified: %v", g.lastGrantError)
	require.NotEmpty(t, g.challenge)

	// the auth URL must ask for S256 PKCE, offline access, consent, a loopback
	// redirect and the requested scope.
	require.Equal(t, "S256", g.challengeMethod)
	require.Equal(t, "offline", g.authURLParams.Get("access_type"))
	require.Equal(t, "consent", g.authURLParams.Get("prompt"))
	require.Equal(t, "code", g.authURLParams.Get("response_type"))
	require.Equal(t, testClientID, g.authURLParams.Get("client_id"))
	require.Equal(t, drive.DriveFileScope, g.authURLParams.Get("scope"))
	require.Regexp(t, `^http://127\.0\.0\.1:\d+/$`, g.authURLParams.Get("redirect_uri"))
	require.NotContains(t, g.authURLParams.Encode(), testClientSecret)
	g.mu.Unlock()

	st, err := os.Stat(opt.TokenCacheFile)
	require.NoError(t, err)

	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	}

	cached, err := os.ReadFile(opt.TokenCacheFile)
	require.NoError(t, err)

	var au authorizedUserJSON

	require.NoError(t, json.Unmarshal(cached, &au))
	require.Equal(t, credentialTypeAuthorizedUser, au.Type)
	require.Equal(t, testClientID, au.ClientID)
	require.Equal(t, testRefreshToken, au.RefreshToken)

	// the cached document must be directly usable as a credentials document.
	pc, err := sniffCredentialJSON(cached, "the token cache")
	require.NoError(t, err)
	require.Equal(t, credentialTypeAuthorizedUser, pc.kind)
	require.False(t, pc.typeInferred)

	// second call must be served from the cache: no prompt, no browser, no
	// additional call to the token endpoint.
	browser2 := newFakeBrowser()
	useAuthPrompt(t, browser2)

	ts2, mode2, err := newTokenSource(ctx, opt, drive.DriveFileScope)

	require.NoError(t, err)
	require.NotNil(t, ts2)
	require.Equal(t, authModeCachedToken, mode2)
	require.Zero(t, browser2.writes, "the cached path must not prompt")

	g.mu.Lock()
	require.Equal(t, 1, g.tokenCalls)
	g.mu.Unlock()
}

func TestLoopbackFlowRejectsWrongState(t *testing.T) {
	ctx := testlogging.Context(t)
	g := newFakeGoogle(t)
	dir := t.TempDir()

	opt := &Options{
		FolderID:              "some-folder",
		ClientCredentialsFile: g.clientCredentialsFile(t, dir),
		TokenCacheFile:        filepath.Join(dir, "token.json"),
	}

	browser := newFakeBrowser()
	browser.mutateState = func(string) string { return "not-the-state-we-sent" }

	useAuthPrompt(t, browser)

	_, _, err := newTokenSource(ctx, opt, drive.DriveFileScope)

	require.ErrorContains(t, err, "invalid state parameter")

	<-browser.done

	g.mu.Lock()
	require.Zero(t, g.tokenCalls, "no code must be exchanged when state does not match")
	g.mu.Unlock()

	require.NoFileExists(t, opt.TokenCacheFile)
}

func TestLoopbackFlowHonorsCanceledContext(t *testing.T) {
	g := newFakeGoogle(t)
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(testlogging.Context(t))
	prompted := make(chan struct{})

	// cancel as soon as the prompt has been printed.
	useAuthPrompt(t, writerFunc(func(p []byte) (int, error) {
		close(prompted)

		return len(p), nil
	}))

	go func() {
		<-prompted

		cancel()
	}()

	_, _, err := newTokenSource(ctx, &Options{
		FolderID:              "some-folder",
		ClientCredentialsFile: g.clientCredentialsFile(t, dir),
		TokenCacheFile:        filepath.Join(dir, "token.json"),
	}, drive.DriveFileScope)

	require.ErrorContains(t, err, "canceled")
}

func TestCachedTokenForDifferentClientIsIgnored(t *testing.T) {
	dir := t.TempDir()
	fn := filepath.Join(dir, "token.json")

	require.NoError(t, os.WriteFile(fn, []byte(`{"type":"authorized_user","client_id":"someone-else","refresh_token":"r"}`), 0o600))

	_, err := loadCachedRefreshToken(fn, testClientID)
	require.ErrorContains(t, err, "different OAuth client")

	require.NoError(t, os.WriteFile(fn, []byte(`{"type":"authorized_user","client_id":"`+testClientID+`"}`), 0o600))

	_, err = loadCachedRefreshToken(fn, testClientID)
	require.ErrorContains(t, err, "no refresh_token")

	require.NoError(t, os.WriteFile(fn, []byte(`garbage`), 0o600))

	_, err = loadCachedRefreshToken(fn, testClientID)
	require.ErrorContains(t, err, "not valid JSON")

	_, err = loadCachedRefreshToken(filepath.Join(dir, "absent.json"), testClientID)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSaveAuthorizedUserTokenRequiresRefreshToken(t *testing.T) {
	fn := filepath.Join(t.TempDir(), "token.json")

	err := saveAuthorizedUserToken(fn, &oauth2.Config{ClientID: testClientID}, &oauth2.Token{AccessToken: "a"})

	require.ErrorContains(t, err, "refresh token")
	require.NoFileExists(t, fn)
}

// --- live -------------------------------------------------------------------

// TestLiveTokenSource authenticates against real Google Drive and makes exactly
// one files.list call. Skipped unless KOPIA_PROVIDER_TEST and the Drive test
// environment variables are set.
func TestLiveTokenSource(t *testing.T) {
	testutil.ProviderTest(t)

	credentialsFile := getEnvOrSkip(t, "KOPIA_GDRIVE_TEST_CREDENTIALS")
	folderID := getEnvOrSkip(t, "KOPIA_GDRIVE_TEST_FOLDER")

	ctx := testlogging.Context(t)

	opt := &Options{
		FolderID:                      folderID,
		ServiceAccountCredentialsFile: credentialsFile,
	}

	scope, err := driveScope(opt)
	require.NoError(t, err)
	require.Equal(t, drive.DriveFileScope, scope)

	ts, mode, err := newTokenSource(ctx, opt, scope)

	require.NoError(t, err)
	require.NotNil(t, ts)
	require.NotEmpty(t, mode)

	t.Logf("resolved Google Drive auth mode: %v", mode)

	svc, err := drive.NewService(ctx, option.WithHTTPClient(oauth2.NewClient(ctx, ts)))
	require.NoError(t, err)

	res, err := svc.Files.List().
		Q("'" + folderID + "' in parents and trashed = false").
		PageSize(1).
		Fields("files(id,name)").
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true).
		Context(ctx).
		Do()

	require.NoError(t, err)
	t.Logf("files.list against the test folder returned %v file(s)", len(res.Files))
}

func getEnvOrSkip(t *testing.T, name string) string {
	t.Helper()

	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%q is not set", name)
	}

	return v
}

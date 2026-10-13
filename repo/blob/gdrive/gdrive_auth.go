//go:build !no_extra_providers

package gdrive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"
	drive "google.golang.org/api/drive/v3"

	"github.com/kopia/kopia/internal/ospath"
)

// Supported values for Options.Scope.
//
// ScopeDriveFile ("drive.file") is the default and by far the safer choice: it
// is a non-sensitive scope which grants Kopia access only to the files and
// folders it created itself (or that the user explicitly handed to it through
// a Google Picker). A compromised or misbehaving Kopia can therefore never
// touch the rest of the user's Drive. The cost is that a folder created by
// hand in the Drive web UI is invisible to Kopia - a files.get on it returns
// 404 - so under this scope the backend must create its own repository folder.
//
// ScopeDrive ("drive") is Google's restricted "view and manage all your Drive
// files" scope. It is required only when the repository must live in a
// pre-existing, externally created folder.
const (
	ScopeDriveFile = "drive.file"
	ScopeDrive     = "drive"
)

// Recognized values of the "type" key in a Google credentials JSON document.
const (
	credentialTypeServiceAccount = "service_account"
	credentialTypeAuthorizedUser = "authorized_user"
)

// Human-readable descriptions of the resolved authentication mode. These are
// safe to log: they never contain credential material.
const (
	authModeServiceAccount         = "service account"
	authModeAuthorizedUser         = "authorized user (OAuth refresh token)"
	authModeAuthorizedUserInferred = `authorized user (OAuth refresh token, "type" inferred)`
	authModeLoopback               = "interactive OAuth (loopback redirect)"
	authModeDevice                 = "interactive OAuth (device code)"
	authModeCachedToken            = "interactive OAuth (cached refresh token)"
	authModeDefaultCredentials     = "application default credentials"
)

const (
	// number of random bytes behind the OAuth `state` parameter.
	oauthStateBytes = 32

	// number of bytes of the SHA-256 digest used to name the default token cache file.
	tokenCacheHashBytes = 8

	tokenCacheFileMode = os.FileMode(0o600)
	tokenCacheDirMode  = os.FileMode(0o700)

	loopbackReadHeaderTimeout = 10 * time.Second

	// loopbackListenAddr binds the loopback interface on an ephemeral port.
	// Google ignores the port when matching loopback redirect URIs, and prefers
	// 127.0.0.1 over "localhost" because some client firewalls block the latter.
	loopbackListenAddr = "127.0.0.1:0"
)

// authPromptWriter is where the interactive flows print their instructions.
// Kopia frequently runs on a remote or headless machine, so we deliberately do
// NOT try to launch a browser; we print the URL and let the operator open it
// wherever they are. Overridden by tests.
//
//nolint:gochecknoglobals
var authPromptWriter io.Writer = os.Stderr

// loopbackSuccessHTML is served to the browser once the authorization code has
// been captured. It is fully self-contained on purpose.
const loopbackSuccessHTML = `<!doctype html><html><head><meta charset="utf-8">` +
	`<title>Kopia - Google Drive authorized</title></head><body>` +
	`<h2>Kopia is now authorized to use Google Drive.</h2>` +
	`<p>You can close this window and return to the terminal.</p></body></html>`

// driveScope maps Options onto the OAuth scope Kopia asks Google for.
//
// ReadOnly keeps the behavior of the original backend: it uses the
// drive.readonly scope and takes precedence over Scope. Scope is still
// validated in that case so that a typo is reported rather than ignored.
func driveScope(opt *Options) (string, error) {
	var scope string

	switch opt.Scope {
	case "", ScopeDriveFile:
		scope = drive.DriveFileScope
	case ScopeDrive:
		scope = drive.DriveScope
	default:
		return "", errors.Errorf("unsupported Google Drive scope %q, supported values are %q (the default) and %q", opt.Scope, ScopeDriveFile, ScopeDrive)
	}

	if opt.ReadOnly {
		return drive.DriveReadonlyScope, nil
	}

	return scope, nil
}

// newTokenSource resolves credentials per Options, returning a TokenSource and
// a description of the auth mode for logging (never log the credentials).
//
// Credential resolution order:
//
//  1. Options.ServiceAccountCredentialJSON (embedded JSON), then
//     Options.ServiceAccountCredentialsFile (path). Despite their historical
//     names both accept EITHER a service account key JSON or a Google
//     "authorized user" JSON; the document's "type" key selects between them.
//  2. Options.ClientCredentialsFile - an OAuth client secret JSON downloaded
//     from the Google Cloud console. This runs an interactive sign-in
//     (loopback redirect, or device code when Options.UseDeviceFlow is set)
//     and caches the resulting refresh token so later runs are silent.
//  3. Neither - Google Application Default Credentials, which is what the
//     original backend did when no credentials were configured.
func newTokenSource(ctx context.Context, opt *Options, scope string) (oauth2.TokenSource, string, error) {
	switch {
	case len(opt.ServiceAccountCredentialJSON) > 0:
		return tokenSourceFromCredentialJSON(ctx, opt.ServiceAccountCredentialJSON, "the embedded credentials", opt, scope)

	case opt.ServiceAccountCredentialsFile != "":
		data, err := os.ReadFile(opt.ServiceAccountCredentialsFile)
		if err != nil {
			return nil, "", errors.Wrap(err, "error reading credentials file")
		}

		return tokenSourceFromCredentialJSON(ctx, data, opt.ServiceAccountCredentialsFile, opt, scope)

	case opt.ClientCredentialsFile != "":
		return newInteractiveTokenSource(ctx, opt, scope)

	default:
		if opt.Impersonate != "" {
			return nil, "", errors.New("impersonation requires explicit service account credentials, it cannot be combined with application default credentials")
		}

		ts, err := google.DefaultTokenSource(ctx, scope)
		if err != nil {
			return nil, "", errors.Wrap(err, "unable to obtain Google application default credentials")
		}

		return ts, authModeDefaultCredentials, nil
	}
}

// parsedCredential is the result of sniffing a Google credentials JSON blob.
type parsedCredential struct {
	// data is the (possibly rewritten) JSON to hand to the Google libraries.
	data []byte

	// kind is one of the credentialType* constants.
	kind string

	// typeInferred is true when the document had no "type" key and we supplied one.
	typeInferred bool
}

// tokenSourceFromCredentialJSON sniffs a credentials document and builds the
// matching token source. source names where the document came from and is only
// used in error messages - it is never the document itself.
func tokenSourceFromCredentialJSON(ctx context.Context, data []byte, source string, opt *Options, scope string) (oauth2.TokenSource, string, error) {
	pc, err := sniffCredentialJSON(data, source)
	if err != nil {
		return nil, "", err
	}

	if pc.kind == credentialTypeServiceAccount {
		return serviceAccountTokenSource(ctx, pc.data, opt.Impersonate, scope)
	}

	if opt.Impersonate != "" {
		return nil, "", errors.New("impersonation (domain-wide delegation) requires a service account key, but the supplied credentials are an authorized user token")
	}

	ts, err := authorizedUserTokenSource(ctx, pc.data, scope)
	if err != nil {
		return nil, "", errors.Wrapf(err, "unable to use the authorized user credentials from %v", source)
	}

	if pc.typeInferred {
		return ts, authModeAuthorizedUserInferred, nil
	}

	return ts, authModeAuthorizedUser, nil
}

// authorizedUserTokenSource builds a refreshing token source from a Google
// "authorized user" document.
//
// google.CredentialsFromJSON is deliberately not used here: it carries a
// deprecation notice pointing at a different module. The construction below is
// exactly what that function performs for authorized_user documents, and doing
// it here also keeps the token URI overridable.
func authorizedUserTokenSource(ctx context.Context, data []byte, scope string) (oauth2.TokenSource, error) {
	var au authorizedUserJSON

	if err := json.Unmarshal(data, &au); err != nil {
		return nil, errors.Wrap(err, "unable to parse the authorized user credentials")
	}

	if au.ClientID == "" || au.RefreshToken == "" {
		return nil, errors.New("the authorized user credentials need both a client_id and a refresh_token")
	}

	cfg := oauth2.Config{
		ClientID:     au.ClientID,
		ClientSecret: au.ClientSecret,
		Scopes:       []string{scope},
		Endpoint: oauth2.Endpoint{
			AuthURL:   firstNonEmpty(au.AuthURI, google.Endpoint.AuthURL),
			TokenURL:  firstNonEmpty(au.TokenURI, google.Endpoint.TokenURL),
			AuthStyle: google.Endpoint.AuthStyle,
		},
	}

	// an empty AccessToken forces a refresh on first use.
	return cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: au.RefreshToken}), nil
}

// sniffCredentialJSON determines which flavor of Google credentials document
// it was given.
func sniffCredentialJSON(data []byte, source string) (parsedCredential, error) {
	var raw map[string]json.RawMessage

	if err := json.Unmarshal(data, &raw); err != nil {
		return parsedCredential{}, errors.Wrapf(err, "%v does not contain valid JSON, %v", source, acceptedCredentialFormats)
	}

	var credType string

	if v, ok := raw["type"]; ok {
		if err := json.Unmarshal(v, &credType); err != nil {
			return parsedCredential{}, errors.Wrapf(err, `the "type" field in %v is not a string`, source)
		}
	}

	switch credType {
	case credentialTypeServiceAccount, credentialTypeAuthorizedUser:
		return parsedCredential{data: data, kind: credType}, nil

	case "":
		// 2026-08-01: live-verified requirement. Refresh tokens minted by
		// several Google tools (and by the gcloud "application-default" flow of
		// some versions) are written without the "type" key, and
		// google.CredentialsFromJSON then rejects them with
		// `missing 'type' field in credentials`. When the document
		// unambiguously carries the three fields that define an authorized
		// user, supply the missing key in memory and carry on. The file on disk
		// is never rewritten.
		if hasAllKeys(raw, "refresh_token", "client_id", "client_secret") {
			raw["type"] = json.RawMessage(`"` + credentialTypeAuthorizedUser + `"`)

			patched, err := json.Marshal(raw)
			if err != nil {
				return parsedCredential{}, errors.Wrapf(err, "unable to normalize the credentials in %v", source)
			}

			return parsedCredential{data: patched, kind: credentialTypeAuthorizedUser, typeInferred: true}, nil
		}
	}

	return parsedCredential{}, errors.Errorf("unrecognized Google Drive credentials in %v, %v", source, acceptedCredentialFormats)
}

// acceptedCredentialFormats names every document shape the credentials options
// accept, so that a wrong file produces an actionable message rather than a
// library-internal one.
const acceptedCredentialFormats = `expected one of: a service account key JSON ("type":"service_account"), ` +
	`an authorized user JSON ("type":"authorized_user"), or an authorized user JSON without the "type" key ` +
	`but carrying client_id, client_secret and refresh_token. ` +
	`An OAuth client secret JSON (the one with an "installed" or "web" section) is not a credential - ` +
	`pass it as the client-credentials-file option instead to run the interactive sign-in flow`

func hasAllKeys(m map[string]json.RawMessage, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}

	return true
}

// bareServiceAccountWarning is issue #2656 in one paragraph.
const bareServiceAccountWarning = "Using a bare Google service account. Files Kopia creates will be owned by the " +
	"service account, not by you, and they count against the service account's own ~15 GB Drive quota which cannot be " +
	"increased (kopia issue #2656). Store the repository on a Shared Drive, or authenticate as a regular user, or use " +
	"domain-wide delegation via the impersonate-user option."

// serviceAccountTokenSource builds a JWT token source from a service account
// key, optionally impersonating a user via domain-wide delegation.
func serviceAccountTokenSource(ctx context.Context, data []byte, impersonate, scope string) (oauth2.TokenSource, string, error) {
	cfg, err := serviceAccountJWTConfig(data, impersonate, scope)
	if err != nil {
		return nil, "", err
	}

	if cfg.Subject == "" {
		// Issue #2656: a bare service account is its own Drive user. Everything
		// Kopia writes is owned by the service account and counts against the
		// account's own storage, which is ~15 GB and cannot be purchased or
		// raised. Warn loudly at startup; the fixes are a Shared Drive (whose
		// quota belongs to the organization), domain-wide delegation, or plain
		// user credentials.
		log(ctx).Warn(bareServiceAccountWarning)

		return cfg.TokenSource(ctx), authModeServiceAccount, nil
	}

	return cfg.TokenSource(ctx), authModeServiceAccount + " impersonating " + cfg.Subject, nil
}

// serviceAccountJWTConfig parses a service account key and applies
// impersonation. Split out from serviceAccountTokenSource so that it can be
// exercised without any network I/O.
//
// EXPERIMENTAL: the impersonation (domain-wide delegation) path is complete but
// has not been validated against a real Workspace domain - setting up DWD
// requires a Workspace super-admin to authorize the service account's client ID
// for the requested scope in the admin console. If it is not authorized, Google
// answers the token request with `unauthorized_client`.
func serviceAccountJWTConfig(data []byte, impersonate, scope string) (*jwt.Config, error) {
	cfg, err := google.JWTConfigFromJSON(data, scope)
	if err != nil {
		return nil, errors.Wrap(err, "google.JWTConfigFromJSON")
	}

	// Subject is what turns a plain service account token request into a
	// domain-wide-delegation request on behalf of that user.
	cfg.Subject = impersonate

	return cfg, nil
}

// oauthClientSection mirrors one half of a Google OAuth client secret JSON.
type oauthClientSection struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	AuthURI      string `json:"auth_uri"`
	TokenURI     string `json:"token_uri"`

	// DeviceAuthURI is not part of Google's downloadable client secret file; it
	// is honored as an override so that the device flow can be pointed at a
	// different authorization server.
	DeviceAuthURI string `json:"device_auth_uri"`
}

// oauthClientFile mirrors a Google OAuth client secret JSON document.
type oauthClientFile struct {
	Installed *oauthClientSection `json:"installed"`
	Web       *oauthClientSection `json:"web"`
}

// oauthConfigFromClientCredentialsFile loads an OAuth client secret JSON.
//
// google.ConfigFromJSON is deliberately not used: it insists on a
// `redirect_uris` entry (Desktop app clients created after 2022 often ship
// without one) and it drops the device authorization endpoint. Both matter
// here.
func oauthConfigFromClientCredentialsFile(fn, scope string) (*oauth2.Config, error) {
	data, err := os.ReadFile(fn) //nolint:gosec
	if err != nil {
		return nil, errors.Wrap(err, "error reading OAuth client credentials file")
	}

	var f oauthClientFile

	if err := json.Unmarshal(data, &f); err != nil {
		return nil, errors.Wrapf(err, "%v does not contain valid JSON, expected an OAuth client secret JSON with an \"installed\" or \"web\" section", fn)
	}

	section := f.Installed
	if section == nil {
		section = f.Web
	}

	if section == nil || section.ClientID == "" {
		return nil, errors.Errorf(`%v is not an OAuth client secret JSON: it has no "installed" or "web" section with a `+
			`client_id. Download one from the Google Cloud console under APIs & Services > Credentials, using the `+
			`"Desktop app" application type`, fn)
	}

	ep := oauth2.Endpoint{
		AuthURL:       firstNonEmpty(section.AuthURI, google.Endpoint.AuthURL),
		TokenURL:      firstNonEmpty(section.TokenURI, google.Endpoint.TokenURL),
		DeviceAuthURL: firstNonEmpty(section.DeviceAuthURI, google.Endpoint.DeviceAuthURL),
		AuthStyle:     google.Endpoint.AuthStyle,
	}

	return &oauth2.Config{
		ClientID:     section.ClientID,
		ClientSecret: section.ClientSecret,
		Scopes:       []string{scope},
		Endpoint:     ep,
	}, nil
}

func firstNonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}

	return fallback
}

// newInteractiveTokenSource returns a token source backed by a user
// authorization obtained interactively, reusing a cached refresh token when one
// is available.
func newInteractiveTokenSource(ctx context.Context, opt *Options, scope string) (oauth2.TokenSource, string, error) {
	if opt.Impersonate != "" {
		return nil, "", errors.New("impersonation (domain-wide delegation) requires a service account key, it cannot be combined with an interactive OAuth sign-in")
	}

	cfg, err := oauthConfigFromClientCredentialsFile(opt.ClientCredentialsFile, scope)
	if err != nil {
		return nil, "", err
	}

	cachePath, err := tokenCachePath(opt, cfg.ClientID)
	if err != nil {
		return nil, "", err
	}

	cached, err := loadCachedRefreshToken(cachePath, cfg.ClientID)

	switch {
	case err == nil:
		return cfg.TokenSource(ctx, cached), authModeCachedToken, nil

	case !errors.Is(err, fs.ErrNotExist):
		log(ctx).Warnf("ignoring unusable Google Drive token cache %v: %v", cachePath, err)
	}

	var (
		token *oauth2.Token
		mode  string
	)

	if opt.UseDeviceFlow {
		// RECON C.6: Google allows the OAuth 2.0 device flow only for
		// drive.appdata and drive.file. Full "drive" (and drive.readonly) are
		// not in the allowed-scope list and the device endpoint rejects them.
		if scope != drive.DriveFileScope {
			return nil, "", errors.Errorf("the OAuth device flow only supports the %q scope, not %q. Use the loopback flow (drop the device-flow option), forwarding a local port over SSH if this machine has no browser", ScopeDriveFile, scope)
		}

		token, err = runDeviceFlow(ctx, cfg)
		mode = authModeDevice
	} else {
		token, err = runLoopbackFlow(ctx, cfg)
		mode = authModeLoopback
	}

	if err != nil {
		return nil, "", err
	}

	if err := saveAuthorizedUserToken(cachePath, cfg, token); err != nil {
		return nil, "", err
	}

	log(ctx).Infof("Google Drive authorization saved to %v", cachePath)

	return cfg.TokenSource(ctx, token), mode, nil
}

// runLoopbackFlow performs the OAuth authorization code flow using a loopback
// redirect, which is the flow Google documents (and recommends) for desktop
// applications. The out-of-band "copy the code back into the terminal" flow was
// fully switched off by Google on 2023-01-31 and is not an option.
//
// The listener binds 127.0.0.1 on an ephemeral port; Google ignores the port
// when matching loopback redirect URIs, so no per-machine registration is
// needed. PKCE (S256) and a random, verified `state` protect the exchange.
func runLoopbackFlow(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", loopbackListenAddr)
	if err != nil {
		return nil, errors.Wrap(err, "unable to listen on a loopback port for the OAuth redirect")
	}

	defer listener.Close() //nolint:errcheck

	state, err := randomURLSafeString(oauthStateBytes)
	if err != nil {
		return nil, err
	}

	verifier := oauth2.GenerateVerifier()

	// copy so the caller's config keeps its empty RedirectURL, which is what
	// the refresh path wants.
	lcfg := *cfg
	lcfg.RedirectURL = "http://" + listener.Addr().String() + "/"

	codes := make(chan loopbackResult, 1)

	srv := &http.Server{
		Handler:           loopbackHandler(state, codes),
		ReadHeaderTimeout: loopbackReadHeaderTimeout,
	}

	go func() {
		_ = srv.Serve(listener)
	}()

	defer srv.Close() //nolint:errcheck

	authURL := lcfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.S256ChallengeOption(verifier),
	)

	fmt.Fprintf(authPromptWriter, //nolint:errcheck
		"\nKopia needs your permission to use Google Drive.\n\nOpen this URL in a browser, sign in and approve the request:\n\n    %v\n\nWaiting for the response on %v (press Ctrl-C to abort) ...\n\n",
		authURL, lcfg.RedirectURL)

	select {
	case <-ctx.Done():
		return nil, errors.Wrap(ctx.Err(), "canceled while waiting for the Google Drive authorization")

	case res := <-codes:
		if res.err != nil {
			return nil, res.err
		}

		token, err := lcfg.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, errors.Wrap(err, "unable to exchange the authorization code for a token")
		}

		return token, nil
	}
}

type loopbackResult struct {
	code string
	err  error
}

// loopbackHandler returns a one-shot handler that captures the authorization
// code from Google's redirect.
func loopbackHandler(state string, codes chan<- loopbackResult) http.Handler {
	deliver := func(r loopbackResult) {
		select {
		case codes <- r:
		default:
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		if !q.Has("code") && !q.Has("error") && !q.Has("state") {
			// stray request (favicon and friends), do not disturb the flow.
			http.NotFound(w, r)

			return
		}

		if e := q.Get("error"); e != "" {
			http.Error(w, "Authorization was not granted. You can close this window.", http.StatusForbidden)
			deliver(loopbackResult{err: errors.Errorf("Google Drive authorization was denied: %v", e)})

			return
		}

		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid state parameter.", http.StatusBadRequest)
			deliver(loopbackResult{err: errors.New("the OAuth redirect carried an invalid state parameter, the authorization was rejected")})

			return
		}

		code := q.Get("code")
		if code == "" {
			http.Error(w, "No authorization code in the redirect.", http.StatusBadRequest)
			deliver(loopbackResult{err: errors.New("the OAuth redirect did not carry an authorization code")})

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, loopbackSuccessHTML) //nolint:errcheck

		deliver(loopbackResult{code: code})
	})
}

// runDeviceFlow performs the OAuth 2.0 device authorization flow, for machines
// with no browser at all. Google restricts this flow to the drive.appdata and
// drive.file scopes; the caller must have checked that already.
func runDeviceFlow(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	// PKCE is deliberately omitted here: Google's device endpoint does not
	// document support for it, and the device flow's security model does not
	// rely on a redirect that could be intercepted.
	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "unable to start the OAuth device authorization flow")
	}

	verificationURI := da.VerificationURI
	if da.VerificationURIComplete != "" {
		verificationURI = da.VerificationURIComplete
	}

	fmt.Fprintf(authPromptWriter, //nolint:errcheck
		"\nKopia needs your permission to use Google Drive.\n\nOn any device with a browser, open:\n\n    %v\n\nand enter the code:\n\n    %v\n\nWaiting for you to finish (press Ctrl-C to abort) ...\n\n",
		verificationURI, da.UserCode)

	token, err := cfg.DeviceAccessToken(ctx, da)
	if err != nil {
		return nil, errors.Wrap(err, "the OAuth device authorization did not complete")
	}

	return token, nil
}

// authorizedUserJSON is Google's "authorized user" credentials document. The
// token cache is written in exactly this format, which means the cached file is
// also directly usable as the value of the credentials/credentialsFile options.
type authorizedUserJSON struct {
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`

	// optional endpoint overrides, present in some gcloud-produced documents.
	AuthURI  string `json:"auth_uri,omitempty"`
	TokenURI string `json:"token_uri,omitempty"`
}

// tokenCachePath resolves where the refresh token obtained by an interactive
// flow is kept. The default lives next to the rest of Kopia's configuration and
// is keyed by (OAuth client, folder) so that several repositories, or several
// OAuth clients, never fight over one file.
func tokenCachePath(opt *Options, clientID string) (string, error) {
	if opt.TokenCacheFile != "" {
		return ospath.ResolveUserFriendlyPath(opt.TokenCacheFile, false), nil
	}

	dir := ospath.ConfigDir()
	if dir == "" {
		return "", errors.New("unable to determine the Kopia configuration directory, set the token-cache-file option explicitly")
	}

	h := sha256.Sum256([]byte(clientID + "\x00" + opt.FolderID))

	return filepath.Join(dir, fmt.Sprintf("gdrive-token-%x.json", h[:tokenCacheHashBytes])), nil
}

// loadCachedRefreshToken reads a previously cached authorization. The returned
// token intentionally has no access token, so the first use refreshes it.
func loadCachedRefreshToken(fn, clientID string) (*oauth2.Token, error) {
	data, err := os.ReadFile(fn) //nolint:gosec
	if err != nil {
		//nolint:wrapcheck // the fs.ErrNotExist sentinel must survive for the caller.
		return nil, err
	}

	var au authorizedUserJSON

	if err := json.Unmarshal(data, &au); err != nil {
		return nil, errors.Wrap(err, "the cached token is not valid JSON")
	}

	if au.RefreshToken == "" {
		return nil, errors.New("the cached token has no refresh_token")
	}

	if clientID != "" && au.ClientID != "" && au.ClientID != clientID {
		return nil, errors.New("the cached token was issued to a different OAuth client")
	}

	return &oauth2.Token{RefreshToken: au.RefreshToken}, nil
}

// saveAuthorizedUserToken persists the refresh token with owner-only
// permissions.
func saveAuthorizedUserToken(fn string, cfg *oauth2.Config, token *oauth2.Token) error {
	if token.RefreshToken == "" {
		return errors.New("Google did not return a refresh token. This normally means the account had already authorized this OAuth client, revoke Kopia's access at https://myaccount.google.com/permissions and sign in again")
	}

	//nolint:gosec // G117: persisting the client secret alongside the refresh token is the purpose of this owner-only token cache file.
	data, err := json.MarshalIndent(authorizedUserJSON{
		Type:         credentialTypeAuthorizedUser,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RefreshToken: token.RefreshToken,
	}, "", "  ")
	if err != nil {
		return errors.Wrap(err, "unable to serialize the token")
	}

	if err := os.MkdirAll(filepath.Dir(fn), tokenCacheDirMode); err != nil {
		return errors.Wrap(err, "unable to create the token cache directory")
	}

	return writePrivateFile(fn, data)
}

// writePrivateFile atomically writes data to fn with owner-only permissions.
func writePrivateFile(fn string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(fn), ".gdrive-token-*")
	if err != nil {
		return errors.Wrap(err, "unable to create a temporary file")
	}

	defer os.Remove(f.Name()) //nolint:errcheck

	if err := f.Chmod(tokenCacheFileMode); err != nil {
		f.Close() //nolint:errcheck

		return errors.Wrap(err, "unable to set permissions on the token cache")
	}

	if _, err := f.Write(data); err != nil {
		f.Close() //nolint:errcheck

		return errors.Wrap(err, "unable to write the token cache")
	}

	if err := f.Close(); err != nil {
		return errors.Wrap(err, "unable to close the token cache")
	}

	if err := os.Rename(f.Name(), fn); err != nil {
		return errors.Wrap(err, "unable to rename the token cache into place")
	}

	return nil
}

// randomURLSafeString returns n cryptographically random bytes, base64url-encoded.
func randomURLSafeString(n int) (string, error) {
	b := make([]byte, n)

	if _, err := rand.Read(b); err != nil {
		return "", errors.Wrap(err, "unable to generate random data")
	}

	return base64.RawURLEncoding.EncodeToString(b), nil
}

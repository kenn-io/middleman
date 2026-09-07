package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	appfiles "go.kenn.io/forge/internal/githubapp"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.kenn.io/forge/githubapp"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/githubapp/ui"
)

func runCreate(args []string, env *appEnv) error {
	fs := flag.NewFlagSet("kenn-forge-github-app create", flag.ContinueOnError)
	fs.SetOutput(env.stdout)
	configPath := fs.String("config", env.configPath, "kenn-forge config path")
	host := fs.String("host", "", "GitHub host (default github.com)")
	org := fs.String("org", "", "create the app under this organization instead of your user")
	name := fs.String("name", "", "app name (default kenn-forge-<random>)")
	role := fs.String("role", config.GitHubAppRoleSync, "Forge workload for this App: sync or archive")
	homepage := fs.String("homepage", "", "app homepage URL shown on its public page")
	noBrowser := fs.Bool("no-browser", false, "print URLs instead of opening a browser")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for each browser step")
	registerTestFlags(fs, env)
	if err := fs.Parse(args); err != nil {
		return err
	}
	env.configPath = *configPath
	h, err := normalizeHostFlag(*host)
	if err != nil {
		return err
	}
	appRole := strings.ToLower(strings.TrimSpace(*role))
	if appRole != config.GitHubAppRoleSync && appRole != config.GitHubAppRoleArchive {
		return fmt.Errorf("--role must be %q or %q", config.GitHubAppRoleSync, config.GitHubAppRoleArchive)
	}

	if err := config.EnsureDefault(env.configPath); err != nil {
		return fmt.Errorf("ensuring kenn-forge config exists: %w", err)
	}
	cfg, err := env.loadConfig()
	if err != nil {
		return err
	}
	for _, existing := range cfg.GitHubAppsForHost(h) {
		existingRole := strings.ToLower(strings.TrimSpace(existing.Role))
		if existingRole == "" {
			existingRole = config.GitHubAppRoleSync
		}
		if existingRole != appRole {
			continue
		}
		if *org != "" && strings.EqualFold(existing.Owner, *org) {
			return fmt.Errorf(
				"a github app for host %q and owner %q already exists (app id %d, slug %q); "+
					"use \"install --owner %s\" to add an installation or \"delete --owner %s\" to replace it",
				h, existing.Owner, existing.AppID, existing.Slug, existing.Owner, existing.Owner,
			)
		}
		if *org == "" && strings.EqualFold(existing.OwnerType, "User") {
			return fmt.Errorf(
				"a user-owned github app for host %q already exists (app id %d, slug %q); "+
					"use \"install --owner %s\" to add an installation, \"create --org\" for an org-owned app, "+
					"or \"delete --owner %s\" to replace it",
				h, existing.AppID, existing.Slug, existing.Owner, existing.Owner,
			)
		}
	}

	appName := strings.TrimSpace(*name)
	if appName == "" {
		appName, err = appfiles.RandomAppName()
		if err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The flow server stays up through the install step so the browser
	// can finish loading the setup page's "done" view after GitHub
	// redirects back.
	flow, err := newFlowServer(env.stdout)
	if err != nil {
		return err
	}
	defer flow.Close()

	creds, err := env.runManifestFlow(ctx, flow, manifestFlowOptions{
		host:     h,
		org:      *org,
		name:     appName,
		homepage: *homepage,
		open:     !*noBrowser,
		timeout:  *timeout,
	})
	if err != nil {
		return err
	}

	keyPath, err := writePrivateKey(env.configPath, h, creds.Slug, creds.PEM)
	if err != nil {
		return err
	}
	app := config.GitHubAppConfig{
		Host:           h,
		Role:           appRole,
		AppID:          creds.ID,
		Slug:           creds.Slug,
		Owner:          creds.Owner.Login,
		OwnerType:      creds.Owner.Type,
		PrivateKeyPath: keyPath,
	}
	if err := updateAppInConfig(cfg, env.configPath, app); err != nil {
		return fmt.Errorf("saving app credentials to config: %w", err)
	}
	fmt.Fprintf(env.stdout,
		"Created GitHub App %q (id %d) owned by %s.\n"+
			"Private key: %s\nConfig updated: %s\n",
		creds.Slug, creds.ID, creds.Owner.Login, keyPath, env.configPath,
	)

	// Step two: the app must be installed on the account that owns the
	// synced repos before it can mint tokens.
	if err := env.runInstallFlow(ctx, cfg, app, !*noBrowser, *timeout); err != nil {
		return fmt.Errorf(
			"app created but not installed yet: %w\n"+
				"run \"kenn-forge-github-app install\" to finish", err,
		)
	}
	return nil
}

type manifestFlowOptions struct {
	host     string
	org      string
	name     string
	homepage string
	open     bool
	timeout  time.Duration
}

// flowServer is the loopback HTTP server backing the browser side of
// app creation. It serves the embedded Svelte setup page and manifest
// hand-off contract under an unguessable setup path, and receives
// GitHub's post-creation redirect. It outlives the manifest exchange so
// the browser can still load the page's assets for the "done" view
// while the terminal continues with the install step.
type flowServer struct {
	localBase    string
	setupPath    string
	callbackPath string
	state        string
	listener     net.Listener
	srv          *http.Server
	codeCh       chan string
	errCh        chan error

	mu       sync.Mutex
	manifest string
	action   string
	appName  string
	host     string
	consumed bool
}

// missingUIPage is served when the binary was built without the
// embedded Svelte setup page (plain `go build`). The flow cannot
// continue in the browser, so say that instead of dead-ending.
const missingUIPage = `<!DOCTYPE html><html><head><title>kenn-forge-github-app</title></head><body>
<h1>Setup page not included in this build</h1>
<p>This kenn-forge-github-app binary was built without the embedded setup UI,
so the GitHub App creation flow cannot continue in the browser.</p>
<p>Rebuild with <code>make build</code> (which builds and embeds the page),
then re-run <code>kenn-forge-github-app create</code>.</p>
</body></html>`

// flowJSON is the contract between the Go flow server and the Svelte
// setup page: the page POSTs manifest to action exactly as a plain
// HTML form would.
type flowJSON struct {
	Action   string `json:"action"`
	Manifest string `json:"manifest"`
	Name     string `json:"name"`
	Host     string `json:"host"`
}

func newFlowServer(stdout io.Writer) (*flowServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("starting local callback server: %w", err)
	}
	state, err := randomToken()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	setupToken, err := randomToken()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	assets, err := ui.Assets()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("loading embedded setup page: %w", err)
	}
	hasBuiltApp := ui.HasBuiltApp()
	if !hasBuiltApp {
		fmt.Fprintln(stdout,
			"warning: this binary was built without the setup page (plain go build); "+
				"the browser shows instructions instead. Build with `make build`.")
	}

	fs := &flowServer{
		localBase: "http://" + listener.Addr().String(),
		setupPath: "/setup/" + setupToken + "/",
		// The callback path is itself unguessable, and the handler also
		// requires GitHub to echo the expected state.
		callbackPath: "/callback/" + state,
		state:        state,
		listener:     listener,
		codeCh:       make(chan string, 1),
		errCh:        make(chan error, 1),
	}

	mux := http.NewServeMux()
	if hasBuiltApp {
		mux.Handle("GET "+fs.setupPath, http.StripPrefix(fs.setupPath, http.FileServerFS(assets)))
	} else {
		// Without the embedded page the flow is a dead end in the
		// browser; explain that instead of serving a stub directory.
		mux.HandleFunc("GET "+fs.setupPath, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, missingUIPage)
		})
	}
	mux.HandleFunc("GET "+fs.flowJSONPath(), fs.handleFlowJSON)
	mux.HandleFunc("GET "+fs.callbackPath, fs.handleCallback)
	fs.srv = &http.Server{Handler: mux}
	go func() {
		if err := fs.srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case fs.errCh <- err:
			default:
			}
		}
	}()
	return fs, nil
}

func (f *flowServer) setupURL() string {
	return f.localBase + f.setupPath
}

func (f *flowServer) flowJSONPath() string {
	return f.setupPath + "flow.json"
}

func (f *flowServer) Close() {
	_ = f.srv.Close()
	_ = f.listener.Close()
}

func (f *flowServer) setFlow(action, manifest, appName, host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.action = action
	f.manifest = manifest
	f.appName = appName
	f.host = host
}

func (f *flowServer) handleFlowJSON(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	flow := flowJSON{
		Action:   f.action,
		Manifest: f.manifest,
		Name:     f.appName,
		Host:     f.host,
	}
	consumed := f.consumed
	f.mu.Unlock()
	// Once GitHub has redirected back, re-serving the manifest would
	// let a refreshed create tab auto-submit again and register a
	// second app that nothing records. The done view's assets keep
	// being served; only the hand-off contract dies.
	if consumed || flow.Action == "" {
		http.Error(w, "no app creation in progress", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(flow)
}

func (f *flowServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	gotState := r.URL.Query().Get("state")
	if gotState == "" || subtle.ConstantTimeCompare([]byte(gotState), []byte(f.state)) != 1 {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.consumed = true
	f.mu.Unlock()
	select {
	case f.codeCh <- code:
	default:
	}
	// Hand the browser to the setup page's success view; the code
	// exchange continues in the terminal.
	http.Redirect(w, r, f.setupURL()+"?step=done", http.StatusFound)
}

// runManifestFlow points the user's browser at the setup page, which
// submits the prepared manifest to GitHub, then exchanges the code
// GitHub redirects back with for the new app's credentials.
func (env *appEnv) runManifestFlow(
	ctx context.Context, flow *flowServer, opts manifestFlowOptions,
) (*githubapp.AppCredentials, error) {
	manifest, err := appfiles.NewManifest(
		opts.name, opts.homepage, flow.localBase+flow.callbackPath,
	)
	if err != nil {
		return nil, err
	}
	manifestJSON, err := manifest.JSON()
	if err != nil {
		return nil, err
	}
	action := env.webBaseFor(opts.host) + "/settings/apps/new?state=" + flow.state
	if opts.org != "" {
		action = env.webBaseFor(opts.host) +
			"/organizations/" + opts.org + "/settings/apps/new?state=" + flow.state
	}
	flow.setFlow(action, manifestJSON, opts.name, opts.host)

	fmt.Fprintf(env.stdout,
		"Open this page to create the app (it hands a prepared manifest to GitHub):\n  %s\n",
		flow.setupURL(),
	)
	if opts.open {
		if err := env.openBrowser(flow.setupURL()); err != nil {
			fmt.Fprintf(env.stdout, "could not open browser: %v\n", err)
		}
	}
	fmt.Fprintln(env.stdout, "Waiting for GitHub to redirect back after you click Create...")

	var code string
	select {
	case code = <-flow.codeCh:
	case err := <-flow.errCh:
		return nil, fmt.Errorf("local callback server failed: %w", err)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(opts.timeout):
		return nil, fmt.Errorf("timed out after %s waiting for app creation", opts.timeout)
	}

	creds, err := env.apiClient(opts.host).ConvertManifest(ctx, code)
	if err != nil {
		return nil, err
	}
	return creds, nil
}

// runInstallFlow opens the app's install page and waits until an
// installation appears, then records it in the config.
func (env *appEnv) runInstallFlow(
	ctx context.Context,
	cfg *config.Config,
	app config.GitHubAppConfig,
	open bool,
	timeout time.Duration,
) error {
	client := env.apiClient(app.Host)
	var err error
	app, err = env.refreshAppMetadata(ctx, client, app)
	if err != nil {
		return err
	}
	// A recorded installation makes "install" a refresh: re-verify the
	// existing installation and re-record its repository selection
	// (config validation points users here when the recorded snapshot
	// goes stale). Only when the recorded installation disappeared on
	// GitHub does the flow fall through to waiting for a new one.
	var picked githubapp.Installation
	refreshed := false
	if app.InstallationID != 0 {
		jwt, err := appJWT(app, env.now())
		if err != nil {
			return err
		}
		installs, err := client.ListInstallations(ctx, jwt)
		if err != nil {
			return err
		}
		for _, install := range installs {
			if install.ID == app.InstallationID {
				picked = install
				refreshed = true
				break
			}
		}
		if refreshed {
			fmt.Fprintf(env.stdout,
				"Refreshing recorded installation %d on %s.\n",
				picked.ID, picked.Account.Login,
			)
		} else if picked.ID == 0 {
			fmt.Fprintf(env.stdout,
				"Recorded installation %d no longer exists on GitHub; waiting for a new one.\n",
				app.InstallationID,
			)
		}
	}
	if !refreshed {
		url := installURL(env.webBaseFor(app.Host), app)
		fmt.Fprintf(env.stdout,
			"Install the app on the account that owns your synced repos:\n  %s\n", url,
		)
		known := make(map[int64]struct{})
		if app.InstallationID != 0 {
			known[app.InstallationID] = struct{}{}
		}
		if open {
			jwt, err := appJWT(app, env.now())
			if err != nil {
				return err
			}
			installs, err := client.ListInstallations(ctx, jwt)
			if err != nil {
				return err
			}
			for _, install := range installs {
				known[install.ID] = struct{}{}
			}
		}
		if open {
			if err := env.openBrowser(url); err != nil {
				fmt.Fprintf(env.stdout, "could not open browser: %v\n", err)
			}
		}
		fmt.Fprintln(env.stdout, "Waiting for the installation to appear...")
		err := env.pollUntil(ctx, timeout, func(ctx context.Context) (bool, error) {
			jwt, err := appJWT(app, env.now())
			if err != nil {
				return false, err
			}
			installs, err := client.ListInstallations(ctx, jwt)
			if err != nil {
				return false, err
			}
			for _, install := range installs {
				if _, ok := known[install.ID]; ok {
					continue
				}
				picked = install
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			// Editing an installation's repository access or re-running
			// after a coverage failure reconfigures the existing
			// installation instead of minting a new ID, so no new
			// installation ever appears and the poll times out -- the
			// dead-end the coverage error's own "re-run install" guidance
			// would otherwise hit. When exactly one installation exists
			// for this app the intent is unambiguous, so adopt it. Only a
			// clean deadline qualifies: a probe error (transient API
			// failure) or a user interrupt must surface, not silently
			// adopt a stale installation.
			if !errors.Is(err, errPollDeadline) {
				return err
			}
			adopted, adoptErr := env.adoptSoleInstallation(ctx, cfg, app, &picked)
			if adoptErr != nil {
				return adoptErr
			}
			if !adopted {
				return err
			}
			fmt.Fprintf(env.stdout,
				"No new installation appeared; recording the existing installation %d on %s.\n",
				picked.ID, picked.Account.Login,
			)
		}
	}

	app.InstallationID = picked.ID
	app.InstallationAccount = picked.Account.Login
	// Account ownership is not enough for an "Only select repositories"
	// install: the token reaches only the chosen repos. Record the
	// selection and reachable repo list so credential routing can use
	// the App for covered repositories and PAT credentials elsewhere.
	app.RepositorySelection = strings.ToLower(picked.RepositorySelection)
	app.SelectedRepos = nil
	if !strings.EqualFold(picked.RepositorySelection, "all") {
		reachable, err := env.verifySelectedInstallationCoverage(ctx, cfg, app, picked)
		if err != nil {
			return err
		}
		app.SelectedRepos = reachable
	}
	if err := updateAppInConfig(cfg, env.configPath, app); err != nil {
		return fmt.Errorf("saving installation to config: %w", err)
	}
	fmt.Fprintf(env.stdout,
		"Installed on %s (installation %d). kenn-forge will now sync %s repos on %s with app tokens.\n",
		picked.Account.Login, picked.ID, picked.Account.Login, app.Host,
	)
	return nil
}

// adoptSoleInstallation recovers the install flow after the poll reached
// its deadline without a new installation appearing. The caller gates
// this on errPollDeadline, so probe errors and user interrupts never
// reach it and are surfaced instead. Re-running "install" after a
// coverage failure or a restored config reconfigures the existing
// installation rather than minting a new id, so the wait never
// completes; when the app has exactly one GitHub-side installation that
// belongs to an account this config actually intends the app to serve,
// the target is unambiguous, so adopt it into picked and report true.
//
// Adoption is bounded by intent: the sole installation's account must be
// the recorded installation account or own a configured repo that
// resolves to the app. A lone installation on an unrelated account is
// not what the user is waiting for, so it keeps the timeout rather than
// recording the wrong account while reporting success. Multiple
// installations stay ambiguous and also keep the timeout.
func (env *appEnv) adoptSoleInstallation(
	ctx context.Context,
	cfg *config.Config,
	app config.GitHubAppConfig,
	picked *githubapp.Installation,
) (bool, error) {
	jwt, err := appJWT(app, env.now())
	if err != nil {
		return false, err
	}
	installs, err := env.apiClient(app.Host).ListInstallations(ctx, jwt)
	if err != nil {
		return false, err
	}
	if len(installs) != 1 {
		return false, nil
	}
	inst := installs[0]
	account := inst.Account.Login
	if !strings.EqualFold(account, app.InstallationAccount) &&
		!accountServesConfiguredRepos(cfg, app.Host, account) {
		return false, nil
	}
	*picked = inst
	return true, nil
}

// accountServesConfiguredRepos reports whether account owns at least one
// configured github repo on host that resolves to the app token (no
// per-repo credential override). It marks an account the app is actually
// meant to serve, so install recovery adopts a sole existing
// installation only for an intended account instead of any account that
// happens to be the app's only installation.
func accountServesConfiguredRepos(cfg *config.Config, host, account string) bool {
	for _, r := range cfg.Repos {
		if r.PlatformOrDefault() != "github" || r.PlatformHostOrDefault() != host {
			continue
		}
		if r.TokenEnv != "" || r.TokenFile != "" {
			continue
		}
		if strings.EqualFold(r.Owner, account) {
			return true
		}
	}
	return false
}

func (env *appEnv) refreshAppMetadata(
	ctx context.Context,
	client *githubapp.Client,
	app config.GitHubAppConfig,
) (config.GitHubAppConfig, error) {
	jwt, err := appJWT(app, env.now())
	if err != nil {
		return app, err
	}
	live, err := client.GetApp(ctx, jwt)
	if err != nil {
		return app, fmt.Errorf("refreshing GitHub App metadata: %w", err)
	}
	app.Slug = live.Slug
	app.Owner = live.Owner.Login
	app.OwnerType = live.Owner.Type
	return app, nil
}

// verifySelectedInstallationCoverage lists what an "Only select
// repositories" installation token can reach. Sync Apps must cover every
// configured repository for their account; archive Apps record their
// reachable set and use normal routes for repositories outside it. Glob repo
// patterns are therefore rejected only for sync Apps because they expand to
// an open-ended set.
func (env *appEnv) verifySelectedInstallationCoverage(
	ctx context.Context,
	cfg *config.Config,
	app config.GitHubAppConfig,
	picked githubapp.Installation,
) ([]string, error) {
	client := env.apiClient(app.Host)
	jwt, err := appJWT(app, env.now())
	if err != nil {
		return nil, err
	}
	token, err := client.CreateInstallationToken(ctx, jwt, picked.ID)
	if err != nil {
		return nil, fmt.Errorf("verifying selected-repository installation: %w", err)
	}
	names, err := client.ListInstallationRepositories(ctx, token.Token)
	if err != nil {
		return nil, fmt.Errorf("verifying selected-repository installation: %w", err)
	}
	missing := missingSelectedRepos(cfg, app.Host, picked.Account.Login, names)
	if app.Role != config.GitHubAppRoleArchive && len(missing) > 0 {
		return nil, fmt.Errorf(
			"the app was installed on %q with \"Only select repositories\", but the "+
				"installation cannot reach %s; not recording it in config. Edit the "+
				"installation's repository access on GitHub (or choose \"All "+
				"repositories\") and re-run \"install\"",
			picked.Account.Login, strings.Join(missing, ", "),
		)
	}
	return names, nil
}

// validAppSlug matches GitHub's app slug shape (letters, digits,
// hyphens). The slug arrives from the manifest conversion response
// and is used as a filename, so anything else — path separators,
// dots, traversal — is rejected rather than written.
var validAppSlug = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)

// validKeyFileHost limits the host's contribution to the key
// filename to hostname-shaped characters. Hosts are normalized by
// config loading, but the filename must stay safe even if that
// changes.
var validKeyFileHost = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?$`)

// writePrivateKey stores the app's PEM next to the config file with
// owner-only permissions and returns its absolute path. The path is
// absolute so later config loads do not re-resolve it against the
// config directory (a relative --config like tmp/config.toml would
// otherwise turn "tmp/x.pem" into "tmp/tmp/x.pem"). The filename
// carries the host because slugs are only unique per host: two apps
// with the same slug on different hosts must not share a key file.
// The slug is untrusted input from the manifest conversion response;
// a malicious GHES host must not be able to steer the write outside
// the config directory.
func writePrivateKey(configPath, host, slug, pem string) (string, error) {
	if !validAppSlug.MatchString(slug) {
		return "", fmt.Errorf(
			"refusing to use app slug %q as a filename: slugs contain only letters, digits, and hyphens",
			slug,
		)
	}
	if !validKeyFileHost.MatchString(host) {
		return "", fmt.Errorf("refusing to use host %q in a key filename", host)
	}
	dir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return "", fmt.Errorf("resolving config directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating config directory: %w", err)
	}
	name := "github-app-" + strings.ReplaceAll(host, ":", "_") + "-" + slug + ".pem"
	path := filepath.Join(dir, name)
	if filepath.Dir(path) != dir {
		return "", fmt.Errorf("app key path %q escapes the config directory", path)
	}
	// Exclusive create: a pre-existing file at this path is another
	// app's key (or stale state) that must not be silently replaced,
	// and O_EXCL also refuses to write through a planted symlink.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf(
				"app private key %s already exists; refusing to overwrite it (remove the file first if it is stale)",
				path,
			)
		}
		return "", fmt.Errorf("writing app private key: %w", err)
	}
	if _, err := f.WriteString(pem); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("writing app private key: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("writing app private key: %w", err)
	}
	return path, nil
}

func randomToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating state token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// registerTestFlags exposes the endpoint overrides the e2e tests use
// to point both browser-facing and API URLs at a fake GitHub.
func registerTestFlags(fs *flag.FlagSet, env *appEnv) {
	fs.StringVar(&env.apiBase, "api-base", env.apiBase,
		"override the GitHub REST API base URL (testing)")
	fs.StringVar(&env.webBase, "web-base", env.webBase,
		"override the GitHub web base URL (testing)")
}

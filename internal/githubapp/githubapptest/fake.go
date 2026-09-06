// Package githubapptest provides a fake GitHub server covering the
// App Manifest flow and app-scoped REST endpoints. The fake verifies
// app JWT signatures against the private key it issued, so consumer
// tests exercise real signing rather than a stubbed auth check.
package githubapptest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type App struct {
	ID            int64
	Slug          string
	Name          string
	Key           *rsa.PrivateKey
	PEM           string
	Owner         string
	OwnerType     string
	Installations []Installation
	Deleted       bool
}

type appOwner struct {
	Login string
	Type  string
}

type Installation struct {
	ID      int64
	Account string
	// RepositorySelection mirrors GitHub's field: "all" (default) or
	// "selected". Repos lists accessible full names for "selected".
	RepositorySelection string
	Repos               []string
}

type mintedToken struct {
	appID      int64
	installID  int64
	expiresAt  time.Time
	rateLimit  int
	rateUsed   int
	rateReset  int64
	identifier string
}

// Fake is an in-process GitHub stand-in. URL() serves both the web
// surface (manifest form POST target) and the REST API.
type Fake struct {
	mu            sync.Mutex
	srv           *httptest.Server
	apps          map[int64]*App
	pendingCodes  map[string]string // conversion code -> manifest JSON
	pendingOwners map[string]appOwner
	tokens        map[string]*mintedToken
	nextID        int64
	rateLimit     int
	manifests     []string // every manifest JSON received, in order
	// failListInstallations, when > 0, makes that many upcoming
	// /app/installations requests respond 500 before serving normally,
	// so tests can exercise transient install-poll probe failures.
	failListInstallations int
}

func NewFake() *Fake {
	f := &Fake{
		apps:          make(map[int64]*App),
		pendingCodes:  make(map[string]string),
		pendingOwners: make(map[string]appOwner),
		tokens:        make(map[string]*mintedToken),
		nextID:        100,
		rateLimit:     5000,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /settings/apps/new", f.handleManifestSubmit)
	mux.HandleFunc("POST /organizations/{org}/settings/apps/new", f.handleManifestSubmit)
	mux.HandleFunc("POST /api/v3/app-manifests/{code}/conversions", f.handleConversion)
	mux.HandleFunc("GET /api/v3/app", f.handleGetApp)
	mux.HandleFunc("GET /api/v3/app/installations", f.handleListInstallations)
	mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", f.handleCreateToken)
	mux.HandleFunc("DELETE /api/v3/app/installations/{id}", f.handleDeleteInstallation)
	mux.HandleFunc("GET /api/v3/installation/repositories", f.handleInstallationRepos)
	mux.HandleFunc("GET /api/v3/rate_limit", f.handleRateLimit)
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *Fake) Close() { f.srv.Close() }

// URL is the fake's base; use it as both web base and host source.
func (f *Fake) URL() string { return f.srv.URL }

// APIBase is the REST API root, shaped like a GHES /api/v3 mount.
func (f *Fake) APIBase() string { return f.srv.URL + "/api/v3" }

// Manifests returns every manifest JSON the web surface received.
func (f *Fake) Manifests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.manifests...)
}

// FailNextListInstallations makes the next n list-installations requests
// respond 500 before subsequent ones serve normally, letting tests model
// a transient probe failure during the install poll.
func (f *Fake) FailNextListInstallations(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failListInstallations = n
}

// Install simulates the user completing the browser install flow for
// the app, returning the new installation id.
func (f *Fake) Install(appID int64, account string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	app, ok := f.apps[appID]
	if !ok {
		return 0, fmt.Errorf("no app %d", appID)
	}
	f.nextID++
	app.Installations = append(app.Installations, Installation{
		ID: f.nextID, Account: account, RepositorySelection: "all",
	})
	return f.nextID, nil
}

// InstallSelected simulates the user installing the app with "Only
// select repositories", granting access to exactly repos (full
// "owner/name" names).
func (f *Fake) InstallSelected(appID int64, account string, repos ...string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	app, ok := f.apps[appID]
	if !ok {
		return 0, fmt.Errorf("no app %d", appID)
	}
	f.nextID++
	app.Installations = append(app.Installations, Installation{
		ID:                  f.nextID,
		Account:             account,
		RepositorySelection: "selected",
		Repos:               append([]string(nil), repos...),
	})
	return f.nextID, nil
}

// DeleteApp simulates the user deleting the app in browser settings.
func (f *Fake) DeleteApp(appID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	app, ok := f.apps[appID]
	if !ok {
		return fmt.Errorf("no app %d", appID)
	}
	app.Deleted = true
	return nil
}

// AppBySlug returns a snapshot of a registered app by slug. Tests
// scripting the browser use it to map an install URL back to the app.
func (f *Fake) AppBySlug(slug string) (App, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, app := range f.apps {
		if app.Slug == slug {
			snapshot := *app
			snapshot.Installations = append([]Installation(nil), app.Installations...)
			return snapshot, true
		}
	}
	return App{}, false
}

// RenameApp simulates a user renaming the GitHub App out of band.
func (f *Fake) RenameApp(appID int64, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	app, ok := f.apps[appID]
	if !ok {
		return fmt.Errorf("no app %d", appID)
	}
	app.Slug = slug
	return nil
}

// SetRateRemaining configures the core rate numbers reported for
// installation tokens minted after the call.
func (f *Fake) SetRateRemaining(limit, used int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateLimit = limit
	for _, tok := range f.tokens {
		tok.rateLimit = limit
		tok.rateUsed = used
	}
}

func (f *Fake) handleManifestSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	manifest := r.PostFormValue("manifest")
	if manifest == "" {
		http.Error(w, "missing manifest", http.StatusBadRequest)
		return
	}
	var parsed struct {
		URL            string `json:"url"`
		HookAttributes struct {
			URL string `json:"url"`
		} `json:"hook_attributes"`
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.Unmarshal([]byte(manifest), &parsed); err != nil || parsed.RedirectURL == "" {
		http.Error(w, "manifest missing redirect_url", http.StatusBadRequest)
		return
	}
	if parsed.URL == "" {
		http.Error(w, "manifest missing url", http.StatusBadRequest)
		return
	}
	if parsed.HookAttributes.URL == "" {
		http.Error(w, "manifest missing hook_attributes.url", http.StatusBadRequest)
		return
	}
	owner := "fake-owner"
	ownerType := "User"
	if org := r.PathValue("org"); org != "" {
		owner = org
		ownerType = "Organization"
	}
	code := randomHex(16)
	f.mu.Lock()
	f.pendingCodes[code] = manifest
	f.pendingOwners[code] = appOwner{Login: owner, Type: ownerType}
	f.manifests = append(f.manifests, manifest)
	f.mu.Unlock()
	redirect, err := url.Parse(parsed.RedirectURL)
	if err != nil {
		http.Error(w, "bad redirect_url", http.StatusBadRequest)
		return
	}
	q := redirect.Query()
	q.Set("code", code)
	if state := r.URL.Query().Get("state"); state != "" {
		q.Set("state", state)
	}
	redirect.RawQuery = q.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (f *Fake) handleConversion(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	f.mu.Lock()
	manifest, ok := f.pendingCodes[code]
	owner := f.pendingOwners[code]
	if ok {
		delete(f.pendingCodes, code)
		delete(f.pendingOwners, code)
	}
	f.mu.Unlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, "unknown or used conversion code")
		return
	}
	if owner.Login == "" {
		owner = appOwner{Login: "fake-owner", Type: "User"}
	}
	var parsed struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(manifest), &parsed); err != nil || parsed.Name == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "manifest missing name")
		return
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "keygen failed")
		return
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	f.mu.Lock()
	f.nextID++
	app := &App{
		ID:        f.nextID,
		Slug:      strings.ToLower(parsed.Name),
		Name:      parsed.Name,
		Key:       key,
		PEM:       string(pemBytes),
		Owner:     owner.Login,
		OwnerType: owner.Type,
	}
	f.apps[app.ID] = app
	f.mu.Unlock()
	webhookSecret := randomHex(8)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":             app.ID,
		"slug":           app.Slug,
		"name":           app.Name,
		"client_id":      "Iv1." + randomHex(8),
		"client_secret":  randomHex(20),
		"webhook_secret": webhookSecret,
		"pem":            app.PEM,
		"html_url":       f.srv.URL + "/apps/" + app.Slug,
		"owner": map[string]any{
			"login": app.Owner, "type": app.OwnerType,
		},
	})
}

func (f *Fake) handleGetApp(w http.ResponseWriter, r *http.Request) {
	app, ok := f.authenticateAppJWT(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "bad app JWT")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       app.ID,
		"slug":     app.Slug,
		"name":     app.Name,
		"html_url": f.srv.URL + "/apps/" + app.Slug,
		"owner":    map[string]any{"login": app.Owner, "type": app.OwnerType},
	})
}

func (f *Fake) handleListInstallations(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.failListInstallations > 0 {
		f.failListInstallations--
		f.mu.Unlock()
		writeJSONError(w, http.StatusInternalServerError, "transient listing failure")
		return
	}
	f.mu.Unlock()
	app, ok := f.authenticateAppJWT(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "bad app JWT")
		return
	}
	out := make([]map[string]any, 0, len(app.Installations))
	for _, inst := range app.Installations {
		selection := inst.RepositorySelection
		if selection == "" {
			selection = "all"
		}
		out = append(out, map[string]any{
			"id": inst.ID,
			"account": map[string]any{
				"login": inst.Account, "type": "Organization",
			},
			"repository_selection": selection,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	app, ok := f.authenticateAppJWT(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "bad app JWT")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad installation id")
		return
	}
	found := false
	for _, inst := range app.Installations {
		if inst.ID == id {
			found = true
			break
		}
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "installation not found")
		return
	}
	token := "ghs_" + randomHex(16)
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	f.mu.Lock()
	f.tokens[token] = &mintedToken{
		appID: app.ID, installID: id, expiresAt: expires,
		rateLimit: f.rateLimit, rateReset: time.Now().Add(time.Hour).Unix(),
		identifier: token,
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":      token,
		"expires_at": expires.Format(time.RFC3339),
	})
}

func (f *Fake) handleDeleteInstallation(w http.ResponseWriter, r *http.Request) {
	app, ok := f.authenticateAppJWT(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "bad app JWT")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad installation id")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	live, ok := f.apps[app.ID]
	if !ok {
		writeJSONError(w, http.StatusNotFound, "app not found")
		return
	}
	for i, inst := range live.Installations {
		if inst.ID == id {
			live.Installations = append(
				live.Installations[:i], live.Installations[i+1:]...,
			)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	writeJSONError(w, http.StatusNotFound, "installation not found")
}

// handleInstallationRepos answers GET /installation/repositories for
// a minted installation token, returning the repos that token's
// installation can reach.
func (f *Fake) handleInstallationRepos(w http.ResponseWriter, r *http.Request) {
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	tok, ok := f.tokens[auth]
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unknown token")
		return
	}
	app, ok := f.apps[tok.appID]
	if !ok {
		writeJSONError(w, http.StatusNotFound, "app not found")
		return
	}
	for _, inst := range app.Installations {
		if inst.ID != tok.installID {
			continue
		}
		repos := make([]map[string]any, 0, len(inst.Repos))
		for _, name := range inst.Repos {
			repos = append(repos, map[string]any{"full_name": name})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"total_count":  len(repos),
			"repositories": repos,
		})
		return
	}
	writeJSONError(w, http.StatusNotFound, "installation not found")
}

func (f *Fake) handleRateLimit(w http.ResponseWriter, r *http.Request) {
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	tok, ok := f.tokens[auth]
	f.mu.Unlock()
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unknown token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resources": map[string]any{
			"core": map[string]any{
				"limit":     tok.rateLimit,
				"remaining": tok.rateLimit - tok.rateUsed,
				"reset":     tok.rateReset,
			},
		},
	})
}

// authenticateAppJWT verifies the bearer JWT's RS256 signature against
// the issuing app's key and returns a snapshot of that app. Deleted
// apps fail auth, matching GitHub's behavior after app deletion.
func (f *Fake) authenticateAppJWT(r *http.Request) (App, bool) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return App{}, false
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return App{}, false
	}
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return App{}, false
	}
	appID, err := strconv.ParseInt(claims.Iss, 10, 64)
	if err != nil {
		return App{}, false
	}
	f.mu.Lock()
	app, ok := f.apps[appID]
	var snapshot App
	if ok {
		snapshot = *app
		snapshot.Installations = append([]Installation(nil), app.Installations...)
	}
	f.mu.Unlock()
	if !ok || snapshot.Deleted {
		return App{}, false
	}
	if claims.Exp < time.Now().Unix() {
		return App{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return App{}, false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&snapshot.Key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		return App{}, false
	}
	return snapshot, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

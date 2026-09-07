// Package kata owns the Kata daemon HTTP boundary.
package kata

import (
	"net/http"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	katacatalog "go.kenn.io/forge/internal/kata"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/server/workspaceapi"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/platform"
)

// Deps contains the committed state and shared services consumed by Kata.
// Defaults preserve the normal local Kata discovery behavior while allowing
// tests and alternate composition roots to supply owned transports/catalogs.
type Deps struct {
	DB           *db.DB
	Resolver     *httpapi.RepositoryResolver
	Config       ConfigSnapshot
	Workspaces   *workspace.Manager
	WorkspaceAPI *workspaceapi.Handler
	LoadCatalog  func() (katacatalog.Catalog, error)
	// OnCatalogTokenEnvNames receives the catalog's daemon token_env
	// names after every successful catalog load so credential stripping
	// covers externally cataloged tokens, including catalog edits.
	OnCatalogTokenEnvNames func([]string)
	ResolveDaemon          func(katacatalog.Daemon) (katacatalog.Daemon, error)
	DiscoverLocalDaemonURL func() string
	NewHTTPTransport       func() http.RoundTripper
	SamePlatformHost       func(string, string) bool
	ConfigRepoPath         func(config.Repo) string
}

// Handler owns Kata routes, daemon caches, and their lifecycle.
type Handler struct {
	db           *db.DB
	resolver     *httpapi.RepositoryResolver
	workspaces   *workspace.Manager
	workspaceAPI *workspaceapi.Handler

	configMu sync.RWMutex
	config   ConfigSnapshot

	loadCatalog            func() (katacatalog.Catalog, error)
	resolveDaemon          func(katacatalog.Daemon) (katacatalog.Daemon, error)
	discoverLocalDaemonURL func() string
	newHTTPTransport       func() http.RoundTripper
	samePlatformHost       func(string, string) bool
	configRepoPath         func(config.Repo) string

	kataHealthMu       sync.Mutex
	kataHealthCache    map[string]kataDaemonHealthCacheEntry
	kataHealthInFlight map[string]*kataDaemonInflightProbe

	kataLinkHydrationMu          sync.Mutex
	kataLinkHydrationGlobalSlots chan struct{}
	kataLinkHydrationDaemonSlots map[string]chan struct{}

	kataWorkspaceCreateMu sync.Mutex
}

// New constructs the Kata API handler from immutable state and explicit
// shared-domain dependencies.
func New(deps Deps) *Handler {
	loadCatalog := deps.LoadCatalog
	if loadCatalog == nil {
		loadCatalog = katacatalog.LoadCatalog
	}
	if deps.OnCatalogTokenEnvNames != nil {
		inner := loadCatalog
		onNames := deps.OnCatalogTokenEnvNames
		loadCatalog = func() (katacatalog.Catalog, error) {
			cat, err := inner()
			// Decoded-but-invalid catalogs still carry declared
			// token_env names; report them so a rejected catalog's
			// credentials keep being stripped from terminals.
			if names := cat.TokenEnvNames(); len(names) > 0 {
				onNames(names)
			}
			return cat, err
		}
	}
	resolveDaemon := deps.ResolveDaemon
	if resolveDaemon == nil {
		resolveDaemon = katacatalog.ResolveDaemon
	}
	discoverLocalDaemonURL := deps.DiscoverLocalDaemonURL
	if discoverLocalDaemonURL == nil {
		discoverLocalDaemonURL = katacatalog.DiscoverLocalDaemonURL
	}
	newHTTPTransport := deps.NewHTTPTransport
	if newHTTPTransport == nil {
		newHTTPTransport = newDefaultKataDaemonTransport
	}
	return &Handler{
		db:                     deps.DB,
		resolver:               deps.Resolver,
		config:                 cloneConfigSnapshot(deps.Config),
		workspaces:             deps.Workspaces,
		workspaceAPI:           deps.WorkspaceAPI,
		loadCatalog:            loadCatalog,
		resolveDaemon:          resolveDaemon,
		discoverLocalDaemonURL: discoverLocalDaemonURL,
		newHTTPTransport:       newHTTPTransport,
		samePlatformHost:       deps.SamePlatformHost,
		configRepoPath:         deps.ConfigRepoPath,
		kataLinkHydrationGlobalSlots: make(
			chan struct{}, kataLinkHydrationGlobalConcurrency,
		),
		kataLinkHydrationDaemonSlots: make(map[string]chan struct{}),
	}
}

// Register registers all documented and passthrough Kata routes.
func (h *Handler) Register(api huma.API) {
	huma.Get(api, "/kata/daemons", h.listKataDaemons,
		httpapi.DocumentOperation("list-kata-daemons", "List Kata daemons", "Kata"))
	registerKataLinkAPI(api, h)
	registerKataReadAPI(api, h)
	registerKataWorkspaceAPI(api, h)
}

func (h *Handler) repoRefFromParts(
	provider, host, owner, name string,
) httpapi.RepoRefResponse {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = string(platform.KindGitHub)
	}
	response := httpapi.RepoRefResponse{
		Provider: provider, PlatformHost: host,
		RepoPath: owner + "/" + name, Owner: owner, Name: name,
	}
	if h.resolver != nil {
		response.Capabilities = h.resolver.Capabilities(platform.Kind(provider), host)
	}
	return response
}

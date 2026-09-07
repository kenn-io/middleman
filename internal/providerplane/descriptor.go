// Package providerplane owns the hub-facing provider protocol used by
// spokes. It deliberately does not own spoke-local Git or workspace
// state.
package providerplane

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/forge/internal/federation"
	"go.kenn.io/forge/platform"
	gitremote "go.kenn.io/kit/git/remote"
)

// ProtocolVersionHeader carries the exact federation protocol version on
// provider requests, whose ordinary response bodies have no version field.
const ProtocolVersionHeader = "X-Kenn-Forge-Federation-Protocol"

// ProtocolVersionHeaderValue returns the one accepted wire value.
func ProtocolVersionHeaderValue() string {
	return strconv.Itoa(federation.ProtocolVersion)
}

// Hub identifies the one provider-control-plane destination.
type Hub struct {
	NodeID  string
	BaseURL string
}

// RepositoryRoute identifies a repository by its current provider route.
// Unlike RepositoryIdentity, it is suitable for requests whose caller does
// not yet know the provider's stable repository ID.
type RepositoryRoute struct {
	Provider     string `json:"provider"`
	PlatformHost string `json:"platform_host"`
	Owner        string `json:"owner"`
	Name         string `json:"name"`
}

// CanonicalRepositoryRoute normalizes route spellings accepted by public API
// paths into the exact form carried on the federation wire.
func CanonicalRepositoryRoute(route RepositoryRoute) (RepositoryRoute, error) {
	kind, err := platform.NormalizeKind(route.Provider)
	if err != nil {
		return RepositoryRoute{}, err
	}
	host, ok := platform.HostOrDefault(kind, route.PlatformHost)
	if !ok {
		return RepositoryRoute{}, fmt.Errorf("platform host is required")
	}
	route.Provider = string(kind)
	route.PlatformHost = strings.ToLower(strings.TrimSpace(host))
	route.Owner = strings.TrimSpace(route.Owner)
	route.Name = strings.TrimSpace(route.Name)
	if kind == platform.KindGitHub {
		route.Owner = strings.ToLower(route.Owner)
		route.Name = strings.ToLower(route.Name)
	}
	if err := route.Validate(); err != nil {
		return RepositoryRoute{}, err
	}
	return route, nil
}

// Validate rejects noncanonical or incomplete provider routes.
func (r RepositoryRoute) Validate() error {
	ref := platform.RepoRef{
		Platform: platform.Kind(r.Provider),
		Host:     r.PlatformHost,
		Owner:    r.Owner,
		Name:     r.Name,
		RepoPath: r.Owner + "/" + r.Name,
	}
	if err := platform.ValidateCanonicalRepoRef(ref); err != nil {
		return fmt.Errorf("repository route: %w", err)
	}
	return validateProviderHostPair(ref.Platform, ref.Host)
}

// RepositorySnapshot is the hub-owned input used to construct one
// repository descriptor from a stable database snapshot.
type RepositorySnapshot struct {
	Provider         string
	PlatformHost     string
	PlatformRepoID   string
	Owner            string
	Name             string
	CloneURL         string
	DefaultBranch    string
	SnapshotRevision uint64
	ObservedAt       time.Time
	Stale            bool
}

// RepositoryDescriptor carries only provider-verified facts a spoke needs to
// reconcile a repository and perform Git work locally.
type RepositoryDescriptor struct {
	ProtocolVersion  int       `json:"protocol_version"`
	Provider         string    `json:"provider"`
	PlatformHost     string    `json:"platform_host"`
	PlatformRepoID   string    `json:"platform_repo_id"`
	Owner            string    `json:"owner"`
	Name             string    `json:"name"`
	CloneURL         string    `json:"clone_url"`
	DefaultBranch    string    `json:"default_branch"`
	SnapshotRevision uint64    `json:"snapshot_revision"`
	ObservedAt       time.Time `json:"observed_at"`
	Stale            bool      `json:"stale"`
}

// BuildRepositoryDescriptor constructs and validates the wire value at the
// hub boundary.
func BuildRepositoryDescriptor(snapshot RepositorySnapshot) (RepositoryDescriptor, error) {
	descriptor := RepositoryDescriptor{
		ProtocolVersion:  federation.ProtocolVersion,
		Provider:         snapshot.Provider,
		PlatformHost:     snapshot.PlatformHost,
		PlatformRepoID:   snapshot.PlatformRepoID,
		Owner:            snapshot.Owner,
		Name:             snapshot.Name,
		CloneURL:         snapshot.CloneURL,
		DefaultBranch:    snapshot.DefaultBranch,
		SnapshotRevision: snapshot.SnapshotRevision,
		ObservedAt:       snapshot.ObservedAt.UTC(),
		Stale:            snapshot.Stale,
	}
	if err := descriptor.Validate(); err != nil {
		return RepositoryDescriptor{}, err
	}
	return descriptor, nil
}

// Route returns the descriptor's mutable provider route.
func (d RepositoryDescriptor) Route() RepositoryRoute {
	return RepositoryRoute{
		Provider: d.Provider, PlatformHost: d.PlatformHost,
		Owner: d.Owner, Name: d.Name,
	}
}

// Identity returns the descriptor's stable cross-spoke identity.
func (d RepositoryDescriptor) Identity() RepositoryIdentity {
	return RepositoryIdentity{
		Provider: d.Provider, PlatformHost: d.PlatformHost,
		PlatformRepoID: d.PlatformRepoID,
	}
}

// Validate verifies every trust-bearing descriptor field.
func (d RepositoryDescriptor) Validate() error {
	if d.ProtocolVersion != federation.ProtocolVersion {
		return fmt.Errorf(
			"repository descriptor protocol version %d does not match %d",
			d.ProtocolVersion, federation.ProtocolVersion,
		)
	}
	if err := d.Route().Validate(); err != nil {
		return err
	}
	if !d.Identity().Valid() {
		return fmt.Errorf("repository descriptor stable identity is required")
	}
	if d.Provider != strings.TrimSpace(d.Provider) ||
		d.PlatformHost != strings.TrimSpace(d.PlatformHost) ||
		d.PlatformRepoID != strings.TrimSpace(d.PlatformRepoID) ||
		d.Owner != strings.TrimSpace(d.Owner) ||
		d.Name != strings.TrimSpace(d.Name) {
		return fmt.Errorf("repository descriptor identity must be canonical")
	}
	if strings.TrimSpace(d.CloneURL) == "" || d.CloneURL != strings.TrimSpace(d.CloneURL) {
		return fmt.Errorf("repository descriptor clone URL is required")
	}
	if err := validateFederationNetworkRemote(d.CloneURL); err != nil {
		return fmt.Errorf("repository descriptor clone URL: %w", err)
	}
	if err := gitremote.ValidateRemoteIdentity(gitremote.Identity{
		Host: d.PlatformHost, Owner: d.Owner, Name: d.Name,
	}, d.CloneURL); err != nil {
		return fmt.Errorf("repository descriptor clone URL: %w", err)
	}
	if strings.TrimSpace(d.DefaultBranch) == "" ||
		d.DefaultBranch != strings.TrimSpace(d.DefaultBranch) {
		return fmt.Errorf("repository descriptor default branch is required")
	}
	if d.SnapshotRevision == 0 {
		return fmt.Errorf("repository descriptor snapshot revision is required")
	}
	if d.ObservedAt.IsZero() || d.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("repository descriptor observed time must be UTC")
	}
	return nil
}

// ValidateRoute proves that a descriptor answers the exact requested route.
func (d RepositoryDescriptor) ValidateRoute(route RepositoryRoute) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if err := route.Validate(); err != nil {
		return err
	}
	if d.Route() != route {
		return fmt.Errorf("repository descriptor does not match requested route")
	}
	return nil
}

// DiffSnapshot is the hub-owned input used to construct one pull
// diff descriptor while the repository route and pull snapshot are locked.
type DiffSnapshot struct {
	Repository       RepositorySnapshot
	PullNumber       int
	SnapshotRevision uint64
	PlatformHeadSHA  string
	PlatformBaseSHA  string
	DiffHeadSHA      string
	DiffBaseSHA      string
	MergeBaseSHA     string
	Stale            bool
}

// DiffDescriptor adds the exact pull snapshot needed for spoke-local clone
// reads to a verified repository descriptor.
type DiffDescriptor struct {
	ProtocolVersion  int                  `json:"protocol_version"`
	Repository       RepositoryDescriptor `json:"repository"`
	PullNumber       int                  `json:"pull_number"`
	SnapshotRevision uint64               `json:"snapshot_revision"`
	PlatformHeadSHA  string               `json:"platform_head_sha"`
	PlatformBaseSHA  string               `json:"platform_base_sha"`
	DiffHeadSHA      string               `json:"diff_head_sha"`
	DiffBaseSHA      string               `json:"diff_base_sha"`
	MergeBaseSHA     string               `json:"merge_base_sha"`
	Stale            bool                 `json:"stale"`
}

// BuildDiffDescriptor constructs and validates one internally consistent wire
// value from the hub's locked snapshot.
func BuildDiffDescriptor(snapshot DiffSnapshot) (DiffDescriptor, error) {
	repository, err := BuildRepositoryDescriptor(snapshot.Repository)
	if err != nil {
		return DiffDescriptor{}, err
	}
	descriptor := DiffDescriptor{
		ProtocolVersion: federation.ProtocolVersion,
		Repository:      repository, PullNumber: snapshot.PullNumber,
		SnapshotRevision: snapshot.SnapshotRevision,
		PlatformHeadSHA:  snapshot.PlatformHeadSHA,
		PlatformBaseSHA:  snapshot.PlatformBaseSHA,
		DiffHeadSHA:      snapshot.DiffHeadSHA,
		DiffBaseSHA:      snapshot.DiffBaseSHA,
		MergeBaseSHA:     snapshot.MergeBaseSHA,
		Stale:            snapshot.Stale,
	}
	if err := descriptor.Validate(); err != nil {
		return DiffDescriptor{}, err
	}
	return descriptor, nil
}

// Validate verifies the repository and every SHA from the pull snapshot.
func (d DiffDescriptor) Validate() error {
	if d.ProtocolVersion != federation.ProtocolVersion ||
		d.ProtocolVersion != d.Repository.ProtocolVersion {
		return fmt.Errorf("diff descriptor protocol version mismatch")
	}
	if err := d.Repository.Validate(); err != nil {
		return err
	}
	if d.PullNumber < 1 {
		return fmt.Errorf("diff descriptor pull number is required")
	}
	if d.SnapshotRevision == 0 {
		return fmt.Errorf("diff descriptor snapshot revision is required")
	}
	for name, value := range map[string]string{
		"platform head": d.PlatformHeadSHA,
		"platform base": d.PlatformBaseSHA,
		"diff head":     d.DiffHeadSHA,
		"diff base":     d.DiffBaseSHA,
		"merge base":    d.MergeBaseSHA,
	} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("diff descriptor %s SHA is required", name)
		}
	}
	return nil
}

func validateProviderHostPair(kind platform.Kind, host string) error {
	for _, other := range []platform.Kind{
		platform.KindGitHub, platform.KindGitLab,
		platform.KindForgejo, platform.KindGitea,
	} {
		if other == kind {
			continue
		}
		if defaultHost, ok := platform.DefaultHost(other); ok && host == defaultHost {
			return fmt.Errorf("provider %q does not match platform host %q", kind, host)
		}
	}
	return nil
}

func (c Hub) validate() (Hub, error) {
	c.NodeID = strings.TrimSpace(c.NodeID)
	if !federation.ValidNodeID(c.NodeID) {
		return Hub{}, fmt.Errorf("hub node ID is invalid")
	}
	baseURL, err := federation.CanonicalOrigin(c.BaseURL)
	if err != nil {
		return Hub{}, fmt.Errorf("hub origin: %w", err)
	}
	c.BaseURL = baseURL
	return c, nil
}

// RepositoryIdentity is the stable cross-spoke key for provider repository
// data. Local numeric database IDs are intentionally absent.
type RepositoryIdentity struct {
	Provider       string `json:"provider"`
	PlatformHost   string `json:"platform_host"`
	PlatformRepoID string `json:"platform_repo_id"`
}

// Canonical returns the comparable cross-spoke form of a repository identity.
// Provider repository IDs remain case-sensitive.
func (r RepositoryIdentity) Canonical() RepositoryIdentity {
	r.Provider = strings.ToLower(strings.TrimSpace(r.Provider))
	r.PlatformHost = strings.ToLower(strings.TrimSpace(r.PlatformHost))
	r.PlatformRepoID = strings.TrimSpace(r.PlatformRepoID)
	return r
}

// Valid reports whether the stable cross-spoke identity is complete.
func (r RepositoryIdentity) Valid() bool {
	r = r.Canonical()
	return r.Provider != "" && r.PlatformHost != "" && r.PlatformRepoID != ""
}

// ItemIdentity identifies one provider item without a spoke-local row ID.
type ItemIdentity struct {
	Repository RepositoryIdentity `json:"repository"`
	ItemType   string             `json:"item_type"`
	ItemNumber int                `json:"item_number"`
}

// Canonical returns the comparable cross-spoke form of an item identity.
func (i ItemIdentity) Canonical() ItemIdentity {
	i.Repository = i.Repository.Canonical()
	i.ItemType = strings.ToLower(strings.TrimSpace(i.ItemType))
	return i
}

// Valid reports whether an item can be correlated without a spoke-local row ID.
func (i ItemIdentity) Valid() bool {
	i = i.Canonical()
	return i.Repository.Valid() && i.ItemType != "" && i.ItemNumber > 0
}

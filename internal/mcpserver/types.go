package mcpserver

import (
	"fmt"
	"strings"
	"time"

	"go.kenn.io/forge/platform"
)

type repoFilterInput struct {
	Provider       string `json:"provider,omitempty" jsonschema:"provider kind, such as github or gitlab"`
	PlatformHost   string `json:"platform_host,omitempty" jsonschema:"provider host; defaults to the provider public host"`
	PlatformRepoID string `json:"platform_repo_id,omitempty" jsonschema:"stable provider-verified repository id from kenn_forge_list_repos"`
	RepoPath       string `json:"repo_path,omitempty" jsonschema:"full repository path from kenn_forge_list_repos; preferred for nested namespaces"`
	Owner          string `json:"owner,omitempty" jsonschema:"repository owner or namespace"`
	Name           string `json:"name,omitempty" jsonschema:"repository name"`
}

func (r repoFilterInput) repositoryIdentity() (RepositoryIdentity, error) {
	provider := strings.TrimSpace(r.Provider)
	host := strings.TrimSpace(r.PlatformHost)
	platformRepoID := strings.TrimSpace(r.PlatformRepoID)
	repoPath := strings.Trim(strings.TrimSpace(r.RepoPath), "/")
	owner := strings.Trim(strings.TrimSpace(r.Owner), "/")
	name := strings.Trim(strings.TrimSpace(r.Name), "/")
	if provider == "" && host == "" && platformRepoID == "" && repoPath == "" && owner == "" && name == "" {
		return RepositoryIdentity{}, nil
	}
	if provider == "" {
		return RepositoryIdentity{}, fmt.Errorf("repo provider is required")
	}
	if platformRepoID == "" {
		return RepositoryIdentity{}, fmt.Errorf("repo platform_repo_id is required")
	}
	kind, err := platform.NormalizeKind(provider)
	if err != nil {
		return RepositoryIdentity{}, err
	}
	meta, ok := platform.MetadataFor(kind)
	if !ok {
		return RepositoryIdentity{}, fmt.Errorf("unsupported provider %q", provider)
	}
	if host == "" {
		host = meta.DefaultHost
	}
	if repoPath != "" {
		parts := strings.Split(repoPath, "/")
		if len(parts) < 2 {
			return RepositoryIdentity{}, fmt.Errorf("repo_path must contain an owner and repository name")
		}
		owner = strings.Join(parts[:len(parts)-1], "/")
		name = parts[len(parts)-1]
	} else {
		if owner == "" {
			return RepositoryIdentity{}, fmt.Errorf("repo owner is required")
		}
		if name == "" {
			return RepositoryIdentity{}, fmt.Errorf("repo name is required")
		}
		repoPath = owner + "/" + name
	}
	return RepositoryIdentity{
		Provider: string(kind), PlatformHost: host, PlatformRepoID: platformRepoID,
		RepoPath: repoPath, Owner: owner, Name: name,
	}, nil
}

type itemRef struct {
	Type           string `json:"type"`
	Provider       string `json:"provider"`
	PlatformHost   string `json:"platform_host"`
	PlatformRepoID string `json:"platform_repo_id"`
	Owner          string `json:"owner"`
	Name           string `json:"name"`
	RepoPath       string `json:"repo_path"`
	Number         int    `json:"number"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	State          string `json:"state"`
	Author         string `json:"author"`
	IsDraft        bool   `json:"is_draft"`
}

func repositoryPath(repo RepositoryIdentity) string {
	if repo.RepoPath != "" {
		return repo.RepoPath
	}
	if repo.Owner == "" {
		return repo.Name
	}
	if repo.Name == "" {
		return repo.Owner
	}
	return repo.Owner + "/" + repo.Name
}

func workflowStatusOrNew(status string) string {
	if status == "" {
		return "new"
	}
	return status
}

func formatMCPTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (p Pull) itemRef() itemRef {
	return itemRef{
		Type: "pr", Provider: p.Repository.Provider,
		PlatformHost: p.Repository.PlatformHost, PlatformRepoID: p.Repository.PlatformRepoID,
		Owner: p.Repository.Owner,
		Name:  p.Repository.Name, RepoPath: repositoryPath(p.Repository),
		Number: p.Number, Title: p.Title, URL: p.URL, State: p.State,
		Author: p.Author, IsDraft: p.IsDraft,
	}
}

func (i Issue) itemRef() itemRef {
	return itemRef{
		Type: "issue", Provider: i.Repository.Provider,
		PlatformHost: i.Repository.PlatformHost, PlatformRepoID: i.Repository.PlatformRepoID,
		Owner: i.Repository.Owner,
		Name:  i.Repository.Name, RepoPath: repositoryPath(i.Repository),
		Number: i.Number, Title: i.Title, URL: i.URL, State: i.State,
		Author: i.Author,
	}
}

func (a ActivityItem) itemRef() itemRef {
	return itemRef{
		Type: a.ItemType, Provider: a.Repository.Provider,
		PlatformHost: a.Repository.PlatformHost, PlatformRepoID: a.Repository.PlatformRepoID,
		Owner: a.Repository.Owner,
		Name:  a.Repository.Name, RepoPath: repositoryPath(a.Repository),
		Number: a.ItemNumber, Title: a.ItemTitle, URL: a.ItemURL,
		State: a.ItemState, Author: a.ItemAuthor,
	}
}

func itemIdentity(ref itemRefInput) ItemIdentity {
	return ItemIdentity(ref)
}

func itemIdentityFromRef(ref itemRef) ItemIdentity {
	return ItemIdentity{
		Type: ref.Type, Provider: ref.Provider, PlatformHost: ref.PlatformHost,
		PlatformRepoID: ref.PlatformRepoID,
		Owner:          ref.Owner, Name: ref.Name, Number: ref.Number,
	}
}

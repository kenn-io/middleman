package platform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCanonicalRepoRefAcceptsProviderIdentities(t *testing.T) {
	tests := []struct {
		name string
		ref  RepoRef
	}{
		{
			name: "public host",
			ref:  RepoRef{Platform: KindGitHub, Host: DefaultGitHubHost, Owner: "acme", Name: "widget"},
		},
		{
			name: "self hosted with port",
			ref:  RepoRef{Platform: KindForgejo, Host: "forge.example.com:8443", Owner: "Acme", Name: "Widget"},
		},
		{
			name: "nested GitLab owner",
			ref: RepoRef{
				Platform: KindGitLab,
				Host:     "gitlab.example.com",
				Owner:    "group/subgroup",
				Name:     "project",
				RepoPath: "group/subgroup/project",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NoError(t, ValidateCanonicalRepoRef(tt.ref))
		})
	}
}

func TestValidateCanonicalRepoRefRejectsNoncanonicalIdentities(t *testing.T) {
	valid := RepoRef{Platform: KindGitHub, Host: DefaultGitHubHost, Owner: "acme", Name: "widget"}
	tests := []struct {
		name string
		edit func(*RepoRef)
	}{
		{name: "provider alias", edit: func(ref *RepoRef) { ref.Platform = "gh" }},
		{name: "mixed case provider", edit: func(ref *RepoRef) { ref.Platform = "GitHub" }},
		{name: "unknown provider", edit: func(ref *RepoRef) { ref.Platform = "unknown" }},
		{name: "empty host", edit: func(ref *RepoRef) { ref.Host = "" }},
		{name: "whitespace host", edit: func(ref *RepoRef) { ref.Host = " github.com" }},
		{name: "schemed host", edit: func(ref *RepoRef) { ref.Host = "https://github.com" }},
		{name: "mixed case host", edit: func(ref *RepoRef) { ref.Host = "GitHub.com" }},
		{name: "default TLS port", edit: func(ref *RepoRef) { ref.Host = "github.com:443" }},
		{name: "empty port", edit: func(ref *RepoRef) { ref.Host = "github.com:" }},
		{name: "nonnumeric port", edit: func(ref *RepoRef) { ref.Host = "github.com:bad" }},
		{name: "host path", edit: func(ref *RepoRef) { ref.Host = "github.com/api" }},
		{name: "empty owner", edit: func(ref *RepoRef) { ref.Owner = "" }},
		{name: "whitespace owner", edit: func(ref *RepoRef) { ref.Owner = "acme org" }},
		{name: "nested non GitLab owner", edit: func(ref *RepoRef) { ref.Owner = "acme/team" }},
		{name: "empty name", edit: func(ref *RepoRef) { ref.Name = "" }},
		{name: "whitespace name", edit: func(ref *RepoRef) { ref.Name = " widget" }},
		{name: "nested name", edit: func(ref *RepoRef) { ref.Name = "team/widget" }},
		{name: "inconsistent repo path", edit: func(ref *RepoRef) { ref.RepoPath = "other/widget" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := valid
			tt.edit(&ref)

			err := ValidateCanonicalRepoRef(ref)

			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidRepoRef)
		})
	}
}

func TestCanonicalRepoRefsEqualUsesFullValidatedIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	left := RepoRef{
		Platform: KindGitLab,
		Host:     "gitlab.example.com:8443",
		Owner:    "group/subgroup",
		Name:     "project",
		RepoPath: "group/subgroup/project",
	}
	right := left

	equal, err := CanonicalRepoRefsEqual(left, right)
	require.NoError(err)
	assert.True(equal)

	right.Host = "other.example.com:8443"
	equal, err = CanonicalRepoRefsEqual(left, right)
	require.NoError(err)
	assert.False(equal)

	right = left
	right.RepoPath = "other/project"
	_, err = CanonicalRepoRefsEqual(left, right)
	require.ErrorIs(err, ErrInvalidRepoRef)
}

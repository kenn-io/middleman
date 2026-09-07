package platform

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testProvider struct {
	kind Kind
	host string
	caps Capabilities
}

func (p testProvider) Platform() Kind {
	return p.kind
}

func (p testProvider) Host() string {
	return p.host
}

func (p testProvider) Capabilities() Capabilities {
	return p.caps
}

type testRepositoryReader struct {
	testProvider
}

func (p testRepositoryReader) GetRepository(
	context.Context, RepoRef,
) (Repository, error) {
	return Repository{}, nil
}

func (p testRepositoryReader) ListRepositories(
	context.Context, string, RepositoryListOptions,
) ([]Repository, error) {
	return nil, nil
}

func TestRegistryLooksUpProvidersByKindAndHost(t *testing.T) {
	provider := testProvider{
		kind: KindGitLab,
		host: "gitlab.example.com:8443",
		caps: Capabilities{ReadMergeRequests: true},
	}

	registry, err := NewRegistry(provider)
	require.NoError(t, err)

	got, err := registry.Provider(KindGitLab, "gitlab.example.com:8443")
	require.NoError(t, err)

	caps, err := registry.Capabilities(KindGitLab, "gitlab.example.com:8443")
	require.NoError(t, err)

	assert := assert.New(t)
	assert.Equal(KindGitLab, got.Platform())
	assert.Equal("gitlab.example.com:8443", got.Host())
	assert.True(caps.ReadMergeRequests)
}

func TestRegistryReturnsTypedErrorForMissingProvider(t *testing.T) {
	registry, err := NewRegistry(testProvider{
		kind: KindGitHub,
		host: "github.com",
	})
	require.NoError(t, err)

	_, err = registry.Provider(KindGitLab, "gitlab.com")

	var platformErr *Error
	require.ErrorAs(t, err, &platformErr)
	require.ErrorIs(t, err, ErrProviderNotConfigured)
	assert := assert.New(t)
	assert.Equal(ErrCodeProviderNotConfigured, platformErr.Code)
	assert.Equal(KindGitLab, platformErr.Provider)
	assert.Equal("gitlab.com", platformErr.PlatformHost)
}

func TestRegistryReturnsTypedErrorForMissingProviderCapabilities(t *testing.T) {
	registry, err := NewRegistry(testProvider{
		kind: KindGitHub,
		host: "github.com",
	})
	require.NoError(t, err)

	_, err = registry.Capabilities(KindGitLab, "gitlab.com")

	var platformErr *Error
	require.ErrorAs(t, err, &platformErr)
	require.ErrorIs(t, err, ErrProviderNotConfigured)
	assert := assert.New(t)
	assert.Equal(ErrCodeProviderNotConfigured, platformErr.Code)
	assert.Equal(KindGitLab, platformErr.Provider)
	assert.Equal("gitlab.com", platformErr.PlatformHost)
}

func TestRegistryFindsOptionalRepositoryReader(t *testing.T) {
	require := require.New(t)

	registry, err := NewRegistry(testRepositoryReader{
		kind: KindGitLab,
		host: "gitlab.com",
		caps: Capabilities{ReadRepositories: true},
	})
	require.NoError(err)

	reader, err := registry.RepositoryReader(KindGitLab, "gitlab.com")
	require.NoError(err)

	repo, err := reader.GetRepository(context.Background(), RepoRef{
		Platform: KindGitLab,
		Host:     "gitlab.com",
		RepoPath: "group/project",
	})
	require.NoError(err)
	assert.Equal(t, Repository{}, repo)
}

func TestRegistryReturnsUnsupportedCapabilityForMissingOptionalReader(t *testing.T) {
	registry, err := NewRegistry(testProvider{
		kind: KindGitLab,
		host: "gitlab.com",
		caps: Capabilities{ReadRepositories: false},
	})
	require.NoError(t, err)

	_, err = registry.RepositoryReader(KindGitLab, "gitlab.com")

	var platformErr *Error
	require.ErrorAs(t, err, &platformErr)
	require.ErrorIs(t, err, ErrUnsupportedCapability)
	assert := assert.New(t)
	assert.Equal(ErrCodeUnsupportedCapability, platformErr.Code)
	assert.Equal("read_repositories", platformErr.Capability)
}

func TestRegistryReturnsUnsupportedCapabilityForMissingReviewDraftMutator(t *testing.T) {
	registry, err := NewRegistry(testProvider{
		kind: KindForgejo,
		host: DefaultForgejoHost,
		caps: Capabilities{ReviewDraftMutation: false},
	})
	require.NoError(t, err)

	_, err = registry.DiffReviewDraftMutator(KindForgejo, DefaultForgejoHost)

	var platformErr *Error
	require.ErrorAs(t, err, &platformErr)
	require.ErrorIs(t, err, ErrUnsupportedCapability)
	assert := assert.New(t)
	assert.Equal(ErrCodeUnsupportedCapability, platformErr.Code)
	assert.Equal("review_draft_mutation", platformErr.Capability)
}

func TestRegistryReturnsUnsupportedCapabilityForMissingReviewThreadResolver(t *testing.T) {
	registry, err := NewRegistry(testProvider{
		kind: KindForgejo,
		host: DefaultForgejoHost,
		caps: Capabilities{ReviewThreadResolution: false},
	})
	require.NoError(t, err)

	_, err = registry.DiffReviewThreadResolver(KindForgejo, DefaultForgejoHost)

	var platformErr *Error
	require.ErrorAs(t, err, &platformErr)
	require.ErrorIs(t, err, ErrUnsupportedCapability)
	assert := assert.New(t)
	assert.Equal(ErrCodeUnsupportedCapability, platformErr.Code)
	assert.Equal("review_thread_resolution", platformErr.Capability)
}

func TestRegistryReturnsUnsupportedCapabilityForMissingReviewThreadReader(t *testing.T) {
	registry, err := NewRegistry(testProvider{
		kind: KindForgejo,
		host: DefaultForgejoHost,
		caps: Capabilities{ReadReviewThreads: false},
	})
	require.NoError(t, err)

	_, err = registry.MergeRequestReviewThreadReader(KindForgejo, DefaultForgejoHost)

	var platformErr *Error
	require.ErrorAs(t, err, &platformErr)
	require.ErrorIs(t, err, ErrUnsupportedCapability)
	assert := assert.New(t)
	assert.Equal(ErrCodeUnsupportedCapability, platformErr.Code)
	assert.Equal("read_review_threads", platformErr.Capability)
}

func TestRegistryRejectsDuplicateProviderKeys(t *testing.T) {
	registry, err := NewRegistry(testProvider{
		kind: KindGitLab,
		host: "gitlab.com",
	})
	require.NoError(t, err)

	err = registry.Register(testProvider{
		kind: KindGitLab,
		host: "gitlab.com",
	})

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrProviderNotConfigured)
}

func TestNewRegistryReturnsErrorForDuplicateProviderKeys(t *testing.T) {
	_, err := NewRegistry(
		testProvider{
			kind: KindGitLab,
			host: "gitlab.com",
		},
		testProvider{
			kind: KindGitLab,
			host: "gitlab.com",
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider already registered for gitlab/gitlab.com")
}

func TestZeroValueRegistryCanRegisterProvider(t *testing.T) {
	var registry Registry

	err := registry.Register(testProvider{
		kind: KindGitLab,
		host: "gitlab.com",
	})
	require.NoError(t, err)

	got, err := registry.Provider(KindGitLab, "gitlab.com")
	require.NoError(t, err)

	assert := assert.New(t)
	assert.Equal(KindGitLab, got.Platform())
	assert.Equal("gitlab.com", got.Host())
}

type testNotificationProvider struct {
	testProvider
}

func (p testNotificationProvider) ListNotifications(
	context.Context, NotificationListOptions,
) ([]NotificationThread, bool, error) {
	return nil, false, nil
}

func (p testNotificationProvider) MarkNotificationThreadRead(
	context.Context, string,
) error {
	return nil
}

func TestRegistryFindsNotificationReaderAndMutator(t *testing.T) {
	require := require.New(t)

	registry, err := NewRegistry(testNotificationProvider{
		kind: KindGitHub,
		host: "github.com",
		caps: Capabilities{
			ReadNotifications:    true,
			NotificationMutation: true,
		},
	})
	require.NoError(err)

	reader, err := registry.NotificationReader(KindGitHub, "github.com")
	require.NoError(err)
	threads, hasNext, err := reader.ListNotifications(
		context.Background(), NotificationListOptions{},
	)
	require.NoError(err)
	assert := assert.New(t)
	assert.Empty(threads)
	assert.False(hasNext)

	mutator, err := registry.NotificationMutator(KindGitHub, "github.com")
	require.NoError(err)
	assert.NoError(mutator.MarkNotificationThreadRead(context.Background(), "1"))
}

// Providers ship stub notification methods before real support lands,
// so the registry must gate on the declared capability rather than
// interface satisfaction alone.
func TestRegistryReturnsUnsupportedCapabilityForStubNotificationProvider(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	registry, err := NewRegistry(testNotificationProvider{
		kind: KindGitLab,
		host: "gitlab.com",
		caps: Capabilities{},
	})
	require.NoError(err)

	_, err = registry.NotificationReader(KindGitLab, "gitlab.com")
	var platformErr *Error
	require.ErrorAs(err, &platformErr)
	require.ErrorIs(err, ErrUnsupportedCapability)
	assert.Equal("read_notifications", platformErr.Capability)

	_, err = registry.NotificationMutator(KindGitLab, "gitlab.com")
	require.ErrorAs(err, &platformErr)
	require.ErrorIs(err, ErrUnsupportedCapability)
	assert.Equal("notification_mutation", platformErr.Capability)
}

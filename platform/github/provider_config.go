package github

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/forge/platform"
)

type ProviderConfig struct {
	Host           string
	Client         API
	Clock          func() time.Time
	ViewerCacheTTL time.Duration
	Warning        func(string, ...any)
}

// NewProvider adapts the supplied GitHub API to neutral capability interfaces.
// The caller owns transports, credentials, observation policy and lifecycle.
func NewProvider(config ProviderConfig) (*Provider, error) {
	host := config.Host
	if config.Client == nil || config.Clock == nil {
		return nil, &platform.Error{Code: platform.ErrCodeInvalidArgument, Provider: platform.KindGitHub,
			PlatformHost: host, Err: errors.New("a GitHub client and clock are required")}
	}
	return &Provider{host: host, client: config.Client, now: config.Clock, viewerCacheTTL: config.ViewerCacheTTL, warning: config.Warning}, nil
}

func (p *Provider) warn(message string, args ...any) {
	if p.warning != nil {
		p.warning(message, args...)
	}
}

type issueTimelineLister interface {
	ListIssueTimelineEvents(context.Context, string, string, int) ([]PullRequestTimelineEvent, error)
}

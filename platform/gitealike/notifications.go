package gitealike

import (
	"context"

	"go.kenn.io/forge/platform"
)

// Notification support for Forgejo and Gitea is not implemented yet.
// Both expose a /notifications REST API close in shape to GitHub's;
// filling these in means adding the endpoints to Transport, mapping
// responses onto platform.NotificationThread, and flipping
// ReadNotifications/NotificationMutation in Capabilities(). Until
// then these stubs keep the provider's notification surface explicit
// and return typed unsupported_capability errors instead of silently
// falling back to GitHub-only behavior.

func (p *Provider) ListNotifications(
	_ context.Context,
	_ platform.NotificationListOptions,
) ([]platform.NotificationThread, bool, error) {
	return nil, false, platform.UnsupportedCapability(
		p.kind, p.host, "read_notifications",
	)
}

func (p *Provider) MarkNotificationThreadRead(_ context.Context, _ string) error {
	return platform.UnsupportedCapability(
		p.kind, p.host, "notification_mutation",
	)
}

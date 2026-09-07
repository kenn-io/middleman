package gitlab

import (
	"context"

	"go.kenn.io/forge/platform"
)

// Notification support for GitLab is not implemented yet. GitLab's
// closest equivalent is the to-do items API (/todos); filling these
// in means mapping to-dos onto platform.NotificationThread and
// flipping ReadNotifications/NotificationMutation in Capabilities().
// Until then these stubs keep the provider's notification surface
// explicit and return typed unsupported_capability errors instead of
// silently falling back to GitHub-only behavior.

func (c *Client) ListNotifications(
	_ context.Context,
	_ platform.NotificationListOptions,
) ([]platform.NotificationThread, bool, error) {
	return nil, false, platform.UnsupportedCapability(
		platform.KindGitLab, c.host, "read_notifications",
	)
}

func (c *Client) MarkNotificationThreadRead(_ context.Context, _ string) error {
	return platform.UnsupportedCapability(
		platform.KindGitLab, c.host, "notification_mutation",
	)
}

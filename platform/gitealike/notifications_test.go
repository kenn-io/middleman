package gitealike

import (
	"testing"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

// The notification stubs exist so unsupported providers fail with a
// typed capability error instead of silently behaving GitHub-only;
// pin both the error shape and the undeclared capability flags.
func TestNotificationStubsReturnUnsupportedCapability(t *testing.T) {
	provider := NewProvider(platform.KindForgejo, "codeberg.org", &fakeTransport{}, WithReadActions())

	assert := assert.New(t)
	require := Require.New(t)
	caps := provider.Capabilities()
	assert.False(caps.ReadNotifications)
	assert.False(caps.NotificationMutation)

	_, _, err := provider.ListNotifications(t.Context(), platform.NotificationListOptions{})
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	assert.Equal("read_notifications", platformErr.Capability)
	assert.Equal(platform.KindForgejo, platformErr.Provider)

	err = provider.MarkNotificationThreadRead(t.Context(), "1")
	require.ErrorAs(err, &platformErr)
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	assert.Equal("notification_mutation", platformErr.Capability)
}

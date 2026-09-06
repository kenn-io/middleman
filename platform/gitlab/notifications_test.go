package gitlab

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

// The notification stubs exist so GitLab fails with a typed
// capability error instead of silently behaving GitHub-only; pin
// both the error shape and the undeclared capability flags.
func TestNotificationStubsReturnUnsupportedCapability(t *testing.T) {
	client, err := NewClient("gitlab.example.com", testTokenSource("token"), WithTransport(http.DefaultTransport))
	require := Require.New(t)
	require.NoError(err)

	assert := assert.New(t)
	caps := client.Capabilities()
	assert.False(caps.ReadNotifications)
	assert.False(caps.NotificationMutation)

	_, _, err = client.ListNotifications(t.Context(), platform.NotificationListOptions{})
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	assert.Equal("read_notifications", platformErr.Capability)
	assert.Equal(platform.KindGitLab, platformErr.Provider)
	assert.Equal("gitlab.example.com", platformErr.PlatformHost)

	err = client.MarkNotificationThreadRead(t.Context(), "1")
	require.ErrorAs(err, &platformErr)
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	assert.Equal("notification_mutation", platformErr.Capability)
}

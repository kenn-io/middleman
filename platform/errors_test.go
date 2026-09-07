package platform

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepositoryFeatureDisabled(t *testing.T) {
	cause := errors.New("provider disabled issues")
	err := RepositoryFeatureDisabled(
		KindGitHub,
		"github.example.com",
		RepositoryFeatureIssues,
		cause,
	)

	require.ErrorIs(t, err, ErrRepositoryFeatureDisabled)
	require.ErrorIs(t, err, cause)
	var platformErr *Error
	require.ErrorAs(t, err, &platformErr)
	assert := assert.New(t)
	assert.Equal(ErrCodeRepositoryFeatureDisabled, platformErr.Code)
	assert.Equal(KindGitHub, platformErr.Provider)
	assert.Equal("github.example.com", platformErr.PlatformHost)
	assert.Equal(RepositoryFeatureIssues, platformErr.Capability)
}

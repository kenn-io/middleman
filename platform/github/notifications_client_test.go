package github

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNotificationItemNormalizesPullRequestURL(t *testing.T) {
	client := &Client{platformHost: "github.example.com"}
	itemType, number, webURL := client.notificationItem(
		"PullRequest",
		"https://github.example.com/api/v3/repos/acme/widget/pulls/42",
		"acme",
		"widget",
	)

	check := assert.New(t)
	check.Equal("pr", itemType)
	if check.NotNil(number) {
		check.Equal(42, *number)
	}
	check.Equal("https://github.example.com/acme/widget/pull/42", webURL)
}

func TestNotificationItemNormalizesPullRequestIssueURL(t *testing.T) {
	client := &Client{platformHost: "github.example.com"}
	itemType, number, webURL := client.notificationItem(
		"PullRequest",
		"https://github.example.com/api/v3/repos/acme/widget/issues/42",
		"acme",
		"widget",
	)

	check := assert.New(t)
	check.Equal("pr", itemType)
	if check.NotNil(number) {
		check.Equal(42, *number)
	}
	check.Equal("https://github.example.com/acme/widget/pull/42", webURL)
}

func TestNotificationItemKeepsExternalOnlySubjectsVisible(t *testing.T) {
	client := &Client{platformHost: "github.com"}
	itemType, number, webURL := client.notificationItem(
		"Discussion",
		"https://api.github.com/repos/acme/widget/discussions/5",
		"acme",
		"widget",
	)

	check := assert.New(t)
	check.Equal("other", itemType)
	check.Nil(number)
	check.Empty(webURL)
}

func TestNotificationItemDoesNotSynthesizeReleaseURLFromAPIID(t *testing.T) {
	client := &Client{platformHost: "github.com"}
	itemType, number, webURL := client.notificationItem(
		"Release",
		"https://api.github.com/repos/acme/widget/releases/12345",
		"acme",
		"widget",
	)

	check := assert.New(t)
	check.Equal("release", itemType)
	check.Nil(number)
	check.Empty(webURL)
}

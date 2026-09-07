package gitlab

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

func gitlabLabelTestRef() platform.RepoRef {
	return platform.RepoRef{
		Platform:   platform.KindGitLab,
		Host:       "gitlab.example.com",
		Owner:      "acme",
		Name:       "widget",
		RepoPath:   "acme/widget",
		PlatformID: 42,
	}
}

func TestClientListLabelsCollectsPagesAndNormalizes(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v4/projects/42/labels", r.URL.EscapedPath())
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		if page == "2" {
			writeJSON(w, `[{"id": 5, "name": "triage", "color": "#fbca04", "description": ""}]`)
			return
		}
		w.Header().Set("X-Next-Page", "2")
		writeJSON(w, `[{"id": 4, "name": "bug", "color": "#d73a4a", "description": "Something is broken"}]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	catalog, err := client.ListLabels(context.Background(), gitlabLabelTestRef())
	require.NoError(err)
	require.Len(catalog.Labels, 2)
	assert.Equal([]string{"1", "2"}, pages)
	assert.Equal("bug", catalog.Labels[0].Name)
	assert.Equal("Something is broken", catalog.Labels[0].Description)
	assert.Equal("#d73a4a", catalog.Labels[0].Color)
	assert.Equal(int64(4), catalog.Labels[0].PlatformID)
	assert.Equal("4", catalog.Labels[0].PlatformExternalID)
	assert.Equal("triage", catalog.Labels[1].Name)
}

func TestClientSetLabelsSendsCommaJoinedNames(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		response string
		set      func(*Client) ([]platform.Label, error)
	}{
		{
			name:     "merge request",
			path:     "/api/v4/projects/42/merge_requests/7",
			response: `{"id": 1001, "iid": 7, "project_id": 42, "state": "opened", "labels": ["bug", "triage"]}`,
			set: func(client *Client) ([]platform.Label, error) {
				return client.SetMergeRequestLabels(
					context.Background(), gitlabLabelTestRef(), 7, []string{"bug", "triage"},
				)
			},
		},
		{
			name:     "issue",
			path:     "/api/v4/projects/42/issues/11",
			response: `{"id": 3001, "iid": 11, "project_id": 42, "state": "opened", "labels": ["bug", "triage"]}`,
			set: func(client *Client) ([]platform.Label, error) {
				return client.SetIssueLabels(
					context.Background(), gitlabLabelTestRef(), 11, []string{"bug", "triage"},
				)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := Require.New(t)
			assert := assert.New(t)
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(http.MethodPut, r.Method)
				assert.Equal(tt.path, r.URL.EscapedPath())
				raw, err := io.ReadAll(r.Body)
				assert.NoError(err)
				assert.NoError(json.Unmarshal(raw, &body))
				writeJSON(w, tt.response)
			}))
			defer server.Close()

			labels, err := tt.set(newTestClient(t, server.URL))
			require.NoError(err)
			assert.Equal("bug,triage", body["labels"])
			require.Len(labels, 2)
			assert.Equal("bug", labels[0].Name)
			assert.Equal("triage", labels[1].Name)
		})
	}
}

func TestClientSetLabelsSendsEmptyStringToClearAll(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		assert.NoError(err)
		assert.NoError(json.Unmarshal(raw, &body))
		writeJSON(w, `{"id": 1001, "iid": 7, "project_id": 42, "state": "opened", "labels": []}`)
	}))
	defer server.Close()

	labels, err := newTestClient(t, server.URL).SetMergeRequestLabels(
		context.Background(), gitlabLabelTestRef(), 7, nil,
	)
	require.NoError(err)
	assert.Empty(labels)
	value, ok := body["labels"]
	require.True(ok, "labels field must be present so GitLab clears assignments")
	cleared, isString := value.(string)
	require.True(isString, "labels must be a string; JSON null leaves GitLab labels untouched")
	assert.Empty(cleared)
}

func TestClientSetLabelsRejectsCommaNamesWithoutCallingProvider(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeJSON(w, `{}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)

	for _, set := range []func() ([]platform.Label, error){
		func() ([]platform.Label, error) {
			return client.SetMergeRequestLabels(
				context.Background(), gitlabLabelTestRef(), 7, []string{"bug", "reviewed,deploy"},
			)
		},
		func() ([]platform.Label, error) {
			return client.SetIssueLabels(
				context.Background(), gitlabLabelTestRef(), 11, []string{"reviewed,deploy"},
			)
		},
	} {
		_, err := set()
		require.ErrorIs(err, platform.ErrInvalidArgument)
		assert.Contains(err.Error(), `"reviewed,deploy"`)
	}
	assert.Zero(requests, "comma-name rejection must happen before any provider call")
}

func TestClientSetLabelsMapsProviderErrors(t *testing.T) {
	require := Require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message": "403 Forbidden"}`, http.StatusForbidden)
	}))
	defer server.Close()

	_, err := newTestClient(t, server.URL).SetIssueLabels(
		context.Background(), gitlabLabelTestRef(), 11, []string{"bug"},
	)

	require.ErrorIs(err, platform.ErrPermissionDenied)
}

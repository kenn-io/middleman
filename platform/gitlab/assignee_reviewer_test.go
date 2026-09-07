package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

type updateBody struct {
	AssigneeIDs *[]int64 `json:"assignee_ids"`
	ReviewerIDs *[]int64 `json:"reviewer_ids"`
}

func TestSetMergeRequestAssigneesResolvesAndCachesUserIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	userLookups := 0
	var updates []updateBody
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/users":
			userLookups++
			switch r.URL.Query().Get("username") {
			case "alice":
				writeJSON(w, `[{"id": 5, "username": "alice"}]`)
			// bob is deliberately invisible to /users search: his ID
			// must come from the merge request's own assignee list.
			default:
				writeJSON(w, `[]`)
			}
		case r.URL.Path == "/api/v4/projects/42/merge_requests/7" && r.Method == http.MethodGet:
			writeJSON(w, `{"id": 1001, "iid": 7, "state": "opened", "assignees": [{"id": 6, "username": "Bob"}]}`)
		case r.URL.Path == "/api/v4/projects/42/merge_requests/7" && r.Method == http.MethodPut:
			var body updateBody
			_ = json.NewDecoder(r.Body).Decode(&body) // zero values fail the content assertions below
			updates = append(updates, body)
			writeJSON(w, `{
				"id": 1001, "iid": 7, "state": "opened",
				"assignees": [{"id": 5, "username": "alice"}, {"id": 6, "username": "Bob"}]
			}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	ref := platform.RepoRef{Platform: platform.KindGitLab, Host: "gitlab.example.com", RepoPath: "acme/widget", PlatformID: 42}

	assignees, err := client.SetMergeRequestAssignees(context.Background(), ref, 7, []string{"alice", "bob"})
	require.NoError(err)
	assert.Equal([]string{"alice", "Bob"}, assignees)
	require.Len(updates, 1)
	require.NotNil(updates[0].AssigneeIDs)
	assert.Equal([]int64{5, 6}, *updates[0].AssigneeIDs)
	// Only alice needed search: bob's ID was seeded from the merge
	// request's current assignee list.
	assert.Equal(1, userLookups)

	// A second mutation must reuse the cached user IDs.
	_, err = client.SetMergeRequestAssignees(context.Background(), ref, 7, []string{"alice", "bob"})
	require.NoError(err)
	assert.Equal(1, userLookups)
}

func TestSetIssueAssigneesUpdatesAssigneeIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var update updateBody
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/users":
			writeJSON(w, `[{"id": 9, "username": "dana"}]`)
		case r.URL.Path == "/api/v4/projects/42/issues/3" && r.Method == http.MethodGet:
			writeJSON(w, `{"id": 2001, "iid": 3, "state": "opened", "assignees": []}`)
		case r.URL.Path == "/api/v4/projects/42/issues/3" && r.Method == http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&update) // zero values fail the content assertions below
			writeJSON(w, `{"id": 2001, "iid": 3, "state": "opened", "assignees": [{"id": 9, "username": "dana"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	ref := platform.RepoRef{Platform: platform.KindGitLab, Host: "gitlab.example.com", RepoPath: "acme/widget", PlatformID: 42}

	assignees, err := client.SetIssueAssignees(context.Background(), ref, 3, []string{"dana"})
	require.NoError(err)
	assert.Equal([]string{"dana"}, assignees)
	require.NotNil(update.AssigneeIDs)
	assert.Equal([]int64{9}, *update.AssigneeIDs)
}

func TestRequestAndRemoveMergeRequestReviewersDiffAgainstCurrentSet(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	reviewers := `[{"id": 9, "username": "carol"}]`
	var updates []updateBody
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/users":
			switch r.URL.Query().Get("username") {
			case "alice":
				writeJSON(w, `[{"id": 5, "username": "alice"}]`)
			// carol is deliberately invisible to /users search: her ID
			// must come from the merge request's own reviewer list, so
			// retaining an existing reviewer never depends on search.
			default:
				writeJSON(w, `[]`)
			}
		case r.URL.Path == "/api/v4/projects/42/merge_requests/7" && r.Method == http.MethodGet:
			writeJSON(w, `{"id": 1001, "iid": 7, "state": "opened", "reviewers": `+reviewers+`}`)
		case r.URL.Path == "/api/v4/projects/42/merge_requests/7" && r.Method == http.MethodPut:
			var body updateBody
			_ = json.NewDecoder(r.Body).Decode(&body) // zero values fail the content assertions below
			updates = append(updates, body)
			if len(updates) == 1 {
				reviewers = `[{"id": 9, "username": "carol"}, {"id": 5, "username": "alice"}]`
			} else {
				reviewers = `[{"id": 5, "username": "alice"}]`
			}
			writeJSON(w, `{"id": 1001, "iid": 7, "state": "opened", "reviewers": `+reviewers+`}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	ref := platform.RepoRef{Platform: platform.KindGitLab, Host: "gitlab.example.com", RepoPath: "acme/widget", PlatformID: 42}

	requested, err := client.RequestMergeRequestReviewers(context.Background(), ref, 7, []string{"alice"})
	require.NoError(err)
	assert.Equal([]string{"carol", "alice"}, requested)
	require.Len(updates, 1)
	require.NotNil(updates[0].ReviewerIDs)
	assert.Equal([]int64{9, 5}, *updates[0].ReviewerIDs)

	removed, err := client.RemoveMergeRequestReviewers(context.Background(), ref, 7, []string{"carol"})
	require.NoError(err)
	assert.Equal([]string{"alice"}, removed)
	require.Len(updates, 2)
	require.NotNil(updates[1].ReviewerIDs)
	assert.Equal([]int64{5}, *updates[1].ReviewerIDs)
}

func TestRequestMergeRequestReviewersWithEmptyListReadsWithoutMutating(t *testing.T) {
	puts := 0
	userLookups := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/users":
			userLookups++
			writeJSON(w, `[]`)
		case r.URL.Path == "/api/v4/projects/42/merge_requests/7" && r.Method == http.MethodGet:
			writeJSON(w, `{"id": 1001, "iid": 7, "state": "opened", "reviewers": [{"id": 9, "username": "carol"}]}`)
		case r.Method == http.MethodPut:
			puts++
			writeJSON(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	ref := platform.RepoRef{Platform: platform.KindGitLab, Host: "gitlab.example.com", RepoPath: "acme/widget", PlatformID: 42}

	// The ReviewerMutator contract treats an empty request as a read of
	// the current requested-reviewer set.
	assert := assert.New(t)
	current, err := client.RequestMergeRequestReviewers(context.Background(), ref, 7, nil)
	require.NoError(t, err)
	assert.Equal([]string{"carol"}, current)
	assert.Zero(puts)
	assert.Zero(userLookups)
}

func TestRequestMergeRequestReviewersSkipsUpdateWhenAlreadyRequested(t *testing.T) {
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/projects/42/merge_requests/7" && r.Method == http.MethodGet:
			writeJSON(w, `{"id": 1001, "iid": 7, "state": "opened", "reviewers": [{"id": 9, "username": "carol"}]}`)
		case r.Method == http.MethodPut:
			puts++
			writeJSON(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	ref := platform.RepoRef{Platform: platform.KindGitLab, Host: "gitlab.example.com", RepoPath: "acme/widget", PlatformID: 42}

	requested, err := client.RequestMergeRequestReviewers(context.Background(), ref, 7, []string{"carol"})
	require.NoError(t, err)
	assert.Equal(t, []string{"carol"}, requested)
	assert.Zero(t, puts)
}

func TestLookupUserIDReturnsNotFoundForUnknownUsername(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/users":
			writeJSON(w, `[]`)
		case r.URL.Path == "/api/v4/projects/42/merge_requests/7" && r.Method == http.MethodGet:
			writeJSON(w, `{"id": 1001, "iid": 7, "state": "opened", "assignees": []}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	ref := platform.RepoRef{Platform: platform.KindGitLab, Host: "gitlab.example.com", RepoPath: "acme/widget", PlatformID: 42}

	_, err := client.SetMergeRequestAssignees(context.Background(), ref, 7, []string{"ghost"})
	var platformErr *platform.Error
	require.ErrorAs(t, err, &platformErr)
	assert.Equal(t, platform.ErrCodeNotFound, platformErr.Code)
}

package e2etest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
	gitlabprovider "go.kenn.io/forge/platform/gitlab"
)

// fakeGitLabReviewerAPI serves the minimal GitLab v4 surface the
// reviewer replace-set flow touches: project lookup, the merge request
// (whose reviewer list carries IDs), exact-username user search, and
// the reviewer_ids update. The /users endpoint deliberately knows only
// alice: carol is visible on the merge request but absent from search,
// which is the production condition the retained-ID seeding exists for.
type fakeGitLabReviewerAPI struct {
	mu                 sync.Mutex
	reviewersJSON      string
	assigneesJSON      string
	issueAssigneesJSON string
	userQueries        []string
	reviewerIDs        [][]int64
	assigneeIDs        [][]int64
	issueAssigneeIDs   [][]int64
}

func (f *fakeGitLabReviewerAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key := r.Method + " " + r.URL.EscapedPath()
		switch key {
		case "GET /api/v4/projects/acme%2Fwidget":
			_, _ = w.Write([]byte(`{
				"id": 42,
				"path": "widget",
				"path_with_namespace": "acme/widget",
				"default_branch": "main",
				"web_url": "https://gitlab.com/acme/widget",
				"http_url_to_repo": "https://gitlab.com/acme/widget.git"
			}`))
		case "GET /api/v4/users":
			username := r.URL.Query().Get("username")
			f.mu.Lock()
			f.userQueries = append(f.userQueries, username)
			f.mu.Unlock()
			if username == "alice" {
				_, _ = w.Write([]byte(`[{"id": 5, "username": "alice"}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case "GET /api/v4/projects/42/merge_requests/7":
			f.mu.Lock()
			reviewersJSON := f.reviewersJSON
			assigneesJSON := f.assigneesJSON
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w,
				`{"id": 1001, "iid": 7, "project_id": 42, "state": "opened", "reviewers": %s, "assignees": %s}`,
				reviewersJSON, assigneesJSON,
			)
		case "PUT /api/v4/projects/42/merge_requests/7":
			var body struct {
				ReviewerIDs *[]int64 `json:"reviewer_ids"`
				AssigneeIDs *[]int64 `json:"assignee_ids"`
			}
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			f.mu.Lock()
			switch {
			case body.ReviewerIDs != nil:
				f.reviewerIDs = append(f.reviewerIDs, *body.ReviewerIDs)
				f.reviewersJSON = `[{"id": 9, "username": "carol"}, {"id": 5, "username": "alice"}]`
			case body.AssigneeIDs != nil:
				f.assigneeIDs = append(f.assigneeIDs, *body.AssigneeIDs)
				f.assigneesJSON = `[{"id": 6, "username": "bob"}, {"id": 5, "username": "alice"}]`
			default:
				f.mu.Unlock()
				http.Error(w, `{"message": "reviewer_ids or assignee_ids required"}`, http.StatusBadRequest)
				return
			}
			reviewersJSON := f.reviewersJSON
			assigneesJSON := f.assigneesJSON
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w,
				`{"id": 1001, "iid": 7, "project_id": 42, "state": "opened", "reviewers": %s, "assignees": %s}`,
				reviewersJSON, assigneesJSON,
			)
		case "GET /api/v4/projects/42/issues/11":
			f.mu.Lock()
			issueAssigneesJSON := f.issueAssigneesJSON
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w,
				`{"id": 3001, "iid": 11, "project_id": 42, "state": "opened", "assignees": %s}`,
				issueAssigneesJSON,
			)
		case "PUT /api/v4/projects/42/issues/11":
			var body struct {
				AssigneeIDs *[]int64 `json:"assignee_ids"`
			}
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if body.AssigneeIDs == nil {
				http.Error(w, `{"message": "assignee_ids required"}`, http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.issueAssigneeIDs = append(f.issueAssigneeIDs, *body.AssigneeIDs)
			f.issueAssigneesJSON = `[{"id": 7, "username": "dana"}, {"id": 5, "username": "alice"}]`
			issueAssigneesJSON := f.issueAssigneesJSON
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w,
				`{"id": 3001, "iid": 11, "project_id": 42, "state": "opened", "assignees": %s}`,
				issueAssigneesJSON,
			)
		default:
			http.NotFound(w, r)
		}
	})
}

// A reviewer who is already on the merge request may be invisible to
// /users search (search visibility differs from membership). Adding a
// second reviewer must keep the existing one by reusing the ID the
// merge request already reported, not by re-resolving through search.
func TestGitLabSetPullReviewersRetainsReviewerAbsentFromUserSearch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	fake := &fakeGitLabReviewerAPI{
		reviewersJSON: `[{"id": 9, "username": "carol"}]`,
		assigneesJSON: `[]`,
	}
	upstream := httptest.NewServer(fake.handler(t))
	t.Cleanup(upstream.Close)

	client, err := gitlabprovider.NewClient(
		platform.DefaultGitLabHost,
		staticTokenSource("token"),
		gitlabprovider.WithBaseURLForTesting(upstream.URL+"/api/v4"), gitlabprovider.WithTransport(http.DefaultTransport))
	require.NoError(err)

	repoID := seedProviderRepo(t, database, platform.KindGitLab, platform.DefaultGitLabHost)
	seedProviderPRAndIssue(t, database, repoID)
	srv := newLabelTestServer(t, database, client, platform.KindGitLab, platform.DefaultGitLabHost)

	rr := doJSONRequest(t, srv, http.MethodPut, "/api/v1/pulls/gitlab/acme/widget/7/reviewers", map[string][]string{
		"reviewers": {"carol", "alice"},
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Reviewers []string `json:"reviewers"`
	}
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal([]string{"carol", "alice"}, body.Reviewers)

	// The update reused carol's ID from the merge request and resolved
	// only alice through search.
	require.Len(fake.reviewerIDs, 1)
	assert.Equal([]int64{9, 5}, fake.reviewerIDs[0])
	assert.Equal([]string{"alice"}, fake.userQueries)

	pr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 7)
	require.NoError(err)
	require.NotNil(pr)
	assert.Equal([]string{"carol", "alice"}, pr.RequestedReviewers)
}

// The same retained-identity rule holds for assignees: a user already
// assigned on the merge request may be invisible to /users search, and
// keeping them while adding someone else must reuse the ID the merge
// request reports.
func TestGitLabSetPullAssigneesRetainsAssigneeAbsentFromUserSearch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	fake := &fakeGitLabReviewerAPI{
		reviewersJSON: `[]`,
		assigneesJSON: `[{"id": 6, "username": "bob"}]`,
	}
	upstream := httptest.NewServer(fake.handler(t))
	t.Cleanup(upstream.Close)

	client, err := gitlabprovider.NewClient(
		platform.DefaultGitLabHost,
		staticTokenSource("token"),
		gitlabprovider.WithBaseURLForTesting(upstream.URL+"/api/v4"), gitlabprovider.WithTransport(http.DefaultTransport))
	require.NoError(err)

	repoID := seedProviderRepo(t, database, platform.KindGitLab, platform.DefaultGitLabHost)
	seedProviderPRAndIssue(t, database, repoID)
	srv := newLabelTestServer(t, database, client, platform.KindGitLab, platform.DefaultGitLabHost)

	rr := doJSONRequest(t, srv, http.MethodPut, "/api/v1/pulls/gitlab/acme/widget/7/assignees", map[string][]string{
		"assignees": {"bob", "alice"},
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Assignees []string `json:"assignees"`
	}
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal([]string{"bob", "alice"}, body.Assignees)

	require.Len(fake.assigneeIDs, 1)
	assert.Equal([]int64{6, 5}, fake.assigneeIDs[0])
	assert.Equal([]string{"alice"}, fake.userQueries)

	pr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 7)
	require.NoError(err)
	require.NotNil(pr)
	assert.Equal([]string{"bob", "alice"}, pr.Assignees)
}

// And the issue route: the retained issue assignee comes from
// GET /issues/{iid}, not from /users search.
func TestGitLabSetIssueAssigneesRetainsAssigneeAbsentFromUserSearch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	fake := &fakeGitLabReviewerAPI{
		reviewersJSON:      `[]`,
		assigneesJSON:      `[]`,
		issueAssigneesJSON: `[{"id": 7, "username": "dana"}]`,
	}
	upstream := httptest.NewServer(fake.handler(t))
	t.Cleanup(upstream.Close)

	client, err := gitlabprovider.NewClient(
		platform.DefaultGitLabHost,
		staticTokenSource("token"),
		gitlabprovider.WithBaseURLForTesting(upstream.URL+"/api/v4"), gitlabprovider.WithTransport(http.DefaultTransport))
	require.NoError(err)

	repoID := seedProviderRepo(t, database, platform.KindGitLab, platform.DefaultGitLabHost)
	seedProviderPRAndIssue(t, database, repoID)
	srv := newLabelTestServer(t, database, client, platform.KindGitLab, platform.DefaultGitLabHost)

	rr := doJSONRequest(t, srv, http.MethodPut, "/api/v1/issues/gitlab/acme/widget/11/assignees", map[string][]string{
		"assignees": {"dana", "alice"},
	})
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Assignees []string `json:"assignees"`
	}
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal([]string{"dana", "alice"}, body.Assignees)

	require.Len(fake.issueAssigneeIDs, 1)
	assert.Equal([]int64{7, 5}, fake.issueAssigneeIDs[0])
	assert.Equal([]string{"alice"}, fake.userQueries)

	issue, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 11)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal([]string{"dana", "alice"}, issue.Assignees)
}

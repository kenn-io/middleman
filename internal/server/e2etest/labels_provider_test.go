package e2etest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/forgejo"
	"go.kenn.io/forge/platform/gitea"
	gitlabprovider "go.kenn.io/forge/platform/gitlab"
)

type staticTokenSource string

func (s staticTokenSource) Token(context.Context) (string, error) { return string(s), nil }
func (s staticTokenSource) Invalidate(string)                     {}
func (s staticTokenSource) Descriptor() tokenauth.Descriptor {
	return tokenauth.Descriptor{}
}

type labelWireResponse struct {
	Labels []db.Label `json:"labels"`
}

type repoCapabilitiesWire []struct {
	Owner        string `json:"owner"`
	Name         string `json:"name"`
	Capabilities struct {
		ReadLabels    bool `json:"read_labels"`
		LabelMutation bool `json:"label_mutation"`
	} `json:"capabilities"`
}

// assertRepoLabelCapabilities checks the payload the label picker UI is
// gated on: both label capabilities must be advertised for the repo.
func assertRepoLabelCapabilities(t *testing.T, srv http.Handler) {
	t.Helper()
	rr := doJSONRequest(t, srv, http.MethodGet, "/api/v1/repos", nil)
	require.Equal(t, http.StatusOK, rr.Code, "response: %s", rr.Body.String())
	var body repoCapabilitiesWire
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body, 1)
	assert.True(t, body[0].Capabilities.ReadLabels, "read_labels must be advertised")
	assert.True(t, body[0].Capabilities.LabelMutation, "label_mutation must be advertised")
}

func seedProviderRepo(
	t *testing.T,
	database *db.DB,
	kind platform.Kind,
	host string,
) int64 {
	t.Helper()
	repoID, err := database.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform:       string(kind),
		PlatformHost:   host,
		PlatformRepoID: "repo-" + string(kind) + "-acme-widget",
		Owner:          "acme",
		Name:           "widget",
		RepoPath:       "acme/widget",
	})
	require.NoError(t, err)
	return repoID
}

func seedProviderPRAndIssue(t *testing.T, database *db.DB, repoID int64) {
	t.Helper()
	now := time.Now().UTC()
	_, err := database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID:         repoID,
		PlatformID:     1001,
		Number:         7,
		Title:          "Label target PR",
		Author:         "author",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(t, err)
	_, err = database.UpsertIssue(t.Context(), &db.Issue{
		RepoID:         repoID,
		PlatformID:     3001,
		Number:         11,
		Title:          "Label target issue",
		Author:         "author",
		State:          "open",
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(t, err)
}

// seedAssignedLabel attaches an existing label to the PR or issue so a
// clear-all test starts from a non-empty assignment and proves removal.
func seedAssignedLabel(t *testing.T, database *db.DB, repoID int64, kind string, number int) {
	t.Helper()
	now := time.Now().UTC()
	seeded := []db.Label{{Name: "bug", Color: "d73a4a", UpdatedAt: now}}
	switch kind {
	case "pull":
		mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, number)
		require.NoError(t, err)
		require.NotNil(t, mr)
		require.NoError(t, database.ReplaceMergeRequestLabels(t.Context(), repoID, mr.ID, seeded))
		mr, err = database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, number)
		require.NoError(t, err)
		require.NotEmpty(t, mr.Labels, "seeded pull must start with an assigned label")
	case "issue":
		issue, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, number)
		require.NoError(t, err)
		require.NotNil(t, issue)
		require.NoError(t, database.ReplaceIssueLabels(t.Context(), repoID, issue.ID, seeded))
		issue, err = database.GetIssueByRepoIDAndNumber(t.Context(), repoID, number)
		require.NoError(t, err)
		require.NotEmpty(t, issue.Labels, "seeded issue must start with an assigned label")
	}
}

func newLabelTestServer(
	t *testing.T,
	database *db.DB,
	provider platform.Provider,
	kind platform.Kind,
	host string,
) *server.Server {
	t.Helper()
	registry, err := platform.NewRegistry(provider)
	require.NoError(t, err)
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil,
		[]ghclient.RepoRef{{
			Platform:           kind,
			PlatformHost:       host,
			PlatformExternalID: "repo-" + string(kind) + "-acme-widget",
			Owner:              "acme",
			Name:               "widget",
			RepoPath:           "acme/widget",
		}},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
	})
	return srv
}

func doJSONRequest(
	t *testing.T,
	srv http.Handler,
	method, path string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&payload).Encode(body))
	}
	req := httptest.NewRequest(method, path, &payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// fakeGitLabAPI serves the minimal GitLab v4 surface the label flows
// touch: project lookup, the project label catalog, and label
// assignment on a merge request and an issue. Assignment responses echo
// the requested names the way GitLab does: bare label names, no color.
type fakeGitLabAPI struct {
	mu             sync.Mutex
	catalogJSON    string
	mrLabelBody    map[string]any
	issueLabelBody map[string]any
}

// gitlabAssignedLabelsJSON mirrors GitLab's contract for the labels
// param: it must be a string ("" clears every label). JSON null or a
// missing field leaves labels untouched, so the fake rejects those
// instead of accidentally treating them as a clear.
func gitlabAssignedLabelsJSON(body map[string]any) (string, bool) {
	raw, isString := body["labels"].(string)
	if !isString {
		return "", false
	}
	if raw == "" {
		return "[]", true
	}
	names := strings.Split(raw, ",")
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, fmt.Sprintf("%q", name))
	}
	return "[" + strings.Join(quoted, ",") + "]", true
}

func (f *fakeGitLabAPI) handler(t *testing.T) http.Handler {
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
		case "GET /api/v4/projects/42/labels":
			f.mu.Lock()
			catalogJSON := f.catalogJSON
			f.mu.Unlock()
			_, _ = w.Write([]byte(catalogJSON))
		case "PUT /api/v4/projects/42/merge_requests/7":
			var body map[string]any
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			f.mu.Lock()
			f.mrLabelBody = body
			f.mu.Unlock()
			labelsJSON, isString := gitlabAssignedLabelsJSON(body)
			if !isString {
				http.Error(w, `{"message": "labels must be a string"}`, http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(w,
				`{"id": 1001, "iid": 7, "project_id": 42, "state": "opened", "labels": %s}`,
				labelsJSON,
			)
		case "PUT /api/v4/projects/42/issues/11":
			var body map[string]any
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			f.mu.Lock()
			f.issueLabelBody = body
			f.mu.Unlock()
			labelsJSON, isString := gitlabAssignedLabelsJSON(body)
			if !isString {
				http.Error(w, `{"message": "labels must be a string"}`, http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(w,
				`{"id": 3001, "iid": 11, "project_id": 42, "state": "opened", "labels": %s}`,
				labelsJSON,
			)
		default:
			http.NotFound(w, r)
		}
	})
}

func setupGitLabLabelStack(t *testing.T) (*server.Server, *db.DB, int64, *fakeGitLabAPI) {
	t.Helper()
	database := dbtest.Open(t)
	fake := &fakeGitLabAPI{catalogJSON: `[
		{"id": 4, "name": "bug", "color": "#d73a4a", "description": "Something is broken"},
		{"id": 5, "name": "triage", "color": "#fbca04", "description": "Needs review"}
	]`}
	upstream := httptest.NewServer(fake.handler(t))
	t.Cleanup(upstream.Close)

	client, err := gitlabprovider.NewClient(
		platform.DefaultGitLabHost,
		staticTokenSource("token"),
		gitlabprovider.WithBaseURLForTesting(upstream.URL+"/api/v4"), gitlabprovider.WithTransport(http.DefaultTransport))
	require.NoError(t, err)

	repoID := seedProviderRepo(t, database, platform.KindGitLab, platform.DefaultGitLabHost)
	seedProviderPRAndIssue(t, database, repoID)
	srv := newLabelTestServer(t, database, client, platform.KindGitLab, platform.DefaultGitLabHost)
	return srv, database, repoID, fake
}

func TestGitLabListRepoLabelsSyncsCatalogFromProvider(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, repoID, _ := setupGitLabLabelStack(t)
	assertRepoLabelCapabilities(t, srv)

	rr := doJSONRequest(t, srv, http.MethodGet, "/api/v1/repo/gitlab/acme/widget/labels", nil)
	require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

	require.Eventually(func() bool {
		labels, _, err := database.ListRepoLabelCatalog(t.Context(), repoID)
		return err == nil && len(labels) == 2
	}, 2*time.Second, 10*time.Millisecond)

	labels, _, err := database.ListRepoLabelCatalog(t.Context(), repoID)
	require.NoError(err)
	require.Len(labels, 2)
	assert.Equal("bug", labels[0].Name)
	assert.Equal("#d73a4a", labels[0].Color)
	assert.Equal("Something is broken", labels[0].Description)
	assert.Equal("triage", labels[1].Name)
	assert.Equal("Needs review", labels[1].Description)
}

func TestGitLabSetPullLabelsUpdatesProviderAndDB(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, repoID, fake := setupGitLabLabelStack(t)

	rr := doJSONRequest(t, srv, http.MethodPut, "/api/v1/pulls/gitlab/acme/widget/7/labels", map[string][]string{
		"labels": {"triage"},
	})
	require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

	var body labelWireResponse
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(body.Labels, 1)
	assert.Equal("triage", body.Labels[0].Name)
	assert.Equal("#fbca04", body.Labels[0].Color,
		"response must carry the catalog color even though GitLab returns names only")

	fake.mu.Lock()
	sent := fake.mrLabelBody
	fake.mu.Unlock()
	require.NotNil(sent, "provider must receive the label update")
	assert.Equal("triage", sent["labels"])

	mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	require.Len(mr.Labels, 1)
	assert.Equal("triage", mr.Labels[0].Name)
	assert.Equal("#fbca04", mr.Labels[0].Color,
		"stored label must carry the catalog color even though GitLab returns names only")
	assert.Equal("Needs review", mr.Labels[0].Description)
}

func TestGitLabSetIssueLabelsUpdatesProviderAndDB(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, repoID, fake := setupGitLabLabelStack(t)

	rr := doJSONRequest(t, srv, http.MethodPut, "/api/v1/issues/gitlab/acme/widget/11/labels", map[string][]string{
		"labels": {"triage"},
	})
	require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

	var body labelWireResponse
	require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(body.Labels, 1)
	assert.Equal("triage", body.Labels[0].Name)
	assert.Equal("#fbca04", body.Labels[0].Color,
		"response must carry the catalog color even though GitLab returns names only")
	assert.Equal("Needs review", body.Labels[0].Description)

	fake.mu.Lock()
	sent := fake.issueLabelBody
	fake.mu.Unlock()
	require.NotNil(sent, "provider must receive the label update")
	assert.Equal("triage", sent["labels"])

	issue, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 11)
	require.NoError(err)
	require.NotNil(issue)
	require.Len(issue.Labels, 1)
	assert.Equal("triage", issue.Labels[0].Name)
	assert.Equal("#fbca04", issue.Labels[0].Color,
		"stored label must carry the catalog color even though GitLab returns names only")
}

// An empty labels array is the API contract for "remove every label":
// starting from an existing assignment, the provider must receive an
// explicit clear (labels="" on GitLab) and the stored assignment must
// end up empty. A missing or null labels field is rejected instead
// (covered in internal/server/apitest).
func TestGitLabSetLabelsEmptyArrayClearsAll(t *testing.T) {
	tests := []struct {
		name     string
		kind     string
		number   int
		path     string
		sentBody func(fake *fakeGitLabAPI) map[string]any
		stored   func(t *testing.T, database *db.DB, repoID int64) []db.Label
	}{
		{
			name:   "pull",
			kind:   "pull",
			number: 7,
			path:   "/api/v1/pulls/gitlab/acme/widget/7/labels",
			sentBody: func(fake *fakeGitLabAPI) map[string]any {
				fake.mu.Lock()
				defer fake.mu.Unlock()
				return fake.mrLabelBody
			},
			stored: func(t *testing.T, database *db.DB, repoID int64) []db.Label {
				t.Helper()
				mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 7)
				require.NoError(t, err)
				require.NotNil(t, mr)
				return mr.Labels
			},
		},
		{
			name:   "issue",
			kind:   "issue",
			number: 11,
			path:   "/api/v1/issues/gitlab/acme/widget/11/labels",
			sentBody: func(fake *fakeGitLabAPI) map[string]any {
				fake.mu.Lock()
				defer fake.mu.Unlock()
				return fake.issueLabelBody
			},
			stored: func(t *testing.T, database *db.DB, repoID int64) []db.Label {
				t.Helper()
				issue, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 11)
				require.NoError(t, err)
				require.NotNil(t, issue)
				return issue.Labels
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, database, repoID, fake := setupGitLabLabelStack(t)
			seedAssignedLabel(t, database, repoID, tt.kind, tt.number)

			rr := doJSONRequest(t, srv, http.MethodPut, tt.path, map[string][]string{
				"labels": {},
			})
			require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

			var body labelWireResponse
			require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
			assert.Empty(body.Labels)

			sent := tt.sentBody(fake)
			require.NotNil(sent, "provider must receive the label update")
			value, ok := sent["labels"]
			require.True(ok, "labels field must be sent so GitLab clears assignments")
			cleared, isString := value.(string)
			require.True(isString, "labels must be a string; JSON null leaves GitLab labels untouched")
			assert.Empty(cleared)

			assert.Empty(tt.stored(t, database, repoID), "assigned label must be removed")
		})
	}
}

func TestGitLabSetPullLabelsRejectsCommaNamesFromCatalog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, repoID, fake := setupGitLabLabelStack(t)
	fake.mu.Lock()
	fake.catalogJSON = `[
		{"id": 4, "name": "bug", "color": "#d73a4a", "description": "Something is broken"},
		{"id": 9, "name": "reviewed,deploy", "color": "#00ff00", "description": "comma label"}
	]`
	fake.mu.Unlock()

	rr := doJSONRequest(t, srv, http.MethodPut, "/api/v1/pulls/gitlab/acme/widget/7/labels", map[string][]string{
		"labels": {"reviewed,deploy"},
	})
	require.Equal(http.StatusBadRequest, rr.Code, "response: %s", rr.Body.String())
	assert.Contains(rr.Body.String(), "comma")

	fake.mu.Lock()
	sent := fake.mrLabelBody
	fake.mu.Unlock()
	assert.Nil(sent, "provider must not receive a label update for a rejected name")

	mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	assert.Empty(mr.Labels, "rejected assignment must not be persisted")
}

// A label whose name equals another label's decimal ID must persist:
// GitLab item labels are name-only, and the label store keys catalog
// rows by decimal ID in platform_external_id. If normalization claimed
// the name as an external ID, label "4" would match label ID 4's row by
// external ID and its own row by name, and the save would fail after
// the provider mutation already succeeded.
func TestGitLabSetPullLabelsNameCollidingWithAnotherLabelID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, database, repoID, fake := setupGitLabLabelStack(t)
	fake.mu.Lock()
	fake.catalogJSON = `[
		{"id": 4, "name": "bug", "color": "#d73a4a", "description": "Something is broken"},
		{"id": 5, "name": "4", "color": "#000000", "description": "named like an ID"}
	]`
	fake.mu.Unlock()

	rr := doJSONRequest(t, srv, http.MethodPut, "/api/v1/pulls/gitlab/acme/widget/7/labels", map[string][]string{
		"labels": {"4"},
	})
	require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

	mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 7)
	require.NoError(err)
	require.NotNil(mr)
	require.Len(mr.Labels, 1)
	assert.Equal("4", mr.Labels[0].Name)
	assert.Equal("#000000", mr.Labels[0].Color)
}

// fakeGitealikeAPI serves the minimal Forgejo/Gitea v1 surface the label
// flows touch: the repo label catalog and issue-style label replacement
// (shared by pull requests and issues). Replacement responses echo the
// catalog entries matching the requested IDs, like the real servers.
type fakeGitealikeAPI struct {
	mu          sync.Mutex
	catalog     []fakeGitealikeLabel
	replaceBody map[int]map[string][]int64
}

type fakeGitealikeLabel struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func (f *fakeGitealikeAPI) labelsForIDs(ids []int64) []fakeGitealikeLabel {
	out := []fakeGitealikeLabel{}
	for _, id := range ids {
		for _, label := range f.catalog {
			if label.ID == id {
				out = append(out, label)
			}
		}
	}
	return out
}

func (f *fakeGitealikeAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/widget/labels":
			f.mu.Lock()
			catalog := f.catalog
			f.mu.Unlock()
			assert.NoError(t, json.NewEncoder(w).Encode(catalog))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/repos/acme/widget/issues/7/labels",
			r.Method == http.MethodPut && r.URL.Path == "/api/v1/repos/acme/widget/issues/11/labels":
			number := 7
			if r.URL.Path == "/api/v1/repos/acme/widget/issues/11/labels" {
				number = 11
			}
			// A pointer distinguishes an explicit empty array (clear
			// all) from JSON null or a missing field, which must not
			// count as a replacement.
			var body struct {
				Labels *[]int64 `json:"labels"`
			}
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if body.Labels == nil {
				http.Error(w, `{"message": "labels must be an array"}`, http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			if f.replaceBody == nil {
				f.replaceBody = make(map[int]map[string][]int64)
			}
			f.replaceBody[number] = map[string][]int64{"labels": *body.Labels}
			replaced := f.labelsForIDs(*body.Labels)
			f.mu.Unlock()
			assert.NoError(t, json.NewEncoder(w).Encode(replaced))
		default:
			http.NotFound(w, r)
		}
	})
}

// gitealikeLabelVariant runs the shared gitealike label flows through a
// concrete provider client so each SDK transport gets full-stack
// coverage, not just the shared adapter.
type gitealikeLabelVariant struct {
	name      string
	kind      platform.Kind
	host      string
	route     string
	newClient func(t *testing.T, upstreamURL string) platform.Provider
}

func gitealikeLabelVariants() []gitealikeLabelVariant {
	return []gitealikeLabelVariant{
		{
			name:  "forgejo",
			kind:  platform.KindForgejo,
			host:  platform.DefaultForgejoHost,
			route: "forgejo",
			newClient: func(t *testing.T, upstreamURL string) platform.Provider {
				t.Helper()
				client, err := forgejo.NewClient(
					platform.DefaultForgejoHost,
					staticTokenSource("token"),
					forgejo.WithBaseURLForTesting(upstreamURL), forgejo.WithTransport(http.DefaultTransport))
				require.NoError(t, err)
				return client
			},
		},
		{
			name:  "gitea",
			kind:  platform.KindGitea,
			host:  platform.DefaultGiteaHost,
			route: "gitea",
			newClient: func(t *testing.T, upstreamURL string) platform.Provider {
				t.Helper()
				client, err := gitea.NewClient(
					platform.DefaultGiteaHost,
					staticTokenSource("token"),
					gitea.WithBaseURL(upstreamURL, true),
					gitea.WithServerVersion("1.26.0"), gitea.WithTransport(http.DefaultTransport))
				require.NoError(t, err)
				return client
			},
		},
	}
}

func setupGitealikeLabelStack(
	t *testing.T,
	variant gitealikeLabelVariant,
) (*server.Server, *db.DB, int64, *fakeGitealikeAPI) {
	t.Helper()
	database := dbtest.Open(t)
	fake := &fakeGitealikeAPI{
		catalog: []fakeGitealikeLabel{
			{ID: 11, Name: "bug", Color: "d73a4a", Description: "Something is broken"},
			{ID: 12, Name: "triage", Color: "fbca04", Description: "Needs review"},
		},
	}
	upstream := httptest.NewServer(fake.handler(t))
	t.Cleanup(upstream.Close)

	client := variant.newClient(t, upstream.URL)

	repoID := seedProviderRepo(t, database, variant.kind, variant.host)
	seedProviderPRAndIssue(t, database, repoID)
	srv := newLabelTestServer(t, database, client, variant.kind, variant.host)
	return srv, database, repoID, fake
}

func TestGitealikeListRepoLabelsSyncsCatalogFromProvider(t *testing.T) {
	for _, variant := range gitealikeLabelVariants() {
		t.Run(variant.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, database, repoID, _ := setupGitealikeLabelStack(t, variant)
			assertRepoLabelCapabilities(t, srv)

			rr := doJSONRequest(t, srv, http.MethodGet, "/api/v1/repo/"+variant.route+"/acme/widget/labels", nil)
			require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

			require.Eventually(func() bool {
				labels, _, err := database.ListRepoLabelCatalog(t.Context(), repoID)
				return err == nil && len(labels) == 2
			}, 2*time.Second, 10*time.Millisecond)

			labels, _, err := database.ListRepoLabelCatalog(t.Context(), repoID)
			require.NoError(err)
			require.Len(labels, 2)
			assert.Equal("bug", labels[0].Name)
			assert.Equal("d73a4a", labels[0].Color)
			assert.Equal("Something is broken", labels[0].Description)
			assert.Equal("triage", labels[1].Name)
		})
	}
}

func TestGitealikeSetPullLabelsResolvesIDsAndUpdatesDB(t *testing.T) {
	for _, variant := range gitealikeLabelVariants() {
		t.Run(variant.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, database, repoID, fake := setupGitealikeLabelStack(t, variant)

			rr := doJSONRequest(t, srv, http.MethodPut, "/api/v1/pulls/"+variant.route+"/acme/widget/7/labels", map[string][]string{
				"labels": {"triage"},
			})
			require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

			var body labelWireResponse
			require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
			require.Len(body.Labels, 1)
			assert.Equal("triage", body.Labels[0].Name)
			assert.Equal("fbca04", body.Labels[0].Color)

			fake.mu.Lock()
			sent := fake.replaceBody[7]
			fake.mu.Unlock()
			require.NotNil(sent, "provider must receive the label replacement")
			assert.Equal([]int64{12}, sent["labels"], "names must be resolved to label IDs")

			mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 7)
			require.NoError(err)
			require.NotNil(mr)
			require.Len(mr.Labels, 1)
			assert.Equal("triage", mr.Labels[0].Name)
			assert.Equal("fbca04", mr.Labels[0].Color)
		})
	}
}

func TestGitealikeSetIssueLabelsResolvesIDsAndUpdatesDB(t *testing.T) {
	for _, variant := range gitealikeLabelVariants() {
		t.Run(variant.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, database, repoID, fake := setupGitealikeLabelStack(t, variant)

			rr := doJSONRequest(t, srv, http.MethodPut, "/api/v1/issues/"+variant.route+"/acme/widget/11/labels", map[string][]string{
				"labels": {"triage"},
			})
			require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

			var body labelWireResponse
			require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
			require.Len(body.Labels, 1)
			assert.Equal("triage", body.Labels[0].Name)
			assert.Equal("fbca04", body.Labels[0].Color)

			fake.mu.Lock()
			sent := fake.replaceBody[11]
			fake.mu.Unlock()
			require.NotNil(sent, "provider must receive the label replacement")
			assert.Equal([]int64{12}, sent["labels"], "names must be resolved to label IDs")

			issue, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 11)
			require.NoError(err)
			require.NotNil(issue)
			require.Len(issue.Labels, 1)
			assert.Equal("triage", issue.Labels[0].Name)
		})
	}
}

// An empty labels array is the API contract for "remove every label":
// starting from an existing assignment, the provider must receive an
// explicit empty ID replacement and the stored assignment must end up
// empty. A missing or null labels field is rejected instead (covered in
// internal/server/apitest).
func TestGitealikeSetLabelsEmptyArrayClearsAll(t *testing.T) {
	items := []struct {
		name   string
		kind   string
		number int
		route  string
		stored func(t *testing.T, database *db.DB, repoID int64) []db.Label
	}{
		{
			name:   "pull",
			kind:   "pull",
			number: 7,
			route:  "pulls",
			stored: func(t *testing.T, database *db.DB, repoID int64) []db.Label {
				t.Helper()
				mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 7)
				require.NoError(t, err)
				require.NotNil(t, mr)
				return mr.Labels
			},
		},
		{
			name:   "issue",
			kind:   "issue",
			number: 11,
			route:  "issues",
			stored: func(t *testing.T, database *db.DB, repoID int64) []db.Label {
				t.Helper()
				issue, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 11)
				require.NoError(t, err)
				require.NotNil(t, issue)
				return issue.Labels
			},
		},
	}
	for _, variant := range gitealikeLabelVariants() {
		for _, item := range items {
			t.Run(variant.name+"/"+item.name, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				srv, database, repoID, fake := setupGitealikeLabelStack(t, variant)
				seedAssignedLabel(t, database, repoID, item.kind, item.number)

				path := "/api/v1/" + item.route + "/" + variant.route + "/acme/widget/" +
					strconv.Itoa(item.number) + "/labels"
				rr := doJSONRequest(t, srv, http.MethodPut, path, map[string][]string{
					"labels": {},
				})
				require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

				var body labelWireResponse
				require.NoError(json.Unmarshal(rr.Body.Bytes(), &body))
				assert.Empty(body.Labels)

				fake.mu.Lock()
				sent, called := fake.replaceBody[item.number]
				fake.mu.Unlock()
				require.True(called, "provider must receive the label replacement")
				assert.Empty(sent["labels"])

				assert.Empty(item.stored(t, database, repoID), "assigned label must be removed")
			})
		}
	}
}

func TestGitealikeSetLabelsFailsWhenCatalogNameVanishedUpstream(t *testing.T) {
	for _, variant := range gitealikeLabelVariants() {
		t.Run(variant.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, database, repoID, fake := setupGitealikeLabelStack(t, variant)

			// The DB catalog knows "ghost" (fresh, so no inline refresh
			// runs) but the provider no longer has it: name-to-ID
			// resolution must fail without touching the assignment
			// endpoint.
			now := time.Now().UTC()
			require.NoError(database.ReplaceRepoLabelCatalog(t.Context(), repoID, []db.Label{
				{Name: "ghost", Color: "ffffff", UpdatedAt: now},
			}, now))

			rr := doJSONRequest(t, srv, http.MethodPut, "/api/v1/issues/"+variant.route+"/acme/widget/11/labels", map[string][]string{
				"labels": {"ghost"},
			})
			require.Equal(http.StatusNotFound, rr.Code, "response: %s", rr.Body.String())
			assert.Contains(rr.Body.String(), "ghost")

			fake.mu.Lock()
			sent := fake.replaceBody[11]
			fake.mu.Unlock()
			assert.Nil(sent, "assignment endpoint must not be called when resolution fails")

			issue, err := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 11)
			require.NoError(err)
			require.NotNil(issue)
			assert.Empty(issue.Labels)
		})
	}
}

package kata

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
)

func TestKataEffectiveLinkHydrationBatchesPerDaemon(t *testing.T) {
	assert := assert.New(t)
	var mu sync.Mutex
	calls := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.10.4"}`))
			return
		}
		if r.URL.Path != "/api/v1/ui/references" {
			http.NotFound(w, r)
			return
		}
		uids := r.URL.Query()["issue_uid"]
		if len(uids) > 200 {
			http.Error(w, "too many UIDs", http.StatusBadRequest)
			return
		}
		mu.Lock()
		calls++
		mu.Unlock()
		issues := make([]kataIssueReference, 0, len(uids))
		for _, uid := range uids {
			issues = append(issues, kataIssueReference{
				UID: uid, ProjectUID: "project-a", QualifiedID: "project-a:" + uid,
				Title: "Task " + uid, Status: "open",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": issues})
	}))
	t.Cleanup(daemon.Close)
	configureKataLinkTestDaemon(t, daemon.URL)
	srv, _ := setupTestServer(t)
	candidates := make(map[string]*kataLinkCandidate)
	for i := range 401 {
		uid := fmt.Sprintf("issue-%03d", i)
		mergeKataLinkCandidate(candidates, "primary", "project-a", uid, kataLinkDirect)
	}

	response := srv.hydrateKataLinkCandidates(t.Context(), candidates)
	mu.Lock()
	observedCalls := calls
	mu.Unlock()

	assert.Equal("complete", response.State)
	assert.Len(response.Links, 401)
	assert.Equal(3, observedCalls)
}

func TestKataEffectiveLinkHydrationLimitsAcrossConcurrentRequests(t *testing.T) {
	tests := []struct {
		name             string
		daemonIDs        []string
		candidateCount   int
		concurrentReads  int
		concurrencyLimit int
	}{
		{
			name: "per daemon", daemonIDs: []string{"primary"}, candidateCount: 401,
			concurrentReads: 3, concurrencyLimit: kataLinkHydrationPerDaemonConcurrency,
		},
		{
			name: "global", daemonIDs: []string{"daemon-a", "daemon-b", "daemon-c", "daemon-d", "daemon-e"},
			candidateCount: 1, concurrentReads: 5, concurrencyLimit: kataLinkHydrationGlobalConcurrency,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			started := make(chan struct{}, tt.concurrentReads*3)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseAll()
			var mu sync.Mutex
			active := 0
			maxActive := 0
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.10.4"}`))
					return
				}
				if r.URL.Path != "/api/v1/ui/references" {
					http.NotFound(w, r)
					return
				}
				mu.Lock()
				active++
				maxActive = max(maxActive, active)
				mu.Unlock()
				started <- struct{}{}
				<-release
				issues := make([]kataIssueReference, 0, len(r.URL.Query()["issue_uid"]))
				for _, uid := range r.URL.Query()["issue_uid"] {
					issues = append(issues, kataIssueReference{
						UID: uid, ProjectUID: "project-a", QualifiedID: "project-a:" + uid,
						Title: "Task " + uid, Status: "open",
					})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"issues": issues})
				mu.Lock()
				active--
				mu.Unlock()
			}))
			t.Cleanup(daemon.Close)

			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			var catalog strings.Builder
			for _, daemonID := range tt.daemonIDs {
				_, _ = fmt.Fprintf(&catalog, "[[daemon]]\nname = %q\nurl = %q\n\n", daemonID, daemon.URL)
			}
			writeKataServerCatalog(t, home, catalog.String())
			srv, _ := setupTestServer(t)

			start := make(chan struct{})
			var reads sync.WaitGroup
			reads.Add(tt.concurrentReads)
			for readIndex := range tt.concurrentReads {
				daemonID := tt.daemonIDs[readIndex%len(tt.daemonIDs)]
				candidates := make(map[string]*kataLinkCandidate)
				for candidateIndex := range tt.candidateCount {
					uid := fmt.Sprintf("issue-%d-%03d", readIndex, candidateIndex)
					mergeKataLinkCandidate(candidates, daemonID, "project-a", uid, kataLinkDirect)
				}
				go func() {
					defer reads.Done()
					<-start
					srv.hydrateKataLinkCandidates(t.Context(), candidates)
				}()
			}
			close(start)
			for range tt.concurrencyLimit {
				select {
				case <-started:
				case <-time.After(time.Second):
					require.FailNow("hydration did not fill the expected shared slots")
				}
			}
			exceeded := false
			select {
			case <-started:
				exceeded = true
			case <-time.After(100 * time.Millisecond):
			}
			mu.Lock()
			observedMaxActive := maxActive
			mu.Unlock()
			releaseAll()
			reads.Wait()

			assert.False(exceeded, "a concurrent request exceeded the shared hydration limit")
			assert.LessOrEqual(observedMaxActive, tt.concurrencyLimit)
		})
	}
}

func TestKataEffectiveLinkHydrationReportsOneDiagnosticPerDaemonFailure(t *testing.T) {
	assert := assert.New(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	configureKataLinkTestDaemon(t, down.URL)
	srv, _ := setupTestServer(t)
	candidates := make(map[string]*kataLinkCandidate)
	for i := range 401 {
		uid := fmt.Sprintf("issue-%03d", i)
		mergeKataLinkCandidate(candidates, "primary", "project-a", uid, kataLinkDirect)
	}

	response := srv.hydrateKataLinkCandidates(t.Context(), candidates)

	assert.Equal("unavailable", response.State)
	assert.Len(response.Links, 401)
	assert.Equal([]kataLinkDiagnostic{{DaemonID: "primary", Reason: "daemon unavailable"}}, response.Diagnostics)
}

func TestKataEffectiveLinkHydrationAcceptsCurrentSchema(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.13.0"}`))
			return
		}
		if r.URL.Path != "/api/v1/ui/references" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": []kataIssueReference{{
			UID: "issue-a", ProjectUID: "project-a", QualifiedID: "project-a:issue-a",
			Title: "Task issue-a", Status: "open",
		}}})
	}))
	t.Cleanup(current.Close)
	configureKataLinkTestDaemon(t, current.URL)
	srv, _ := setupTestServer(t)
	candidates := make(map[string]*kataLinkCandidate)
	mergeKataLinkCandidate(candidates, "primary", "project-a", "issue-a", kataLinkDirect)

	response := srv.hydrateKataLinkCandidates(t.Context(), candidates)

	assert.Equal("complete", response.State)
	require.Len(response.Links, 1)
	assert.Equal("0.13.0", response.Links[0].APISchemaVersion)
	assert.Empty(response.Diagnostics)
}

func TestKataEffectiveLinkHydrationKeepsExistingWorkspaceWhenDaemonUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	configureKataLinkTestDaemon(t, down.URL)
	srv, database := setupTestServer(t)
	require.NoError(database.InsertWorkspace(t.Context(), &db.Workspace{
		ID: "workspace-existing", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget", ItemType: db.WorkspaceItemTypeKataTask,
		ItemKey: db.KataWorkspaceItemKey(db.WorkspaceKataMetadata{
			DaemonID: "primary", ProjectUID: "project-a", IssueUID: "issue-a",
		}),
		GitHeadRef: "task-a", WorkspaceBranch: "task-a", WorktreePath: t.TempDir(),
		TmuxSession: "workspace-existing", Status: "ready",
		KataMetadata: &db.WorkspaceKataMetadata{
			DaemonID: "primary", ProjectUID: "project-a", IssueUID: "issue-a",
		},
	}))
	candidates := make(map[string]*kataLinkCandidate)
	mergeKataLinkCandidate(candidates, "primary", "project-a", "issue-a", kataLinkDirect)

	response := srv.hydrateKataLinkCandidates(t.Context(), candidates)

	assert.Equal("unavailable", response.State)
	require.Len(response.Links, 1)
	require.NotNil(response.Links[0].Workspace)
	assert.True(response.Links[0].Workspace.Available)
	require.NotNil(response.Links[0].Workspace.ExistingWorkspace)
	assert.Equal("workspace-existing", response.Links[0].Workspace.ExistingWorkspace.ID)
}

func TestKataEffectiveLinkHydrationReportsPartialWhenAnyCandidateHydrates(t *testing.T) {
	assert := assert.New(t)
	available := newKataEffectiveLinkTestDaemon(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	writeKataServerCatalog(t, home, `
[[daemon]]
name = "primary"
url = "`+available.URL+`"

[[daemon]]
name = "secondary"
url = "`+down.URL+`"
`)
	srv, _ := setupTestServer(t)
	candidates := make(map[string]*kataLinkCandidate)
	mergeKataLinkCandidate(candidates, "primary", "project-a", "issue-a", kataLinkDirect)
	mergeKataLinkCandidate(candidates, "secondary", "project-b", "issue-b", kataLinkDirect)

	response := srv.hydrateKataLinkCandidates(t.Context(), candidates)

	assert.Equal("partial", response.State)
	assert.Len(response.Links, 2)
	assert.Empty(effectiveLinkByUID(t, response.Links, "issue-a").UnavailableReason)
	assert.Equal("daemon unavailable", effectiveLinkByUID(t, response.Links, "issue-b").UnavailableReason)
}

func TestKataEffectiveLinkHydrationKeepsExistingWorkspaceAfterDaemonTimeout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requestStarted := make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
	}))
	t.Cleanup(daemon.Close)
	configureKataLinkTestDaemon(t, daemon.URL)
	srv, database := setupTestServer(t)
	require.NoError(database.InsertWorkspace(t.Context(), &db.Workspace{
		ID: "workspace-timeout", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget", ItemType: db.WorkspaceItemTypeKataTask,
		ItemKey: db.KataWorkspaceItemKey(db.WorkspaceKataMetadata{
			DaemonID: "primary", ProjectUID: "project-a", IssueUID: "issue-timeout",
		}),
		GitHeadRef: "task-timeout", WorkspaceBranch: "task-timeout", WorktreePath: t.TempDir(),
		TmuxSession: "workspace-timeout", Status: "ready",
		KataMetadata: &db.WorkspaceKataMetadata{
			DaemonID: "primary", ProjectUID: "project-a", IssueUID: "issue-timeout",
		},
	}))
	candidates := make(map[string]*kataLinkCandidate)
	mergeKataLinkCandidate(candidates, "primary", "project-a", "issue-timeout", kataLinkDirect)

	startedAt := time.Now()
	response := srv.hydrateKataLinkCandidates(t.Context(), candidates)

	select {
	case <-requestStarted:
	default:
		require.Fail("daemon request did not start")
	}
	assert.GreaterOrEqual(time.Since(startedAt), kataLinkHydrationTimeout)
	assert.Equal("unavailable", response.State)
	require.Len(response.Links, 1)
	require.NotNil(response.Links[0].Workspace)
	require.NotNil(response.Links[0].Workspace.ExistingWorkspace)
	assert.Equal("workspace-timeout", response.Links[0].Workspace.ExistingWorkspace.ID)
}

func TestKataEffectiveLinkHydrationReportsWorkspaceResolutionFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	daemon := newKataEffectiveLinkTestDaemon(t)
	configureKataLinkTestDaemon(t, daemon.URL)
	srv, database := setupTestServer(t)
	_, err := database.WriteDB().ExecContext(t.Context(), `DROP TABLE forge_projects`)
	require.NoError(err)
	candidates := make(map[string]*kataLinkCandidate)
	mergeKataLinkCandidate(candidates, "primary", "project-a", "issue-a", kataLinkDirect)

	response := srv.hydrateKataLinkCandidates(t.Context(), candidates)

	assert.Equal("partial", response.State)
	require.Len(response.Links, 1)
	encoded, err := json.Marshal(response.Links[0])
	require.NoError(err)
	var wire struct {
		Workspace *struct {
			Available         bool   `json:"available"`
			ResolutionStatus  string `json:"resolution_status"`
			UnavailableReason string `json:"unavailable_reason"`
		} `json:"workspace"`
	}
	require.NoError(json.Unmarshal(encoded, &wire))
	require.NotNil(wire.Workspace)
	assert.False(wire.Workspace.Available)
	assert.Equal("error", wire.Workspace.ResolutionStatus)
	assert.Equal(
		"Forge could not resolve the workspace repository. Retry, then check the server logs if it continues.",
		wire.Workspace.UnavailableReason,
	)
}

func TestKataEffectiveWorkspaceLinksMergeIntrinsicDirectAndInheritedProvenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	daemon := newKataEffectiveLinkTestDaemon(t)
	configureKataLinkTestDaemon(t, daemon.URL)
	srv, database := setupTestServer(t)

	repoID, err := database.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform: string(platform.KindGitHub), PlatformHost: platform.DefaultGitHubHost,
		PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID: repoID, PlatformID: 7, PlatformExternalID: "pull-external-7",
		Number: 7, URL: "https://github.com/acme/widget/pull/7",
		Title: "Pull", Author: "user", State: "open", HeadBranch: "feature", BaseBranch: "main",
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(err)
	associatedPR := 7
	require.NoError(database.InsertWorkspace(t.Context(), &db.Workspace{
		ID: "workspace-a", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget", ItemType: db.WorkspaceItemTypeKataTask,
		ItemKey: db.KataWorkspaceItemKey(db.WorkspaceKataMetadata{
			DaemonID: "primary", ProjectUID: "project-a", IssueUID: "issue-a",
		}),
		AssociatedPRNumber: &associatedPR, GitHeadRef: "feature", WorkspaceBranch: "feature",
		WorktreePath: t.TempDir(), TmuxSession: "workspace-a", Status: "ready",
		KataMetadata: &db.WorkspaceKataMetadata{
			DaemonID: "primary", ProjectUID: "project-a", ProjectName: "Project A",
			IssueUID: "issue-a", ShortID: "A-1", QualifiedID: "project-a:A-1", Title: "Task A",
		},
	}))

	workspaceSubject := db.KataLinkSubject{Kind: db.KataLinkSubjectWorkspace, WorkspaceID: "workspace-a"}
	direct, err := database.CreateKataIssueLink(t.Context(), db.KataIssueLink{
		Subject: workspaceSubject, DaemonID: "primary", ProjectUID: "project-a", IssueUID: "issue-a",
	})
	require.NoError(err)
	_, err = database.CreateKataIssueLink(t.Context(), db.KataIssueLink{
		Subject: workspaceSubject, DaemonID: "primary", ProjectUID: "project-a", IssueUID: "issue-c",
	})
	require.NoError(err)
	pullSubject := db.KataLinkSubject{
		Kind: db.KataLinkSubjectPullRequest, RepoID: repoID,
		ProviderItemExternalID: "pull-external-7",
	}
	_, err = database.CreateKataIssueLink(t.Context(), db.KataIssueLink{
		Subject: pullSubject, DaemonID: "primary", ProjectUID: "project-a", IssueUID: "issue-a",
	})
	require.NoError(err)
	_, err = database.CreateKataIssueLink(t.Context(), db.KataIssueLink{
		Subject: pullSubject, DaemonID: "primary", ProjectUID: "project-a", IssueUID: "issue-b",
	})
	require.NoError(err)

	rr := doJSON(t, srv, http.MethodGet, "/api/v1/workspaces/workspace-a/kata-links", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	body := decodeKataEffectiveLinks(t, rr)
	assert.Equal("complete", body.State)
	require.Len(body.Links, 3)
	assert.Equal([]string{"issue-a", "issue-b", "issue-c"}, effectiveLinkUIDs(body.Links))

	merged := effectiveLinkByUID(t, body.Links, "issue-a")
	assert.Equal([]kataLinkProvenance{kataLinkIntrinsic, kataLinkDirect, kataLinkInherited}, merged.Provenance)
	require.NotNil(merged.DirectLinkID)
	assert.Equal(direct.ID, *merged.DirectLinkID)
	assert.Equal("project-a:A-1", merged.Reference)
	assert.Equal("Project A", merged.ProjectName)
	assert.Equal([]kataLinkProvenance{kataLinkInherited}, effectiveLinkByUID(t, body.Links, "issue-b").Provenance)
	assert.Equal([]kataLinkProvenance{kataLinkDirect}, effectiveLinkByUID(t, body.Links, "issue-c").Provenance)
	created := doJSON(t, srv, http.MethodPost, "/api/v1/workspaces/workspace-a/kata-links", map[string]any{
		"daemon_id": "primary", "project_uid": "project-a", "issue_uid": "issue-b",
	})
	require.Equal(http.StatusOK, created.Code, created.Body.String())
	createdLink := effectiveLinkByUID(t, decodeKataEffectiveLinks(t, created).Links, "issue-b")
	assert.Equal([]kataLinkProvenance{kataLinkDirect, kataLinkInherited}, createdLink.Provenance)
	require.NotNil(createdLink.DirectLinkID)

	deleted := doJSON(t, srv, http.MethodDelete,
		"/api/v1/workspaces/workspace-a/kata-links/"+jsonNumber(direct.ID), nil)
	require.Equal(http.StatusNoContent, deleted.Code, deleted.Body.String())
	rr = doJSON(t, srv, http.MethodGet, "/api/v1/workspaces/workspace-a/kata-links", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	merged = effectiveLinkByUID(t, decodeKataEffectiveLinks(t, rr).Links, "issue-a")
	assert.Equal([]kataLinkProvenance{kataLinkIntrinsic, kataLinkInherited}, merged.Provenance)
	assert.Nil(merged.DirectLinkID)
}

func TestKataWorkspaceLinksDoNotInheritAcrossReusedRepositoryRoute(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, database := setupTestServer(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)

	originalRepoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-original",
		Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	_, err = database.UpsertIssue(ctx, &db.Issue{
		RepoID: originalRepoID, PlatformID: 1, PlatformExternalID: "issue-original-1",
		Number: 1, URL: "https://github.com/acme/widget/issues/1", Title: "Original issue",
		Author: "user", State: "open", CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(err)
	require.NoError(database.InsertWorkspace(ctx, &db.Workspace{
		ID: "workspace-route-reuse", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget", ItemType: db.WorkspaceItemTypeIssue,
		ItemNumber: 1, GitHeadRef: "issue-1", WorkspaceBranch: "issue-1",
		WorktreePath: t.TempDir(), TmuxSession: "workspace-route-reuse", Status: "ready",
	}))

	_, err = database.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-original",
		Owner: "acme", Name: "widget-renamed",
	})
	require.NoError(err)
	replacementRepoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-replacement",
		Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	_, err = database.UpsertIssue(ctx, &db.Issue{
		RepoID: replacementRepoID, PlatformID: 1, PlatformExternalID: "issue-replacement-1",
		Number: 1, URL: "https://github.com/acme/widget/issues/1", Title: "Replacement issue",
		Author: "user", State: "open", CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(err)
	_, err = database.CreateKataIssueLink(ctx, db.KataIssueLink{
		Subject: db.KataLinkSubject{
			Kind: db.KataLinkSubjectIssue, RepoID: replacementRepoID,
			ProviderItemExternalID: "issue-replacement-1",
		},
		DaemonID: "primary", ProjectUID: "project-a", IssueUID: "issue-a",
	})
	require.NoError(err)

	rr := doJSON(t, srv, http.MethodGet, "/api/v1/workspaces/workspace-route-reuse/kata-links", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	body := decodeKataEffectiveLinks(t, rr)
	assert.Equal("complete", body.State)
	assert.Empty(body.Links)
}

func TestWorkspaceKataInheritanceIncludesOwningIssueAndAssociatedPull(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, database := setupTestServer(t)
	insertKataProviderSubject(
		t, database, platform.KindGitHub, platform.DefaultGitHubHost,
		"widget", 42, db.KataLinkSubjectIssue, "issue-external-42",
	)
	insertKataProviderSubject(
		t, database, platform.KindGitHub, platform.DefaultGitHubHost,
		"widget", 7, db.KataLinkSubjectPullRequest, "pull-external-7",
	)
	associatedPR := 7

	subjects, err := srv.workspaceInheritedKataSubjects(t.Context(), &db.Workspace{
		Platform: "github", PlatformHost: "github.com", RepoOwner: "acme", RepoName: "widget",
		ItemType: db.WorkspaceItemTypeIssue, ItemNumber: 42, AssociatedPRNumber: &associatedPR,
	})

	require.NoError(err)
	require.Len(subjects, 2)
	assert.Equal(db.KataLinkSubjectIssue, subjects[0].Kind)
	assert.Equal("issue-external-42", subjects[0].ProviderItemExternalID)
	assert.Equal(db.KataLinkSubjectPullRequest, subjects[1].Kind)
	assert.Equal("pull-external-7", subjects[1].ProviderItemExternalID)
}

func newKataEffectiveLinkTestDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.10.4"}`))
		case "/api/v1/ui/references":
			_, _ = w.Write([]byte(`{"issues":[
				{"uid":"issue-a","project_uid":"project-a","project_name":"Project A","short_id":"A-1","qualified_id":"project-a:A-1","title":"Task A","status":"open"},
				{"uid":"issue-b","project_uid":"project-a","project_name":"Project A","short_id":"A-2","qualified_id":"project-a:A-2","title":"Task B","status":"open"},
				{"uid":"issue-c","project_uid":"project-a","project_name":"Project A","short_id":"A-3","qualified_id":"project-a:A-3","title":"Task C","status":"closed"}
			]}`))
		case "/api/v1/issues/issue-b":
			_, _ = w.Write([]byte(`{"issue":{"uid":"issue-b","project_uid":"project-a","title":"Task B"},"comments":[],"links":[],"labels":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func effectiveLinkUIDs(links []kataEffectiveLink) []string {
	uids := make([]string, 0, len(links))
	for _, link := range links {
		uids = append(uids, link.IssueUID)
	}
	return uids
}

func effectiveLinkByUID(t *testing.T, links []kataEffectiveLink, uid string) kataEffectiveLink {
	t.Helper()
	for _, link := range links {
		if link.IssueUID == uid {
			return link
		}
	}
	require.FailNow(t, "effective Kata link not found", uid)
	return kataEffectiveLink{}
}

func jsonNumber(value int64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

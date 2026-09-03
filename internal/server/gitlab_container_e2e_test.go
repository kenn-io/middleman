package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/compose"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/platform"
	platformgitlab "go.kenn.io/forge/internal/platform/gitlab"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

type gitLabContainerManifest struct {
	BaseURL           string `json:"base_url"`
	APIURL            string `json:"api_url"`
	Host              string `json:"host"`
	Token             string `json:"token"`
	Owner             string `json:"owner"`
	Name              string `json:"name"`
	RepoPath          string `json:"repo_path"`
	WebURL            string `json:"web_url"`
	CloneURL          string `json:"clone_url"`
	DefaultBranch     string `json:"default_branch"`
	ProjectID         int64  `json:"project_id"`
	ProjectExternalID string `json:"project_external_id"`
	MergeRequestIID   int    `json:"merge_request_iid"`
	IssueIID          int    `json:"issue_iid"`
	Label             string `json:"label"`
	ReleaseTag        string `json:"release_tag"`
}

func TestGitLabContainerE2E(t *testing.T) {
	if os.Getenv("KENN_FORGE_GITLAB_CONTAINER_E2E") != "1" {
		t.Skip("set KENN_FORGE_GITLAB_CONTAINER_E2E=1 to run GitLab CE container e2e")
	}

	assert := assert.New(t)
	require := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()

	image := envOrDefault("KENN_FORGE_GITLAB_IMAGE", "gitlab/gitlab-ce:19.1.1-ce.0")
	rootPassword := envOrDefault("GITLAB_ROOT_PASSWORD", "V9q!T3m#R7p-L2x@N6s")
	httpPort := envOrDefault("GITLAB_HTTP_PORT", freeLoopbackPort(t))
	stackID := compose.StackIdentifier(envOrDefault("KENN_FORGE_GITLAB_COMPOSE_PROJECT", "kenn-forge-gitlab-e2e"))
	stack, err := compose.NewDockerComposeWith(
		compose.WithStackFiles(filepath.Join(repoRoot(t), "scripts/e2e/gitlab/docker-compose.yml")),
		stackID,
	)
	require.NoError(err)

	composeStack := stack.
		WithEnv(map[string]string{
			"KENN_FORGE_GITLAB_IMAGE": image,
			"GITLAB_ROOT_PASSWORD":    rootPassword,
			"GITLAB_HTTP_PORT":        httpPort,
		}).
		WaitForService("gitlab", wait.ForHTTP("/users/sign_in").
			WithPort("80/tcp").
			WithStartupTimeout(20*time.Minute).
			WithStatusCodeMatcher(func(status int) bool {
				return status == http.StatusOK
			}).
			WithResponseHeadersMatcher(func(headers http.Header) bool {
				return headers.Get("X-Gitlab-Meta") != ""
			}))
	err = composeStack.Up(ctx, compose.Wait(true))
	container, containerErr := composeStack.ServiceContainer(ctx, "gitlab")
	if err != nil {
		if containerErr == nil {
			require.NoError(err, containerLogs(ctx, container))
		}
		require.NoError(err)
	}
	require.NoError(containerErr)
	if os.Getenv("KENN_FORGE_KEEP_GITLAB_FIXTURE") == "1" {
		t.Logf("keeping GitLab Compose stack %s at http://127.0.0.1:%s", stackID, httpPort)
	} else {
		t.Cleanup(func() {
			assert.NoError(composeStack.Down(context.Background(), compose.RemoveOrphans(true)))
		})
	}

	baseURL, err := container.PortEndpoint(ctx, "80/tcp", "http")
	require.NoError(err)

	manifestPath := filepath.Join(t.TempDir(), "gitlab-manifest.json")
	cmd := procutil.CommandContext(
		ctx,
		filepath.Join(repoRoot(t), "scripts/e2e/gitlab/bootstrap.sh"),
		manifestPath,
	)
	cmd.Env = append(os.Environ(),
		"GITLAB_URL="+baseURL,
		"GITLAB_ROOT_PASSWORD="+rootPassword,
		"GITLAB_CONTAINER_ID="+container.GetContainerID(),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		require.NoError(err, string(output)+"\n"+containerLogs(ctx, container))
	}

	manifestFile, err := os.Open(manifestPath)
	require.NoError(err)
	defer manifestFile.Close()
	var manifest gitLabContainerManifest
	require.NoError(json.NewDecoder(manifestFile).Decode(&manifest))

	client, err := platformgitlab.NewClient(
		manifest.Host,
		testTokenSource(manifest.Token),
		platformgitlab.WithBaseURLForTesting(manifest.APIURL),
		platformgitlab.WithForegroundTimeoutForTesting(time.Minute),
	)
	require.NoError(err)

	imageBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	var uploadBody bytes.Buffer
	uploadWriter := multipart.NewWriter(&uploadBody)
	uploadPart, err := uploadWriter.CreateFormFile("file", "private-markdown-image.png")
	require.NoError(err)
	_, err = uploadPart.Write(imageBytes)
	require.NoError(err)
	require.NoError(uploadWriter.Close())
	uploadReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		fmt.Sprintf("%s/projects/%d/uploads", manifest.APIURL, manifest.ProjectID),
		&uploadBody,
	)
	require.NoError(err)
	uploadReq.Header.Set("PRIVATE-TOKEN", manifest.Token)
	uploadReq.Header.Set("Content-Type", uploadWriter.FormDataContentType())
	uploadResp, err := http.DefaultClient.Do(uploadReq)
	require.NoError(err)
	uploadRaw, err := io.ReadAll(io.LimitReader(uploadResp.Body, 1<<20))
	uploadResp.Body.Close()
	require.NoError(err)
	require.Less(uploadResp.StatusCode, 300, string(uploadRaw))
	var upload struct {
		FullPath string `json:"full_path"`
	}
	require.NoError(json.Unmarshal(uploadRaw, &upload), string(uploadRaw))
	require.NotEmpty(upload.FullPath)
	attachmentURL := manifest.BaseURL + upload.FullPath
	anonymousReq, err := http.NewRequestWithContext(ctx, http.MethodGet, attachmentURL, nil)
	require.NoError(err)
	anonymousResp, err := http.DefaultClient.Do(anonymousReq)
	require.NoError(err)
	anonymousResp.Body.Close()
	assert.GreaterOrEqual(anonymousResp.StatusCode, 400)
	markdownImage, err := client.GetMarkdownImage(ctx, platform.RepoRef{
		Platform: platform.KindGitLab, Host: manifest.Host,
		RepoPath: manifest.RepoPath, PlatformID: manifest.ProjectID,
	}, attachmentURL)
	require.NoError(err)
	assert.Equal("image/png", markdownImage.ContentType)
	assert.Equal(imageBytes, markdownImage.Content)

	registry, err := platform.NewRegistry(client)
	require.NoError(err)

	database := dbtest.Open(t)
	repo := ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		PlatformHost:       manifest.Host,
		Owner:              manifest.Owner,
		Name:               manifest.Name,
		RepoPath:           manifest.RepoPath,
		PlatformRepoID:     manifest.ProjectID,
		PlatformExternalID: manifest.ProjectExternalID,
		WebURL:             manifest.WebURL,
		CloneURL:           manifest.CloneURL,
		DefaultBranch:      manifest.DefaultBranch,
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	syncer.RunOnce(ctx)
	require.NoError(syncer.SyncMR(ctx, manifest.Owner, manifest.Name, manifest.MergeRequestIID))
	require.NoError(syncer.SyncIssue(ctx, manifest.Owner, manifest.Name, manifest.IssueIID))

	repoRow, err := database.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   manifest.Host,
		PlatformRepoID: manifest.ProjectExternalID,
		Owner:          manifest.Owner,
		Name:           manifest.Name,
		RepoPath:       manifest.RepoPath,
	})
	require.NoError(err)
	require.NotNil(repoRow)
	assert.Equal(manifest.RepoPath, repoRow.RepoPath)

	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoRow.ID, manifest.MergeRequestIID)
	require.NoError(err)
	require.NotNil(mr)
	assert.Equal("GitLab container MR", mr.Title)
	require.NotEmpty(mr.Labels)
	assert.Equal(manifest.Label, mr.Labels[0].Name)
	mrEvents, err := database.ListMREvents(ctx, mr.ID)
	require.NoError(err)
	assert.NotEmpty(mrEvents)

	issue, err := database.GetIssueByRepoIDAndNumber(ctx, repoRow.ID, manifest.IssueIID)
	require.NoError(err)
	require.NotNil(issue)
	assert.Equal("GitLab container issue", issue.Title)
	issueEvents, err := database.ListIssueEvents(ctx, issue.ID)
	require.NoError(err)
	assert.NotEmpty(issueEvents)
	referencedIssues, err := database.ListIssues(ctx, db.ListIssuesOpts{ReferencedByPR: true})
	require.NoError(err)
	require.Len(referencedIssues, 1)
	assert.Equal(manifest.IssueIID, referencedIssues[0].Number)

	summaries, err := database.ListRepoSummaries(ctx)
	require.NoError(err)
	require.Len(summaries, 1)
	require.NotNil(summaries[0].Overview.LatestRelease)
	assert.Equal(manifest.ReleaseTag, summaries[0].Overview.LatestRelease.TagName)

	// Write surface: drive every GitLab mutation through kenn-forge's HTTP
	// API against the live container.
	srv := New(database, syncer, nil, "/", nil, ServerOptions{})
	t.Cleanup(func() { gracefulShutdown(t, srv) })

	encodedOwner := url.PathEscape(manifest.Owner)
	pullBase := fmt.Sprintf(
		"/api/v1/host/%s/pulls/gl/%s/%s", manifest.Host, encodedOwner, manifest.Name,
	)
	issueBase := fmt.Sprintf(
		"/api/v1/host/%s/issues/gl/%s/%s", manifest.Host, encodedOwner, manifest.Name,
	)
	seededMR := fmt.Sprintf("%s/%d", pullBase, manifest.MergeRequestIID)
	seededIssue := fmt.Sprintf("%s/%d", issueBase, manifest.IssueIID)
	runID := time.Now().UnixNano()

	// Comment post and edit on the seeded MR.
	commentRR := testutil.DoJSON(t, srv, http.MethodPost, seededMR+"/comments", map[string]string{
		"body": fmt.Sprintf("Write parity comment %d", runID),
	})

	require.Equal(http.StatusCreated, commentRR.Code, commentRR.Body.String())
	var commentEvent struct {
		PlatformID *int64
		Author     string
		Body       string
	}
	require.NoError(json.NewDecoder(commentRR.Body).Decode(&commentEvent))
	require.NotNil(commentEvent.PlatformID)
	assert.Equal("root", commentEvent.Author)

	editedBody := fmt.Sprintf("Write parity comment %d (edited)", runID)
	editRR := testutil.DoJSON(
		t, srv, http.MethodPatch,
		fmt.Sprintf("%s/comments/%d", seededMR, *commentEvent.PlatformID),
		map[string]string{"body": editedBody})

	require.Equal(http.StatusOK, editRR.Code, editRR.Body.String())
	editedNote := gitlabContainerAPI(
		t, ctx, manifest, http.MethodGet,
		fmt.Sprintf("/projects/%d/merge_requests/%d/notes/%d",
			manifest.ProjectID, manifest.MergeRequestIID, *commentEvent.PlatformID),
		nil,
	)
	assert.Equal(editedBody, editedNote["body"])

	// MR description edit (title is left alone so bootstrap stays idempotent).
	editedDescription := fmt.Sprintf("Description updated by write parity e2e %d", runID)
	mrEditRR := testutil.DoJSON(t, srv, http.MethodPatch, seededMR, map[string]string{
		"body": editedDescription,
	})

	require.Equal(http.StatusOK, mrEditRR.Code, mrEditRR.Body.String())
	editedMR := gitlabContainerAPI(t, ctx, manifest, http.MethodGet,
		fmt.Sprintf("/projects/%d/merge_requests/%d", manifest.ProjectID, manifest.MergeRequestIID),
		nil,
	)
	assert.Equal(editedDescription, editedMR["description"])

	// Close and reopen the seeded MR.
	closeRR := testutil.DoJSON(t, srv, http.MethodPost, seededMR+"/github-state", map[string]string{"state": "closed"})
	require.Equal(http.StatusOK, closeRR.Code, closeRR.Body.String())
	assert.Equal("closed", gitlabContainerMRState(t, ctx, manifest, manifest.MergeRequestIID))
	reopenRR := testutil.DoJSON(t, srv, http.MethodPost, seededMR+"/github-state", map[string]string{"state": "open"})
	require.Equal(http.StatusOK, reopenRR.Code, reopenRR.Body.String())
	assert.Equal("opened", gitlabContainerMRState(t, ctx, manifest, manifest.MergeRequestIID))

	// Issue comment, close, and reopen on the seeded issue, each verified
	// against GitLab's own API.
	issueCommentBody := fmt.Sprintf("Issue write parity comment %d", runID)
	issueCommentRR := testutil.DoJSON(t, srv, http.MethodPost, seededIssue+"/comments", map[string]string{
		"body": issueCommentBody,
	})

	require.Equal(http.StatusCreated, issueCommentRR.Code, issueCommentRR.Body.String())
	var issueCommentEvent struct {
		PlatformID *int64
	}
	require.NoError(json.NewDecoder(issueCommentRR.Body).Decode(&issueCommentEvent))
	require.NotNil(issueCommentEvent.PlatformID)
	issueNote := gitlabContainerAPI(t, ctx, manifest, http.MethodGet,
		fmt.Sprintf("/projects/%d/issues/%d/notes/%d",
			manifest.ProjectID, manifest.IssueIID, *issueCommentEvent.PlatformID),
		nil,
	)
	assert.Equal(issueCommentBody, issueNote["body"])

	issueCloseRR := testutil.DoJSON(t, srv, http.MethodPost, seededIssue+"/github-state", map[string]string{"state": "closed"})
	require.Equal(http.StatusOK, issueCloseRR.Code, issueCloseRR.Body.String())
	assert.Equal("closed", gitlabContainerIssueState(t, ctx, manifest, manifest.IssueIID))
	issueReopenRR := testutil.DoJSON(t, srv, http.MethodPost, seededIssue+"/github-state", map[string]string{"state": "open"})
	require.Equal(http.StatusOK, issueReopenRR.Code, issueReopenRR.Body.String())
	assert.Equal("opened", gitlabContainerIssueState(t, ctx, manifest, manifest.IssueIID))

	// Issue create plus title/body edit, verified upstream.
	createdIssueTitle := fmt.Sprintf("Write parity issue %d", runID)
	createIssueRR := testutil.DoJSON(t, srv, http.MethodPost, issueBase, map[string]string{
		"title": createdIssueTitle,
		"body":  "Issue created through kenn-forge against the GitLab container.",
	})

	require.Equal(http.StatusCreated, createIssueRR.Code, createIssueRR.Body.String())
	var createdIssue struct {
		Number int
	}
	require.NoError(json.NewDecoder(createIssueRR.Body).Decode(&createdIssue))
	require.Positive(createdIssue.Number)
	upstreamIssue := gitlabContainerAPI(t, ctx, manifest, http.MethodGet,
		fmt.Sprintf("/projects/%d/issues/%d", manifest.ProjectID, createdIssue.Number), nil)
	assert.Equal(createdIssueTitle, upstreamIssue["title"])
	assert.Equal(
		"Issue created through kenn-forge against the GitLab container.",
		upstreamIssue["description"],
	)

	editedIssueTitle := fmt.Sprintf("Write parity issue %d (edited)", runID)
	issueEditRR := testutil.DoJSON(
		t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", issueBase, createdIssue.Number),
		map[string]string{"title": editedIssueTitle})

	require.Equal(http.StatusOK, issueEditRR.Code, issueEditRR.Body.String())
	upstreamIssue = gitlabContainerAPI(t, ctx, manifest, http.MethodGet,
		fmt.Sprintf("/projects/%d/issues/%d", manifest.ProjectID, createdIssue.Number), nil)
	assert.Equal(editedIssueTitle, upstreamIssue["title"])

	// request_changes has no GitLab equivalent and must fail with the typed
	// capability envelope.
	rejectRR := testutil.DoJSON(t, srv, http.MethodPost, seededMR+"/review-draft/publish", map[string]string{
		"action": "request_changes", "body": "needs work",
	})

	require.Equal(http.StatusConflict, rejectRR.Code, rejectRR.Body.String())
	var rejectProblem struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.NewDecoder(rejectRR.Body).Decode(&rejectProblem))
	assert.Equal("unsupportedCapability", rejectProblem.Code)
	require.NotNil(rejectProblem.Details)
	assert.Equal("review_action_request_changes", rejectProblem.Details["capability"])

	// A dedicated MR for approve, discussion, and merge so the seeded MR
	// stays open for repeated warm-volume runs.
	branch := fmt.Sprintf("feature/write-parity-%d", runID)
	gitlabContainerAPI(t, ctx, manifest, http.MethodPost,
		fmt.Sprintf("/projects/%d/repository/branches?branch=%s&ref=main",
			manifest.ProjectID, url.QueryEscape(branch)),
		map[string]any{},
	)
	gitlabContainerAPI(t, ctx, manifest, http.MethodPost,
		fmt.Sprintf("/projects/%d/repository/files/%s",
			manifest.ProjectID, url.PathEscape(fmt.Sprintf("docs/write-parity-%d.txt", runID))),
		map[string]any{
			"branch":         branch,
			"content":        "write parity fixture\n",
			"commit_message": "Add write parity fixture file",
		},
	)
	createdMR := gitlabContainerAPI(t, ctx, manifest, http.MethodPost,
		fmt.Sprintf("/projects/%d/merge_requests", manifest.ProjectID),
		map[string]any{
			"source_branch": branch,
			"target_branch": "main",
			"title":         fmt.Sprintf("Write parity MR %d", runID),
		},
	)
	mergeIID := int(createdMR["iid"].(float64))
	require.Positive(mergeIID)
	require.NoError(syncer.SyncMR(ctx, manifest.Owner, manifest.Name, mergeIID))
	parityMR := fmt.Sprintf("%s/%d", pullBase, mergeIID)

	// Head-bound mutations require the client to pin the head it reviewed;
	// read the freshly synced head the way a real client would.
	parityHead := gitlabContainerAPI(t, ctx, manifest, http.MethodGet,
		fmt.Sprintf("/projects/%d/merge_requests/%d", manifest.ProjectID, mergeIID), nil)
	parityHeadSHA, _ := parityHead["sha"].(string)
	require.NotEmpty(parityHeadSHA)

	// Approve through the approvals API (body becomes a regular note).
	approveRR := testutil.DoJSON(t, srv, http.MethodPost, parityMR+"/approve", map[string]string{
		"body":              "Approving from kenn-forge write parity e2e",
		"expected_head_sha": parityHeadSHA,
	})

	require.Equal(http.StatusOK, approveRR.Code, approveRR.Body.String())
	approvalState := gitlabContainerAPI(t, ctx, manifest, http.MethodGet,
		fmt.Sprintf("/projects/%d/merge_requests/%d/approvals", manifest.ProjectID, mergeIID),
		nil,
	)
	approvedBy, _ := approvalState["approved_by"].([]any)
	assert.NotEmpty(approvedBy, "approvals API should record the kenn-forge approval")

	// Discussion reply and resolve/unresolve on a resolvable thread.
	discussion := gitlabContainerAPI(t, ctx, manifest, http.MethodPost,
		fmt.Sprintf("/projects/%d/merge_requests/%d/discussions", manifest.ProjectID, mergeIID),
		map[string]any{"body": "Thread started for write parity e2e"},
	)
	discussionID, _ := discussion["id"].(string)
	require.NotEmpty(discussionID)

	replyRR := testutil.DoJSON(t, srv, http.MethodPost,
		fmt.Sprintf("%s/discussions/%s/reply", parityMR, discussionID),
		map[string]string{"body": "Reply sent through kenn-forge"})

	require.Equal(http.StatusCreated, replyRR.Code, replyRR.Body.String())
	repliedDiscussion := gitlabContainerAPI(t, ctx, manifest, http.MethodGet,
		fmt.Sprintf("/projects/%d/merge_requests/%d/discussions/%s",
			manifest.ProjectID, mergeIID, discussionID),
		nil,
	)
	replyNotes, _ := repliedDiscussion["notes"].([]any)
	require.Len(replyNotes, 2, "discussion should contain the seed note and the reply")
	lastNote, _ := replyNotes[1].(map[string]any)
	assert.Equal("Reply sent through kenn-forge", lastNote["body"])

	resolveRR := testutil.DoJSON(t, srv, http.MethodPost,
		fmt.Sprintf("%s/discussions/%s/resolve", parityMR, discussionID),
		map[string]bool{"resolved": true})

	require.Equal(http.StatusOK, resolveRR.Code, resolveRR.Body.String())
	assert.True(gitlabContainerDiscussionResolved(t, ctx, manifest, mergeIID, discussionID))

	unresolveRR := testutil.DoJSON(t, srv, http.MethodPost,
		fmt.Sprintf("%s/discussions/%s/resolve", parityMR, discussionID),
		map[string]bool{"resolved": false})

	require.Equal(http.StatusOK, unresolveRR.Code, unresolveRR.Body.String())
	assert.False(gitlabContainerDiscussionResolved(t, ctx, manifest, mergeIID, discussionID))

	// Rebase cannot be honored per merge on GitLab and must fail typed,
	// without merging anything.
	rebaseRR := testutil.DoJSON(t, srv, http.MethodPost, parityMR+"/merge", map[string]string{
		"method": "rebase", "commit_title": "t", "commit_message": "m",
		"expected_head_sha": parityHeadSHA,
	})

	require.Equal(http.StatusConflict, rebaseRR.Code, rebaseRR.Body.String())
	var rebaseProblem struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	require.NoError(json.NewDecoder(rebaseRR.Body).Decode(&rebaseProblem))
	assert.Equal("unsupportedCapability", rebaseProblem.Code)
	require.NotNil(rebaseProblem.Details)
	assert.Equal("merge_method_rebase", rebaseProblem.Details["capability"])
	assert.Equal("opened", gitlabContainerMRState(t, ctx, manifest, mergeIID))

	// Squash merge once GitLab finishes its async mergeability check.
	waitForGitLabMergeable(t, ctx, manifest, mergeIID)
	mergeRR := testutil.DoJSON(t, srv, http.MethodPost, parityMR+"/merge", map[string]string{
		"method":            "squash",
		"commit_title":      fmt.Sprintf("Write parity squash merge %d", runID),
		"commit_message":    "Squash merged through kenn-forge against the GitLab container",
		"expected_head_sha": parityHeadSHA,
	})

	require.Equal(http.StatusOK, mergeRR.Code, mergeRR.Body.String())
	var mergeResult struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	require.NoError(json.NewDecoder(mergeRR.Body).Decode(&mergeResult))
	assert.True(mergeResult.Merged)
	assert.NotEmpty(mergeResult.SHA)
	assert.Equal("merged", gitlabContainerMRState(t, ctx, manifest, mergeIID))
}

// gitlabContainerAPI issues a raw request against the container's GitLab
// REST API for fixture setup and out-of-band verification of kenn-forge's
// mutations. Payload nil sends no body; responses must be JSON objects.
func gitlabContainerAPI(
	t *testing.T,
	ctx context.Context,
	manifest gitLabContainerManifest,
	method, path string,
	payload map[string]any,
) map[string]any {
	t.Helper()
	require := require.New(t)

	var encoded []byte
	if payload != nil {
		var err error
		encoded, err = json.Marshal(payload)
		require.NoError(err)
	}

	// GitLab CE serves a transient 502 waiting page while puma workers
	// warm up, even after the compose health check passes; retry 5xx.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, manifest.APIURL+path, body)
		require.NoError(err)
		req.Header.Set("PRIVATE-TOKEN", manifest.Token)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(err)
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		require.NoError(err)
		if resp.StatusCode >= 500 && time.Now().Before(deadline) {
			t.Logf("retrying %s %s after transient %d", method, path, resp.StatusCode)
			time.Sleep(3 * time.Second)
			continue
		}
		require.Less(resp.StatusCode, 300, "%s %s: %s", method, path, string(raw))
		out := map[string]any{}
		require.NoError(json.Unmarshal(raw, &out), string(raw))
		return out
	}
}

func gitlabContainerMRState(
	t *testing.T,
	ctx context.Context,
	manifest gitLabContainerManifest,
	iid int,
) string {
	t.Helper()
	mr := gitlabContainerAPI(t, ctx, manifest, http.MethodGet,
		fmt.Sprintf("/projects/%d/merge_requests/%d", manifest.ProjectID, iid), nil)
	state, _ := mr["state"].(string)
	return state
}

func gitlabContainerIssueState(
	t *testing.T,
	ctx context.Context,
	manifest gitLabContainerManifest,
	iid int,
) string {
	t.Helper()
	issue := gitlabContainerAPI(t, ctx, manifest, http.MethodGet,
		fmt.Sprintf("/projects/%d/issues/%d", manifest.ProjectID, iid), nil)
	state, _ := issue["state"].(string)
	return state
}

func gitlabContainerDiscussionResolved(
	t *testing.T,
	ctx context.Context,
	manifest gitLabContainerManifest,
	iid int,
	discussionID string,
) bool {
	t.Helper()
	discussion := gitlabContainerAPI(t, ctx, manifest, http.MethodGet,
		fmt.Sprintf("/projects/%d/merge_requests/%d/discussions/%s",
			manifest.ProjectID, iid, discussionID),
		nil,
	)
	notes, _ := discussion["notes"].([]any)
	require.NotEmpty(t, notes)
	first, _ := notes[0].(map[string]any)
	resolved, _ := first["resolved"].(bool)
	return resolved
}

// waitForGitLabMergeable polls until GitLab's async merge status check
// settles; accepting an MR while the check runs returns 405.
func waitForGitLabMergeable(
	t *testing.T,
	ctx context.Context,
	manifest gitLabContainerManifest,
	iid int,
) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		mr := gitlabContainerAPI(t, ctx, manifest, http.MethodGet,
			fmt.Sprintf("/projects/%d/merge_requests/%d", manifest.ProjectID, iid), nil)
		status, _ := mr["detailed_merge_status"].(string)
		if status == "mergeable" {
			return
		}
		require.False(t, time.Now().After(deadline),
			"MR %d never became mergeable, last detailed_merge_status=%q", iid, status)
		time.Sleep(3 * time.Second)
	}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	return fmt.Sprint(addr.Port)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "could not find repo root from %s", dir)
		dir = parent
	}
}

func containerLogs(ctx context.Context, container testcontainers.Container) string {
	logs, err := container.Logs(ctx)
	if err != nil {
		return fmt.Sprintf("failed to read GitLab container logs: %v", err)
	}
	defer logs.Close()
	body, err := io.ReadAll(io.LimitReader(logs, 128*1024))
	if err != nil {
		return fmt.Sprintf("failed to read GitLab container logs: %v", err)
	}
	return string(body)
}

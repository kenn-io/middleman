package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/ratelimit"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
)

func newEnterpriseGHClient(hc *http.Client, baseURL, uploadURL string) (*gh.Client, error) {
	return gh.NewClient(gh.WithHTTPClient(hc), gh.WithEnterpriseURLs(baseURL, uploadURL))
}

func TestAuthenticatedViewerLoginRefreshesExpiredCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var login atomic.Value
	login.Store("alice")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v3/user", r.URL.Path)
		_, _ = fmt.Fprintf(w, `{"login":%q}`, login.Load().(string))
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{Host: "github.com", Read: server.Client(), Write: server.Client(), Notifications: server.Client(), Clock: time.Now, APIBase: server.URL + "/api/v3/", ViewerCacheTTL: time.Hour})
	require.NoError(err)
	live := client

	first, err := live.AuthenticatedViewerLogin(t.Context())
	require.NoError(err)
	assert.Equal("alice", first)
	login.Store("bob")
	live.viewerMu.Lock()
	live.viewerLoginAt = time.Now().Add(-time.Hour - time.Minute)
	live.viewerMu.Unlock()

	second, err := live.AuthenticatedViewerLogin(t.Context())
	require.NoError(err)
	assert.Equal("bob", second)
}

func TestDeleteIssueCommentUsesWriteAPI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	readServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.Fail("delete used the read GitHub client")
		http.Error(w, "wrong credential", http.StatusForbidden)
	}))
	defer readServer.Close()
	writeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodDelete, r.Method)
		assert.Equal("/api/v3/repos/acme/widgets/issues/comments/44", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer writeServer.Close()

	readClient, err := newEnterpriseGHClient(readServer.Client(), readServer.URL+"/api/v3/", readServer.URL+"/api/uploads/")
	require.NoError(err)
	writeClient, err := newEnterpriseGHClient(writeServer.Client(), writeServer.URL+"/api/v3/", writeServer.URL+"/api/uploads/")
	require.NoError(err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx }, gh: readClient, ghWrite: writeClient}

	require.NoError(client.DeleteIssueComment(t.Context(), "acme", "widgets", 44))
}

func TestDeleteIssueCommentMapsNotFound(t *testing.T) {
	require := require.New(t)
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	ghClient, err := newEnterpriseGHClient(server.Client(), server.URL+"/api/v3/", server.URL+"/api/uploads/")
	require.NoError(err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx }, ghWrite: ghClient}

	err = client.DeleteIssueComment(t.Context(), "acme", "widgets", 44)
	require.ErrorIs(err, platform.ErrNotFound)
}

func TestApplyReviewSuggestionEdits(t *testing.T) {
	assert := assert.New(t)
	content := "package main\nfunc main() {\n\tfmt.Println(\"old\")\n}\n"

	got, err := applyReviewSuggestionEdits(content, []platform.ReviewSuggestion{
		{
			Range: platform.DiffReviewLineRange{
				Path: "main.go",
				Side: "right",
				Line: 3,
			},
			Replacement: "\tfmt.Println(\"new\")\n\tfmt.Println(\"done\")",
		},
	})

	require.NoError(t, err)
	assert.Equal("package main\nfunc main() {\n\tfmt.Println(\"new\")\n\tfmt.Println(\"done\")\n}\n", got)
}

func TestApplyReviewSuggestionEditsRejectsOverlap(t *testing.T) {
	startLine := 1
	_, err := applyReviewSuggestionEdits("a\nb\nc\n", []platform.ReviewSuggestion{
		{
			Range:       platform.DiffReviewLineRange{Path: "main.go", Side: "right", StartLine: &startLine, Line: 2},
			Replacement: "x",
		},
		{
			Range:       platform.DiffReviewLineRange{Path: "main.go", Side: "right", Line: 2},
			Replacement: "y",
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlapping")
}

func TestApplyReviewSuggestionEditsPreservesTrailingBlankReplacementLine(t *testing.T) {
	got, err := applyReviewSuggestionEdits("one\ntwo\n", []platform.ReviewSuggestion{
		{
			Range:       platform.DiffReviewLineRange{Path: "main.go", Side: "right", Line: 2},
			Replacement: "TWO\n",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "one\nTWO\n\n", got)
}

func TestApplyReviewSuggestionEditsDeletesOnlyLineWithoutLeavingNewline(t *testing.T) {
	got, err := applyReviewSuggestionEdits("only\n", []platform.ReviewSuggestion{
		{
			Range:       platform.DiffReviewLineRange{Path: "main.go", Side: "right", Line: 1},
			Replacement: "",
		},
	})

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestApplyReviewSuggestionsRejectsWhitespacePaddedPath(t *testing.T) {
	content := base64.StdEncoding.EncodeToString([]byte("one\ntwo\n"))
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/fork/widget/contents/src/main.go", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"` + content + `","path":"src/main.go","sha":"file-sha"}`))
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"createCommitOnBranch":{"commit":{"oid":"commit-sha","url":"https://github.com/fork/widget/commit/commit-sha"}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/",
		srv.URL+"/api/uploads/")

	require.NoError(t, err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:              ghClient,
		ghWrite:         ghClient,
		httpWriteClient: srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	_, err = client.ApplyReviewSuggestions(t.Context(), "acme", "widget", 7, platform.ApplyReviewSuggestionsInput{
		HeadBranch:       "feature/suggestion",
		HeadRepoCloneURL: "https://github.com/fork/widget.git",
		ExpectedHeadSHA:  "head-sha",
		Suggestions: []platform.ReviewSuggestion{{
			Range:       platform.DiffReviewLineRange{Path: "src/main.go ", Side: "right", Line: 2},
			Replacement: "TWO",
		}},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "leading or trailing whitespace")
}

func TestApplyReviewSuggestionsCreatesBoundCommit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := base64.StdEncoding.EncodeToString([]byte("one\ntwo\nthree\n"))
	var graphqlPayload graphQLRequest
	var graphqlDecodeErr error
	var pullCalls int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/widget/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		pullCalls++
		writeReviewSuggestionPullResponse(w, "open", "fork/widget")
	})
	mux.HandleFunc("/api/v3/repos/fork/widget", func(w http.ResponseWriter, _ *http.Request) {
		writeReviewSuggestionRepositoryResponse(w, "fork/widget")
	})
	mux.HandleFunc("/api/v3/repos/fork/widget/contents/src/main.go", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("head-sha", r.URL.Query().Get("ref"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"` + content + `","path":"src/main.go","sha":"file-sha"}`))
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		graphqlDecodeErr = json.NewDecoder(r.Body).Decode(&graphqlPayload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"createCommitOnBranch":{"commit":{"oid":"commit-sha","url":"https://github.com/fork/widget/commit/commit-sha"}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/",
		srv.URL+"/api/uploads/")

	require.NoError(err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:              ghClient,
		ghWrite:         ghClient,
		httpWriteClient: srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	result, err := client.ApplyReviewSuggestions(t.Context(), "acme", "widget", 7, platform.ApplyReviewSuggestionsInput{
		HeadBranch:       "feature/suggestion",
		HeadRepoCloneURL: "https://github.com/fork/widget.git",
		ExpectedHeadSHA:  "head-sha",
		Message:          "Apply suggested fix",
		Suggestions: []platform.ReviewSuggestion{{
			Range:       platform.DiffReviewLineRange{Path: "src/main.go", Side: "right", Line: 2},
			Replacement: "TWO",
		}},
	})

	require.NoError(err)
	require.NoError(graphqlDecodeErr)
	assert.Equal("commit-sha", result.CommitSHA)
	variables := graphqlPayload.Variables["input"].(map[string]any)
	branch := variables["branch"].(map[string]any)
	assert.Equal("fork/widget", branch["repositoryNameWithOwner"])
	assert.Equal("feature/suggestion", branch["branchName"])
	assert.Equal("head-sha", variables["expectedHeadOid"])
	assert.Equal("Apply suggested fix", variables["message"].(map[string]any)["headline"])
	additions := variables["fileChanges"].(map[string]any)["additions"].([]any)
	require.Len(additions, 1)
	addition := additions[0].(map[string]any)
	assert.Equal("src/main.go", addition["path"])
	decoded, err := base64.StdEncoding.DecodeString(addition["contents"].(string))
	require.NoError(err)
	assert.Equal("one\nTWO\nthree\n", string(decoded))
	assert.Equal(2, pullCalls)
}

func TestApplyReviewSuggestionsProbesHeadRepoWithWriteCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := base64.StdEncoding.EncodeToString([]byte("one\ntwo\nthree\n"))
	var pullCalls int
	var readRepoCalls int
	var writeRepoCalls int
	var contentCalls int
	var graphQLCalls int

	readMux := http.NewServeMux()
	readMux.HandleFunc("/api/v3/repos/acme/widget/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		pullCalls++
		writeReviewSuggestionPullResponse(w, "open", "fork/widget")
	})
	readMux.HandleFunc("/api/v3/repos/fork/widget", func(w http.ResponseWriter, _ *http.Request) {
		readRepoCalls++
		http.Error(w, "read credential cannot see fork", http.StatusNotFound)
	})
	readSrv := httptest.NewServer(readMux)
	defer readSrv.Close()

	writeMux := http.NewServeMux()
	writeMux.HandleFunc("/api/v3/repos/fork/widget", func(w http.ResponseWriter, _ *http.Request) {
		writeRepoCalls++
		writeReviewSuggestionRepositoryResponse(w, "fork/widget")
	})
	writeMux.HandleFunc("/api/v3/repos/fork/widget/contents/src/main.go", func(w http.ResponseWriter, _ *http.Request) {
		contentCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"` + content + `","path":"src/main.go","sha":"file-sha"}`))
	})
	writeMux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		graphQLCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"createCommitOnBranch":{"commit":{"oid":"commit-sha","url":"https://github.com/fork/widget/commit/commit-sha"}}}}`))
	})
	writeSrv := httptest.NewServer(writeMux)
	defer writeSrv.Close()

	readGH, err := newEnterpriseGHClient(readSrv.Client(),
		readSrv.URL+"/api/v3/",
		readSrv.URL+"/api/uploads/")

	require.NoError(err)
	writeGH, err := newEnterpriseGHClient(writeSrv.Client(),
		writeSrv.URL+"/api/v3/",
		writeSrv.URL+"/api/uploads/")

	require.NoError(err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:              readGH,
		ghWrite:         writeGH,
		httpWriteClient: writeSrv.Client(),
		graphQLEndpoint: writeSrv.URL + "/graphql",
	}

	result, err := client.ApplyReviewSuggestions(t.Context(), "acme", "widget", 7, platform.ApplyReviewSuggestionsInput{
		HeadBranch:       "feature/suggestion",
		HeadRepoCloneURL: "https://github.com/fork/widget.git",
		ExpectedHeadSHA:  "head-sha",
		Suggestions: []platform.ReviewSuggestion{{
			Range:       platform.DiffReviewLineRange{Path: "src/main.go", Side: "right", Line: 2},
			Replacement: "TWO",
		}},
	})

	require.NoError(err)
	assert.Equal("commit-sha", result.CommitSHA)
	assert.Equal(2, pullCalls)
	assert.Zero(readRepoCalls)
	assert.Equal(2, writeRepoCalls)
	assert.Equal(1, contentCalls)
	assert.Equal(1, graphQLCalls)
}

func TestApplyReviewSuggestionsFailsClosedWhenPullNotOpenUpstream(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var contentCalls int
	var graphqlCalls int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/widget/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		writeReviewSuggestionPullResponse(w, "closed", "")
	})
	mux.HandleFunc("/api/v3/repos/fork/widget/contents/src/main.go", func(w http.ResponseWriter, _ *http.Request) {
		contentCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"","path":"src/main.go","sha":"file-sha"}`))
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		graphqlCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"createCommitOnBranch":{"commit":{"oid":"commit-sha","url":"https://github.com/fork/widget/commit/commit-sha"}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/",
		srv.URL+"/api/uploads/")

	require.NoError(err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:              ghClient,
		ghWrite:         ghClient,
		httpWriteClient: srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	_, err = client.ApplyReviewSuggestions(t.Context(), "acme", "widget", 7, platform.ApplyReviewSuggestionsInput{
		HeadBranch:       "feature/suggestion",
		HeadRepoCloneURL: "https://github.com/fork/widget.git",
		ExpectedHeadSHA:  "head-sha",
		Suggestions: []platform.ReviewSuggestion{{
			Range:       platform.DiffReviewLineRange{Path: "src/main.go", Side: "right", Line: 2},
			Replacement: "TWO",
		}},
	})

	require.Error(err)
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodeConflict, platformErr.Code)
	assert.Equal("not_open", platformErr.Details["reason"])
	assert.Zero(contentCalls)
	assert.Zero(graphqlCalls)
}

func TestApplyReviewSuggestionsFailsClosedWhenPullClosesBeforeMutation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := base64.StdEncoding.EncodeToString([]byte("one\ntwo\nthree\n"))
	var pullCalls int
	var contentCalls int
	var graphQLCalls int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/widget/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		pullCalls++
		if pullCalls == 1 {
			writeReviewSuggestionPullResponse(w, "open", "fork/widget")
			return
		}
		writeReviewSuggestionPullResponse(w, "closed", "")
	})
	mux.HandleFunc("/api/v3/repos/fork/widget", func(w http.ResponseWriter, _ *http.Request) {
		writeReviewSuggestionRepositoryResponse(w, "fork/widget")
	})
	mux.HandleFunc("/api/v3/repos/fork/widget/contents/src/main.go", func(w http.ResponseWriter, _ *http.Request) {
		contentCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"` + content + `","path":"src/main.go","sha":"file-sha"}`))
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		graphQLCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"createCommitOnBranch":{"commit":{"oid":"commit-sha","url":"https://github.com/fork/widget/commit/commit-sha"}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/",
		srv.URL+"/api/uploads/")

	require.NoError(err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:              ghClient,
		ghWrite:         ghClient,
		httpWriteClient: srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	_, err = client.ApplyReviewSuggestions(t.Context(), "acme", "widget", 7, platform.ApplyReviewSuggestionsInput{
		HeadBranch:       "feature/suggestion",
		HeadRepoCloneURL: "https://github.com/fork/widget.git",
		ExpectedHeadSHA:  "head-sha",
		Suggestions: []platform.ReviewSuggestion{{
			Range:       platform.DiffReviewLineRange{Path: "src/main.go", Side: "right", Line: 2},
			Replacement: "TWO",
		}},
	})

	require.Error(err)
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodeConflict, platformErr.Code)
	assert.Equal("not_open", platformErr.Details["reason"])
	assert.Equal(2, pullCalls)
	assert.Equal(1, contentCalls)
	assert.Zero(graphQLCalls)
}

func TestApplyReviewSuggestionsFailsStaleBeforeContentWhenPullHeadAlreadyChanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tests := []struct {
		name    string
		headRef string
		headSHA string
		message string
	}{
		{
			name:    "retargeted branch",
			headRef: "feature/other",
			headSHA: "head-sha",
			message: "branch changed",
		},
		{
			name:    "moved head",
			headRef: "feature/suggestion",
			headSHA: "new-head-sha",
			message: "head changed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var repoCalls int
			var contentCalls int
			var graphQLCalls int

			mux := http.NewServeMux()
			mux.HandleFunc("/api/v3/repos/acme/widget/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
				writeReviewSuggestionPullResponseWithHead(w, "open", "fork/widget", tt.headRef, tt.headSHA)
			})
			mux.HandleFunc("/api/v3/repos/fork/widget", func(w http.ResponseWriter, _ *http.Request) {
				repoCalls++
				http.Error(w, "head repo should not be probed", http.StatusInternalServerError)
			})
			mux.HandleFunc("/api/v3/repos/fork/widget/contents/src/main.go", func(w http.ResponseWriter, _ *http.Request) {
				contentCalls++
				http.Error(w, "content should not be read", http.StatusInternalServerError)
			})
			mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
				graphQLCalls++
				http.Error(w, "mutation should not run", http.StatusInternalServerError)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			ghClient, err := newEnterpriseGHClient(srv.Client(),
				srv.URL+"/api/v3/",
				srv.URL+"/api/uploads/")

			require.NoError(err)
			client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
				gh:              ghClient,
				ghWrite:         ghClient,
				httpWriteClient: srv.Client(),
				graphQLEndpoint: srv.URL + "/graphql",
			}

			_, err = client.ApplyReviewSuggestions(t.Context(), "acme", "widget", 7, platform.ApplyReviewSuggestionsInput{
				HeadBranch:       "feature/suggestion",
				HeadRepoCloneURL: "https://github.com/fork/widget.git",
				ExpectedHeadSHA:  "head-sha",
				Suggestions: []platform.ReviewSuggestion{{
					Range:       platform.DiffReviewLineRange{Path: "src/main.go", Side: "right", Line: 2},
					Replacement: "TWO",
				}},
			})

			require.Error(err)
			require.ErrorIs(err, platform.ErrStaleState)
			var platformErr *platform.Error
			require.ErrorAs(err, &platformErr)
			assert.Equal(platform.ErrCodeStaleState, platformErr.Code)
			assert.Contains(err.Error(), tt.message)
			assert.Zero(repoCalls)
			assert.Zero(contentCalls)
			assert.Zero(graphQLCalls)
		})
	}
}

func TestApplyReviewSuggestionsFailsStaleWhenPullHeadChangesBeforeMutation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := base64.StdEncoding.EncodeToString([]byte("one\ntwo\nthree\n"))

	tests := []struct {
		name      string
		secondRef string
		secondSHA string
		message   string
	}{
		{
			name:      "retargeted branch",
			secondRef: "feature/other",
			secondSHA: "head-sha",
			message:   "branch changed",
		},
		{
			name:      "moved head",
			secondRef: "feature/suggestion",
			secondSHA: "new-head-sha",
			message:   "head changed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pullCalls int
			var contentCalls int
			var graphQLCalls int

			mux := http.NewServeMux()
			mux.HandleFunc("/api/v3/repos/acme/widget/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
				pullCalls++
				if pullCalls == 1 {
					writeReviewSuggestionPullResponse(w, "open", "fork/widget")
					return
				}
				writeReviewSuggestionPullResponseWithHead(w, "open", "fork/widget", tt.secondRef, tt.secondSHA)
			})
			mux.HandleFunc("/api/v3/repos/fork/widget", func(w http.ResponseWriter, _ *http.Request) {
				writeReviewSuggestionRepositoryResponse(w, "fork/widget")
			})
			mux.HandleFunc("/api/v3/repos/fork/widget/contents/src/main.go", func(w http.ResponseWriter, _ *http.Request) {
				contentCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"` + content + `","path":"src/main.go","sha":"file-sha"}`))
			})
			mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
				graphQLCalls++
				http.Error(w, "mutation should not run", http.StatusInternalServerError)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			ghClient, err := newEnterpriseGHClient(srv.Client(),
				srv.URL+"/api/v3/",
				srv.URL+"/api/uploads/")

			require.NoError(err)
			client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
				gh:              ghClient,
				ghWrite:         ghClient,
				httpWriteClient: srv.Client(),
				graphQLEndpoint: srv.URL + "/graphql",
			}

			_, err = client.ApplyReviewSuggestions(t.Context(), "acme", "widget", 7, platform.ApplyReviewSuggestionsInput{
				HeadBranch:       "feature/suggestion",
				HeadRepoCloneURL: "https://github.com/fork/widget.git",
				ExpectedHeadSHA:  "head-sha",
				Suggestions: []platform.ReviewSuggestion{{
					Range:       platform.DiffReviewLineRange{Path: "src/main.go", Side: "right", Line: 2},
					Replacement: "TWO",
				}},
			})

			require.Error(err)
			require.ErrorIs(err, platform.ErrStaleState)
			var platformErr *platform.Error
			require.ErrorAs(err, &platformErr)
			assert.Equal(platform.ErrCodeStaleState, platformErr.Code)
			assert.Contains(err.Error(), tt.message)
			assert.Equal(2, pullCalls)
			assert.Equal(1, contentCalls)
			assert.Zero(graphQLCalls)
		})
	}
}

func TestApplyReviewSuggestionsFailsClosedWhenLiveHeadRepoMissing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var contentCalls int
	var graphQLCalls int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/widget/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		writeReviewSuggestionPullResponse(w, "open", "")
	})
	mux.HandleFunc("/api/v3/repos/fork/widget/contents/src/main.go", func(w http.ResponseWriter, _ *http.Request) {
		contentCalls++
		http.Error(w, "content should not be read", http.StatusInternalServerError)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		graphQLCalls++
		http.Error(w, "mutation should not run", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/",
		srv.URL+"/api/uploads/")

	require.NoError(err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:              ghClient,
		ghWrite:         ghClient,
		httpWriteClient: srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	_, err = client.ApplyReviewSuggestions(t.Context(), "acme", "widget", 7, platform.ApplyReviewSuggestionsInput{
		HeadBranch:       "feature/suggestion",
		HeadRepoCloneURL: "https://github.com/fork/widget.git",
		ExpectedHeadSHA:  "head-sha",
		Suggestions: []platform.ReviewSuggestion{{
			Range:       platform.DiffReviewLineRange{Path: "src/main.go", Side: "right", Line: 2},
			Replacement: "TWO",
		}},
	})

	require.Error(err)
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodeConflict, platformErr.Code)
	assert.Equal("head_repo_unknown", platformErr.Details["reason"])
	assert.Zero(contentCalls)
	assert.Zero(graphQLCalls)
}

func TestApplyReviewSuggestionsFailsClosedWhenLiveHeadRepoInaccessible(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var repoCalls int
	var contentCalls int
	var graphQLCalls int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/widget/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		writeReviewSuggestionPullResponse(w, "open", "fork/widget")
	})
	mux.HandleFunc("/api/v3/repos/fork/widget", func(w http.ResponseWriter, _ *http.Request) {
		repoCalls++
		http.Error(w, "not found", http.StatusNotFound)
	})
	mux.HandleFunc("/api/v3/repos/fork/widget/contents/src/main.go", func(w http.ResponseWriter, _ *http.Request) {
		contentCalls++
		http.Error(w, "content should not be read", http.StatusInternalServerError)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		graphQLCalls++
		http.Error(w, "mutation should not run", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/",
		srv.URL+"/api/uploads/")

	require.NoError(err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:              ghClient,
		ghWrite:         ghClient,
		httpWriteClient: srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	_, err = client.ApplyReviewSuggestions(t.Context(), "acme", "widget", 7, platform.ApplyReviewSuggestionsInput{
		HeadBranch:       "feature/suggestion",
		HeadRepoCloneURL: "https://github.com/fork/widget.git",
		ExpectedHeadSHA:  "head-sha",
		Suggestions: []platform.ReviewSuggestion{{
			Range:       platform.DiffReviewLineRange{Path: "src/main.go", Side: "right", Line: 2},
			Replacement: "TWO",
		}},
	})

	require.Error(err)
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.ErrCodeConflict, platformErr.Code)
	assert.Equal("head_repo_unknown", platformErr.Details["reason"])
	assert.Equal(1, repoCalls)
	assert.Zero(contentCalls)
	assert.Zero(graphQLCalls)
}

func writeReviewSuggestionPullResponse(w http.ResponseWriter, state string, headFullName string) {
	writeReviewSuggestionPullResponseWithHead(w, state, headFullName, "feature/suggestion", "head-sha")
}

func writeReviewSuggestionPullResponseWithHead(
	w http.ResponseWriter,
	state string,
	headFullName string,
	headRef string,
	headSHA string,
) {
	w.Header().Set("Content-Type", "application/json")
	head := fmt.Sprintf(`{"repo":null,"ref":%q,"sha":%q}`, headRef, headSHA)
	if headFullName != "" {
		headOwner, headRepo, _ := strings.Cut(headFullName, "/")
		head = fmt.Sprintf(
			`{"repo":{"full_name":%q,"clone_url":%q,"owner":{"login":%q},"name":%q},"ref":%q,"sha":%q}`,
			headFullName,
			"https://github.com/"+headFullName+".git",
			headOwner,
			headRepo,
			headRef,
			headSHA,
		)
	}
	_, _ = fmt.Fprintf(w, `{"number":7,"state":%q,"head":%s}`, state, head)
}

func writeReviewSuggestionRepositoryResponse(w http.ResponseWriter, fullName string) {
	w.Header().Set("Content-Type", "application/json")
	owner, name, _ := strings.Cut(fullName, "/")
	_, _ = fmt.Fprintf(
		w,
		`{"name":%q,"full_name":%q,"owner":{"login":%q}}`,
		name,
		fullName,
		owner,
	)
}

func TestGitHubCreateCommitGraphQLErrorMapsStableReasons(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tests := []struct {
		name       string
		errors     []graphQLError
		wantCode   platform.PlatformErrorCode
		wantReason string
	}{
		{
			name:     "expected head moved",
			errors:   []graphQLError{{Message: "expectedHeadOid does not match"}},
			wantCode: platform.ErrCodeStaleState,
		},
		{
			name:       "head repo resolution race",
			errors:     []graphQLError{{Type: "NOT_FOUND", Message: "Could not resolve to a Repository"}},
			wantCode:   platform.ErrCodeConflict,
			wantReason: "head_repo_unknown",
		},
		{
			name:       "head branch resolution race",
			errors:     []graphQLError{{Message: "Could not resolve to a Ref"}},
			wantCode:   platform.ErrCodeConflict,
			wantReason: "head_repo_unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := githubCreateCommitGraphQLError("acme", "widget", 7, tt.errors)

			require.Error(err)
			var platformErr *platform.Error
			require.ErrorAs(err, &platformErr)
			assert.Equal(tt.wantCode, platformErr.Code)
			if tt.wantReason != "" {
				assert.Equal(tt.wantReason, platformErr.Details["reason"])
			}
		})
	}
}

func TestGitHubSuggestionHeadRepoRequiresCloneURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tests := []struct {
		name     string
		cloneURL string
		wantFull string
		wantErr  bool
	}{
		{
			name:     "same repo clone URL",
			cloneURL: "https://github.com/acme/widget.git",
			wantFull: "acme/widget",
		},
		{
			name:     "fork clone URL",
			cloneURL: "https://github.com/fork/widget.git",
			wantFull: "fork/widget",
		},
		{
			name:    "missing clone URL",
			wantErr: true,
		},
		{
			name:     "unparseable clone URL",
			cloneURL: "not-a-url",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headOwner, headRepo, fullName, err := githubSuggestionHeadRepo(
				"acme",
				"widget",
				platform.ApplyReviewSuggestionsInput{HeadRepoCloneURL: tt.cloneURL},
			)

			if tt.wantErr {
				require.Error(err)
				var platformErr *platform.Error
				require.ErrorAs(err, &platformErr)
				assert.Equal(platform.ErrCodeInvalidArgument, platformErr.Code)
				assert.Contains(err.Error(), "head repository is required")
				return
			}
			require.NoError(err)
			assert.Equal(tt.wantFull, fullName)
			wantOwner, wantRepo, ok := strings.Cut(tt.wantFull, "/")
			require.True(ok)
			assert.Equal(wantOwner, headOwner)
			assert.Equal(wantRepo, headRepo)
		})
	}
}

func TestListReleasesTracksRate(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	rt := ratelimit.NewPlatformRateTracker(database, "github", "github.example.com", "host", "rest")
	resetAt := time.Now().Add(time.Hour).Unix()
	var gotMethod string
	var gotPerPage string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/widgets/releases", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPerPage = r.URL.Query().Get("per_page")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4998")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt, 10))
		_, _ = w.Write([]byte(`[{"tag_name":"v1.0.0","name":"Release v1.0.0"}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/", srv.URL+"/api/uploads/")

	require.NoError(err)
	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx }, gh: ghClient, rateTracker: rt}

	releases, err := c.ListReleases(t.Context(), "acme", "widgets", 2)
	require.NoError(err)
	require.Len(releases, 1)
	require.Equal(http.MethodGet, gotMethod)
	require.Equal("2", gotPerPage)
	require.Equal("v1.0.0", releases[0].GetTagName())
	require.Equal(1, rt.RequestsThisHour())
	require.Equal(4998, rt.Remaining())
	require.Equal(5000, rt.RateLimit())
}

func TestListTagsTracksRate(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	rt := ratelimit.NewPlatformRateTracker(database, "github", "github.example.com", "host", "rest")
	resetAt := time.Now().Add(time.Hour).Unix()
	var gotMethod string
	var gotPerPage string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repos/acme/widgets/tags", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPerPage = r.URL.Query().Get("per_page")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4997")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt, 10))
		_, _ = w.Write([]byte(`[{"name":"v1.0.0","commit":{"sha":"abcdef1234567890abcdef1234567890abcdef12"}}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/", srv.URL+"/api/uploads/")

	require.NoError(err)
	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx }, gh: ghClient, rateTracker: rt}

	tags, err := c.ListTags(t.Context(), "acme", "widgets", 2)
	require.NoError(err)
	require.Len(tags, 1)
	require.Equal(http.MethodGet, gotMethod)
	require.Equal("2", gotPerPage)
	require.Equal("v1.0.0", tags[0].GetName())
	require.Equal("abcdef1234567890abcdef1234567890abcdef12", tags[0].GetCommit().GetSHA())
	require.Equal(1, rt.RequestsThisHour())
	require.Equal(4997, rt.Remaining())
	require.Equal(5000, rt.RateLimit())
}

func TestTrackGraphQLRateHeadersUsesGraphQLTracker(t *testing.T) {
	assert := assert.New(t)
	database := dbtest.Open(t)
	restRT := ratelimit.NewPlatformRateTracker(database, "github", "github.example.com", "host", "rest")
	gqlRT := ratelimit.NewPlatformRateTracker(database, "github", "github.example.com", "host", "graphql")
	resetAt := time.Now().Add(time.Hour).Unix()
	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx }, rateTracker: restRT}
	c.SetGraphQLRateTracker(gqlRT)
	header := http.Header{}
	header.Set("X-RateLimit-Limit", "5000")
	header.Set("X-RateLimit-Remaining", "4996")
	header.Set("X-RateLimit-Reset", strconv.FormatInt(resetAt, 10))
	resp := &http.Response{Header: header}

	c.trackGraphQLRateHeaders(resp)

	assert.Equal(0, restRT.RequestsThisHour())
	assert.False(restRT.Known())
	assert.Equal(1, gqlRT.RequestsThisHour())
	assert.Equal(4996, gqlRT.Remaining())
	assert.Equal(5000, gqlRT.RateLimit())
	assert.True(gqlRT.Known())
}

func TestListRepositoriesByOwnerUsesAuthenticatedEndpointForViewer(t *testing.T) {
	require := require.New(t)
	var paths []string
	var authenticatedAffiliation string
	var authenticatedType string
	var publicUserEndpointUsed bool

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/user", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"mariusvniekerk"}`))
	})
	mux.HandleFunc("/api/v3/user/repos", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		authenticatedAffiliation = r.URL.Query().Get("affiliation")
		authenticatedType = r.URL.Query().Get("type")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"name":     "dotfiles2026",
			"private":  true,
			"fork":     false,
			"owner":    map[string]string{"login": "mariusvniekerk"},
			"archived": false,
		}})
	})
	mux.HandleFunc("/api/v3/users/mariusvniekerk/repos", func(w http.ResponseWriter, r *http.Request) {
		publicUserEndpointUsed = true
		http.Error(w, "unexpected endpoint", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/", srv.URL+"/api/uploads/")

	require.NoError(err)
	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx }, gh: ghClient}

	repos, err := c.ListRepositoriesByOwner(t.Context(), "mariusvniekerk")
	require.NoError(err)
	require.Len(repos, 1)
	require.Equal("dotfiles2026", repos[0].GetName())
	require.True(repos[0].GetPrivate())
	require.Equal("owner", authenticatedAffiliation)
	require.Empty(authenticatedType)
	require.False(publicUserEndpointUsed)
	require.Equal([]string{
		"/api/v3/user",
		"/api/v3/user/repos?affiliation=owner&per_page=100",
	}, paths)
}

func TestListRepositoriesByOwnerUsesInstallationReposWithAppToken(t *testing.T) {
	require := require.New(t)
	var paths []string
	var installationEndpointUsed bool
	var publicUserEndpointUsed bool

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/user", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		http.Error(w, "installation tokens cannot use /user", http.StatusUnauthorized)
	})
	mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		installationEndpointUsed = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repositories": []map[string]any{
				{
					"name":     "private-repo",
					"private":  true,
					"fork":     false,
					"owner":    map[string]string{"login": "mariusvniekerk"},
					"archived": false,
				},
				{
					"name":     "org-repo",
					"private":  true,
					"fork":     false,
					"owner":    map[string]string{"login": "kenn-io"},
					"archived": false,
				},
			},
		})
	})
	mux.HandleFunc("/api/v3/users/mariusvniekerk/repos", func(w http.ResponseWriter, r *http.Request) {
		publicUserEndpointUsed = true
		http.Error(w, "unexpected endpoint", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/", srv.URL+"/api/uploads/")

	require.NoError(err)
	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:   ghClient,
		auth: Authentication{InstallationActive: func(owner string) bool { return true }},
	}

	repos, err := c.ListRepositoriesByOwner(t.Context(), "mariusvniekerk")
	require.NoError(err)
	require.Len(repos, 1)
	require.Equal("private-repo", repos[0].GetName())
	require.True(repos[0].GetPrivate())
	require.True(installationEndpointUsed)
	require.False(publicUserEndpointUsed)
	require.Equal([]string{
		"/api/v3/installation/repositories?per_page=100",
	}, paths)
}

func TestGetRepositoryOverlaysMergeSettingsFromUserCredential(t *testing.T) {
	require := require.New(t)

	readSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"node_id":"repo-1","name":"widget","owner":{"login":"acme"}}`))
	}))
	defer readSrv.Close()
	writeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"node_id":"repo-1","name":"widget","owner":{"login":"acme"},"allow_squash_merge":true,"allow_merge_commit":false,"allow_rebase_merge":false,"permissions":{"push":true}}`))
	}))
	defer writeSrv.Close()

	readGH, err := newEnterpriseGHClient(readSrv.Client(), readSrv.URL+"/", readSrv.URL+"/")
	require.NoError(err)
	writeGH, err := newEnterpriseGHClient(writeSrv.Client(), writeSrv.URL+"/", writeSrv.URL+"/")
	require.NoError(err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh: readGH, ghWrite: writeGH,
		auth: Authentication{InstallationActive: func(owner string) bool { return true }},
	}

	repo, err := client.GetRepository(t.Context(), "acme", "widget")
	require.NoError(err)
	require.NotNil(repo.AllowSquashMerge)
	require.NotNil(repo.AllowMergeCommit)
	require.NotNil(repo.AllowRebaseMerge)
	require.True(*repo.AllowSquashMerge)
	require.False(*repo.AllowMergeCommit)
	require.False(*repo.AllowRebaseMerge)
	require.True(repo.Permissions.GetPush())
}

func TestGetRepositoryRetainsCompleteAppMergeSettingsWhenUserOverlayFails(t *testing.T) {
	require := require.New(t)

	readSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"node_id":"repo-1","name":"widget","owner":{"login":"acme"},"allow_squash_merge":true,"allow_merge_commit":false,"allow_rebase_merge":false}`))
	}))
	defer readSrv.Close()
	writeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "user credential unavailable", http.StatusUnauthorized)
	}))
	defer writeSrv.Close()

	readGH, err := newEnterpriseGHClient(readSrv.Client(), readSrv.URL+"/", readSrv.URL+"/")
	require.NoError(err)
	writeGH, err := newEnterpriseGHClient(writeSrv.Client(), writeSrv.URL+"/", writeSrv.URL+"/")
	require.NoError(err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh: readGH, ghWrite: writeGH,
		auth: Authentication{InstallationActive: func(owner string) bool { return true }},
	}

	repo, err := client.GetRepository(t.Context(), "acme", "widget")
	require.NoError(err)
	require.True(repo.GetAllowSquashMerge())
	require.False(repo.GetAllowMergeCommit())
	require.False(repo.GetAllowRebaseMerge())
	require.Nil(repo.Permissions)
}

func TestGetRepositoryKeepsCompleteAppMergeSettingsWhenUserFieldsAreIncomplete(t *testing.T) {
	require := require.New(t)

	readSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"node_id":"repo-1","name":"widget","owner":{"login":"acme"},"allow_squash_merge":false,"allow_merge_commit":true,"allow_rebase_merge":false}`))
	}))
	defer readSrv.Close()
	writeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"node_id":"repo-1","name":"widget","owner":{"login":"acme"},"allow_squash_merge":true,"permissions":{"push":true}}`))
	}))
	defer writeSrv.Close()

	readGH, err := newEnterpriseGHClient(readSrv.Client(), readSrv.URL+"/", readSrv.URL+"/")
	require.NoError(err)
	writeGH, err := newEnterpriseGHClient(writeSrv.Client(), writeSrv.URL+"/", writeSrv.URL+"/")
	require.NoError(err)
	client := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh: readGH, ghWrite: writeGH,
		auth: Authentication{InstallationActive: func(owner string) bool { return true }},
	}

	repo, err := client.GetRepository(t.Context(), "acme", "widget")
	require.NoError(err)
	require.False(repo.GetAllowSquashMerge())
	require.True(repo.GetAllowMergeCommit())
	require.False(repo.GetAllowRebaseMerge())
	require.True(repo.Permissions.GetPush())
}

// A host can carry an app installation for one owner while other owners
// on the same host stay on the PAT/gh chain. Listing repos for such an
// owner must not route to the installation-token-only endpoint: an app
// candidate scoped to a different account is skipped during token
// resolution, so /installation/repositories would 403 on the PAT.
func TestListRepositoriesByOwnerSkipsInstallationReposForUnmatchedOwner(t *testing.T) {
	require := require.New(t)
	var installationEndpointUsed bool
	var orgEndpointUsed bool

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, _ *http.Request) {
		installationEndpointUsed = true
		http.Error(w, "installation token cannot serve another owner", http.StatusForbidden)
	})
	mux.HandleFunc("/api/v3/user", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"mariusvniekerk"}`))
	})
	mux.HandleFunc("/api/v3/orgs/acme/repos", func(w http.ResponseWriter, _ *http.Request) {
		orgEndpointUsed = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"name":  "infra",
			"owner": map[string]string{"login": "acme"},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/", srv.URL+"/api/uploads/")

	require.NoError(err)
	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:   ghClient,
		auth: Authentication{InstallationActive: func(owner string) bool { return owner == "" || owner == "kenn-io" }},
	}

	repos, err := c.ListRepositoriesByOwner(t.Context(), "acme")
	require.NoError(err)
	require.Len(repos, 1)
	require.Equal("infra", repos[0].GetName())
	require.True(orgEndpointUsed)
	require.False(installationEndpointUsed,
		"a PAT-backed owner must not be routed to the installation-token-only endpoint")
}

func TestListRepositoriesByOwnerUsesPublicUserEndpointForOtherUsers(t *testing.T) {
	require := require.New(t)
	var paths []string
	var userRepoType string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/user", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"mariusvniekerk"}`))
	})
	mux.HandleFunc("/api/v3/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		http.Error(w, "not an org", http.StatusNotFound)
	})
	mux.HandleFunc("/api/v3/users/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		userRepoType = r.URL.Query().Get("type")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"name":  "public-repo",
			"owner": map[string]string{"login": "acme"},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(),
		srv.URL+"/api/v3/", srv.URL+"/api/uploads/")

	require.NoError(err)
	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx }, gh: ghClient}

	repos, err := c.ListRepositoriesByOwner(t.Context(), "acme")
	require.NoError(err)
	require.Len(repos, 1)
	require.Equal("public-repo", repos[0].GetName())
	require.Equal("owner", userRepoType)
	require.True(strings.HasPrefix(paths[1], "/api/v3/orgs/acme/repos?"))
	require.True(strings.HasPrefix(paths[2], "/api/v3/users/acme/repos?"))
}

func TestListForcePushEvents(t *testing.T) {
	require := require.New(t)
	var calls int
	var methods []string
	var contentTypes []string
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		calls++
		methods = append(methods, r.Method)
		contentTypes = append(contentTypes, r.Header.Get("Content-Type"))
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"__typename":"HeadRefForcePushedEvent","id":"HFP_1","actor":{"login":"alice"},"beforeCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"afterCommit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"createdAt":"2024-06-01T12:00:00Z","ref":{"name":"feature"}}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"__typename":"HeadRefForcePushedEvent","id":"HFP_2","actor":{"login":"alice"},"beforeCommit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"afterCommit":{"oid":"cccccccccccccccccccccccccccccccccccccccc"},"createdAt":"2024-06-01T12:05:00Z","ref":{"name":"feature"}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		httpClient:      srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	events, err := c.ListForcePushEvents(t.Context(), "owner", "repo", 42)
	require.NoError(err)
	require.Len(events, 2)
	require.Equal("alice", events[0].Actor)
	require.Equal("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", events[0].BeforeSHA)
	require.Equal("cccccccccccccccccccccccccccccccccccccccc", events[1].AfterSHA)
	require.Equal("feature", events[0].Ref)
	require.Equal(2, calls)
	require.Equal([]string{http.MethodPost, http.MethodPost}, methods)
	require.Equal([]string{"application/json", "application/json"}, contentTypes)
}

func TestListPullRequestTimelineEvents(t *testing.T) {
	require := require.New(t)
	var calls int
	var methods []string
	var contentTypes []string
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		calls++
		methods = append(methods, r.Method)
		contentTypes = append(contentTypes, r.Header.Get("Content-Type"))
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"__typename":"HeadRefForcePushedEvent","id":"HFP_1","actor":{"login":"alice"},"beforeCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"afterCommit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"createdAt":"2024-06-01T12:00:00Z","ref":{"name":"feature"}},{"__typename":"RenamedTitleEvent","id":"RTE_1","actor":{"login":"bob"},"createdAt":"2024-06-01T12:05:00Z","previousTitle":"Old title","currentTitle":"New title"}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[{"__typename":"BaseRefChangedEvent","id":"BRC_1","actor":{"login":"carol"},"createdAt":"2024-06-01T12:10:00Z","previousRefName":"main","currentRefName":"release"},{"__typename":"CommentDeletedEvent","id":"CDE_1","actor":{"login":"maintainer"},"createdAt":"2024-06-01T12:12:00Z","deletedCommentAuthor":{"login":"reviewer"}},{"__typename":"CrossReferencedEvent","id":"CRE_1","actor":{"login":"dave"},"createdAt":"2024-06-01T12:15:00Z","isCrossRepository":true,"willCloseTarget":false,"source":{"__typename":"Issue","number":77,"title":"Related bug","url":"https://github.com/other/repo/issues/77","repository":{"owner":{"login":"other"},"name":"repo"}}},{"__typename":"AssignedEvent","id":"AE_1","actor":{"login":"wesm"},"assignee":{"__typename":"User","login":"wesm"},"createdAt":"2024-06-01T12:20:00Z"},{"__typename":"UnassignedEvent","id":"UE_1","actor":{"login":"alice"},"assignee":{"__typename":"User","login":"bob"},"createdAt":"2024-06-01T12:25:00Z"},{"__typename":"MergedEvent","id":"ME_1","actor":{"login":"merger"},"createdAt":"2024-06-01T12:30:00Z"},{"__typename":"ClosedEvent","id":"CE_1","actor":{"login":"closer"},"createdAt":"2024-06-01T12:35:00Z"},{"__typename":"ReopenedEvent","id":"RE_1","actor":{"login":"opener"},"createdAt":"2024-06-01T12:40:00Z"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		httpClient:      srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	events, err := c.ListPullRequestTimelineEvents(t.Context(), "owner", "repo", 42)
	require.NoError(err)
	require.Len(events, 10)
	require.Equal("force_push", events[0].EventType)
	require.Equal("HFP_1", events[0].NodeID)
	require.Equal("alice", events[0].Actor)
	require.Equal("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", events[0].BeforeSHA)
	require.Equal("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", events[0].AfterSHA)
	require.Equal("feature", events[0].Ref)
	require.Equal("renamed_title", events[1].EventType)
	require.Equal("Old title", events[1].PreviousTitle)
	require.Equal("New title", events[1].CurrentTitle)
	require.Equal("base_ref_changed", events[2].EventType)
	require.Equal("main", events[2].PreviousRefName)
	require.Equal("release", events[2].CurrentRefName)
	require.Equal("comment_deleted", events[3].EventType)
	require.Equal("maintainer", events[3].Actor)
	require.Equal("reviewer", events[3].DeletedCommentAuthor)
	require.Equal("cross_referenced", events[4].EventType)
	require.Equal("Issue", events[4].SourceType)
	require.Equal("other", events[4].SourceOwner)
	require.Equal("repo", events[4].SourceRepo)
	require.Equal(77, events[4].SourceNumber)
	require.Equal("Related bug", events[4].SourceTitle)
	require.True(events[4].IsCrossRepository)
	require.False(events[4].WillCloseTarget)
	require.Equal("assigned", events[5].EventType)
	require.Equal("wesm", events[5].Actor)
	require.Equal("wesm", events[5].Assignee)
	require.Equal("unassigned", events[6].EventType)
	require.Equal("alice", events[6].Actor)
	require.Equal("bob", events[6].Assignee)
	require.Equal("merged", events[7].EventType)
	require.Equal("merger", events[7].Actor)
	require.Equal("closed", events[8].EventType)
	require.Equal("closer", events[8].Actor)
	require.Equal("reopened", events[9].EventType)
	require.Equal("opener", events[9].Actor)
	require.Equal(2, calls)
	require.Equal([]string{http.MethodPost, http.MethodPost}, methods)
	require.Equal([]string{"application/json", "application/json"}, contentTypes)
}

func TestListPullRequestReviewThreads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls int
	var methods []string
	var contentTypes []string
	var queries []string
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		calls++
		methods = append(methods, r.Method)
		contentTypes = append(contentTypes, r.Header.Get("Content-Type"))
		var request graphQLRequest
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		queries = append(queries, request.Query)
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"edges":[{"cursor":"thread-1","node":{"id":"PRRT_1","isResolved":false,"isOutdated":false,"path":"src/main.go","line":12,"originalLine":12,"startLine":10,"originalStartLine":10,"diffSide":"RIGHT","comments":{"nodes":[{"id":"PRRC_1","databaseId":101,"fullDatabaseId":"3312100450","body":"inline note","path":"src/main.go","line":12,"originalLine":12,"subjectType":"LINE","diffHunk":"@@","url":"https://github.example/pr#discussion_r101","author":{"login":"reviewer"},"commit":{"oid":"head-sha"},"originalCommit":{"oid":"original-sha"},"pullRequestReview":{"databaseId":201},"isMinimized":true,"minimizedReason":"OFF_TOPIC","createdAt":"2026-05-27T16:01:31Z","updatedAt":"2026-05-27T16:02:31Z"}],"pageInfo":{"hasNextPage":true,"endCursor":"comment-cursor-1"}}}}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}}}`))
			return
		}
		if calls == 2 {
			_, _ = w.Write([]byte(`{"data":{"node":{"comments":{"nodes":[{"id":"PRRC_1_REPLY","databaseId":103,"fullDatabaseId":3312100451,"body":"reply note","path":"src/main.go","line":12,"originalLine":12,"subjectType":"LINE","diffHunk":"@@","url":"https://github.example/pr#discussion_r103","author":{"login":"maintainer"},"commit":{"oid":"head-sha"},"originalCommit":{"oid":"original-sha"},"pullRequestReview":{"databaseId":201},"isMinimized":true,"minimizedReason":"ABUSE","createdAt":"2026-05-27T16:03:31Z","updatedAt":"2026-05-27T16:04:31Z"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"edges":[{"cursor":"thread-2","node":{"id":"PRRT_2","isResolved":true,"isOutdated":true,"path":"README.md","line":3,"originalLine":3,"startLine":null,"originalStartLine":null,"diffSide":"LEFT","comments":{"nodes":[{"id":"PRRC_2","databaseId":102,"fullDatabaseId":102,"body":"old note","path":"README.md","line":3,"originalLine":3,"subjectType":"FILE","diffHunk":"","url":"https://github.example/pr#discussion_r102","author":{"login":"maintainer"},"commit":{"oid":"new-head"},"originalCommit":{"oid":"old-head"},"pullRequestReview":{"databaseId":202},"createdAt":"2026-05-27T17:01:31Z","updatedAt":"2026-05-27T17:02:31Z"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		httpClient:      srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	threads, err := c.ListPullRequestReviewThreads(t.Context(), "owner", "repo", 42)
	require.NoError(err)
	require.Len(threads, 2)
	assert.Equal("PRRT_1", threads[0].NodeID)
	assert.False(threads[0].IsResolved)
	assert.False(threads[0].IsOutdated)
	assert.Equal("src/main.go", threads[0].Path)
	assert.Equal("RIGHT", threads[0].Side)
	require.NotNil(threads[0].StartLine)
	assert.Equal(10, *threads[0].StartLine)
	assert.Equal(12, threads[0].Line)
	require.Len(threads[0].Comments, 2)
	assert.Equal(int64(3312100450), threads[0].Comments[0].DatabaseID)
	assert.Equal(int64(201), threads[0].Comments[0].ReviewDatabaseID)
	assert.Equal("inline note", threads[0].Comments[0].Body)
	assert.Equal("LINE", threads[0].Comments[0].SubjectType)
	assert.Equal("reviewer", threads[0].Comments[0].AuthorLogin)
	assert.Equal("head-sha", threads[0].Comments[0].CommitID)
	assert.Equal("original-sha", threads[0].Comments[0].OriginalCommitID)
	assert.True(threads[0].Comments[0].IsMinimized)
	assert.Equal("OFF_TOPIC", threads[0].Comments[0].MinimizedReason)
	assert.Equal(int64(3312100451), threads[0].Comments[1].DatabaseID)
	assert.Equal("reply note", threads[0].Comments[1].Body)
	assert.Equal("maintainer", threads[0].Comments[1].AuthorLogin)
	assert.True(threads[0].Comments[1].IsMinimized)
	assert.Equal("ABUSE", threads[0].Comments[1].MinimizedReason)
	assert.True(threads[1].IsResolved)
	assert.True(threads[1].IsOutdated)
	assert.Equal("LEFT", threads[1].Side)
	assert.Equal("FILE", threads[1].Comments[0].SubjectType)
	assert.Equal(3, calls)
	assert.Equal([]string{http.MethodPost, http.MethodPost, http.MethodPost}, methods)
	assert.Equal([]string{"application/json", "application/json", "application/json"}, contentTypes)
	require.Len(queries, 3)
	for _, query := range queries {
		assert.Contains(query, "isMinimized")
		assert.Contains(query, "minimizedReason")
	}
}

func TestListPullRequestReviewThreadsScopesPaginatedCommentAuthByOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls int
	var minted int
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal("Bearer app-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"edges":[{"cursor":"thread-1","node":{"id":"PRRT_1","isResolved":false,"isOutdated":false,"path":"src/main.go","line":12,"originalLine":12,"diffSide":"RIGHT","comments":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"comment-cursor-1"}}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"node":{"comments":{"nodes":[{"id":"PRRC_1","databaseId":101,"fullDatabaseId":101,"body":"reply","path":"src/main.go","line":12,"originalLine":12,"subjectType":"LINE","diffHunk":"@@","url":"https://github.example/pr#discussion_r101","author":{"login":"reviewer"},"createdAt":"2026-05-27T16:01:31Z","updatedAt":"2026-05-27T16:02:31Z"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src := tokenauth.NewManagedSource(tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: "github", Host: "github.com"},
		Candidates: []tokenauth.Candidate{{
			Kind:                tokenauth.SourceKindGitHubApp,
			Host:                "github.com",
			AppID:               42,
			InstallationID:      7,
			InstallationAccount: "owner",
		}},
	}, tokenauth.Options{
		GitHubApp: func(_ context.Context, c tokenauth.Candidate) (string, time.Time, error) {
			minted++
			assert.Equal("owner", c.InstallationAccount)
			return "app-token", time.Now().Add(time.Hour), nil
		},
	})
	auth := platform.AuthTransport{
		Source:        src,
		Base:          http.DefaultTransport,
		SetHeader:     platform.BearerAuthHeader,
		AllowedOrigin: srv.URL,
	}
	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		httpClient:      &http.Client{Transport: auth},
		graphQLEndpoint: srv.URL + "/graphql",
		auth: Authentication{Context: func(ctx context.Context, owner string, _ bool) context.Context {
			return tokenauth.WithGitHubOwner(ctx, owner)
		}},
	}

	threads, err := c.ListPullRequestReviewThreads(t.Context(), "owner", "repo", 42)
	require.NoError(err)
	require.Len(threads, 1)
	require.Len(threads[0].Comments, 1)
	assert.Equal("reply", threads[0].Comments[0].Body)
	assert.Equal(2, calls)
	assert.Equal(1, minted)
}

func TestListPullRequestTimelineEventsReturnsGraphQLErrors(t *testing.T) {
	require := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"message":"permission denied"}],"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
	}))
	defer srv.Close()

	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		httpClient:      srv.Client(),
		graphQLEndpoint: srv.URL,
	}

	events, err := c.ListPullRequestTimelineEvents(t.Context(), "owner", "repo", 42)
	require.Nil(events)
	require.ErrorContains(err, "permission denied")
}

func TestTimelineEventsPreserveGitHubHTTPErrorResponse(t *testing.T) {
	tests := []struct {
		name string
		list func(*Client) ([]PullRequestTimelineEvent, error)
	}{
		{
			name: "pull request",
			list: func(c *Client) ([]PullRequestTimelineEvent, error) {
				return c.ListPullRequestTimelineEvents(t.Context(), "owner", "repo", 42)
			},
		},
		{
			name: "issue",
			list: func(c *Client) ([]PullRequestTimelineEvent, error) {
				return c.ListIssueTimelineEvents(t.Context(), "owner", "repo", 42)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusGone)
				_, _ = w.Write([]byte(`{"message":"Issues are disabled for this repo"}`))
			}))
			defer srv.Close()

			c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx }, httpClient: srv.Client(), graphQLEndpoint: srv.URL}
			events, err := tc.list(c)
			require.Nil(events)
			require.Error(err)
			var responseErr *gh.ErrorResponse
			require.ErrorAs(err, &responseErr)
			assert.Equal(http.StatusGone, responseErr.Response.StatusCode)
			assert.Equal("Issues are disabled for this repo", responseErr.Message)
		})
	}
}

func TestListIssueTimelineEvents(t *testing.T) {
	require := require.New(t)
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"data":{"repository":{"issue":{"timelineItems":{"nodes":[{"__typename":"AssignedEvent","id":"AE_1","actor":{"login":"wesm"},"assignee":{"__typename":"User","login":"wesm"},"createdAt":"2024-06-01T12:20:00Z"}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"issue":{"timelineItems":{"nodes":[{"__typename":"UnassignedEvent","id":"UE_1","actor":{"login":"alice"},"assignee":{"__typename":"Mannequin","login":"bob"},"createdAt":"2024-06-01T12:25:00Z"},{"__typename":"CrossReferencedEvent","id":"CRE_1","actor":{"login":"mariusvniekerk"},"createdAt":"2024-06-01T12:30:00Z","isCrossRepository":false,"willCloseTarget":false,"source":{"__typename":"PullRequest","number":860,"title":"Add global review guidelines","url":"https://github.com/kenn-io/roborev/pull/860","repository":{"owner":{"login":"kenn-io"},"name":"roborev"}}},{"__typename":"ClosedEvent","id":"CE_1","actor":{"login":"closer"},"createdAt":"2024-06-01T12:35:00Z"},{"__typename":"ReopenedEvent","id":"RE_1","actor":{"login":"opener"},"createdAt":"2024-06-01T12:40:00Z"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		httpClient:      srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	events, err := c.ListIssueTimelineEvents(t.Context(), "owner", "repo", 42)
	require.NoError(err)
	require.Len(events, 5)
	require.Equal("assigned", events[0].EventType)
	require.Equal("AE_1", events[0].NodeID)
	require.Equal("wesm", events[0].Actor)
	require.Equal("wesm", events[0].Assignee)
	require.Equal("unassigned", events[1].EventType)
	require.Equal("UE_1", events[1].NodeID)
	require.Equal("alice", events[1].Actor)
	require.Equal("bob", events[1].Assignee)
	require.Equal("cross_referenced", events[2].EventType)
	require.Equal("CRE_1", events[2].NodeID)
	require.Equal("mariusvniekerk", events[2].Actor)
	require.Equal("PullRequest", events[2].SourceType)
	require.Equal("kenn-io", events[2].SourceOwner)
	require.Equal("roborev", events[2].SourceRepo)
	require.Equal(860, events[2].SourceNumber)
	require.Equal("Add global review guidelines", events[2].SourceTitle)
	require.Equal("https://github.com/kenn-io/roborev/pull/860", events[2].SourceURL)
	require.Equal("closed", events[3].EventType)
	require.Equal("CE_1", events[3].NodeID)
	require.Equal("closer", events[3].Actor)
	require.Equal("reopened", events[4].EventType)
	require.Equal("RE_1", events[4].NodeID)
	require.Equal("opener", events[4].Actor)
	require.Equal(2, calls)
}

func TestListPullRequestTimelineEventsRejectsNullGraphQLNodes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "null repository",
			body: `{"data":{"repository":null}}`,
			want: "missing repository",
		},
		{
			name: "null pull request",
			body: `{"data":{"repository":{"pullRequest":null}}}`,
			want: "missing pull request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
				httpClient:      srv.Client(),
				graphQLEndpoint: srv.URL,
			}

			events, err := c.ListPullRequestTimelineEvents(t.Context(), "owner", "repo", 42)
			require.Nil(events)
			require.ErrorContains(err, tt.want)
		})
	}
}

func TestMarkPullRequestReadyForReviewUsesGraphQLMutation(t *testing.T) {
	require := require.New(t)
	var calls int
	var methods []string
	var contentTypes []string
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		calls++
		methods = append(methods, r.Method)
		contentTypes = append(contentTypes, r.Header.Get("Content-Type"))
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"id":"PR_kwDOAAABc84"}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"markPullRequestReadyForReview":{"pullRequest":{"databaseId":1001,"number":141,"title":"Ready PR","state":"OPEN","isDraft":false,"body":"body","url":"https://github.com/wesm/kenn-forge/pull/141","author":{"login":"wesm"},"createdAt":"2026-04-14T00:00:00Z","updatedAt":"2026-04-14T00:05:00Z","mergedAt":null,"closedAt":null,"additions":12,"deletions":3,"mergeable":"MERGEABLE","reviewDecision":"APPROVED","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","baseRefOid":"def456","headRepository":{"url":"https://github.com/wesm/kenn-forge"},"labels":{"nodes":[]}}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(), srv.URL+"/api/v3/", srv.URL+"/api/uploads/")
	require.NoError(err)

	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:              ghClient,
		httpClient:      srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	pr, err := c.MarkPullRequestReadyForReview(t.Context(), "wesm", "kenn-forge", 141)
	require.NoError(err)
	require.NotNil(pr)
	require.Equal(141, pr.GetNumber())
	require.Equal("Ready PR", pr.GetTitle())
	require.False(pr.GetDraft())
	require.Equal(2, calls)
	require.Equal([]string{http.MethodPost, http.MethodPost}, methods)
	require.Equal([]string{"application/json", "application/json"}, contentTypes)
}

func TestConvertPullRequestToDraftUsesGraphQLMutation(t *testing.T) {
	require := require.New(t)
	updatedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	var calls int
	var methods []string
	var contentTypes []string
	var requestBodies []string
	var readBodyErr error
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		calls++
		methods = append(methods, r.Method)
		contentTypes = append(contentTypes, r.Header.Get("Content-Type"))
		body, err := io.ReadAll(r.Body)
		if err != nil {
			readBodyErr = err
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		requestBodies = append(requestBodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"id":"PR_kwDOAAABc84"}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"convertPullRequestToDraft":{"pullRequest":{"id":"PR_kwDOAAABc84","updatedAt":"` + updatedAt.Format(time.RFC3339) + `"}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(), srv.URL+"/api/v3/", srv.URL+"/api/uploads/")
	require.NoError(err)

	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:              ghClient,
		httpClient:      srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	pr, err := c.ConvertPullRequestToDraft(t.Context(), "wesm", "kenn-forge", 141)
	require.NoError(err)
	require.NotNil(pr)
	require.Equal(141, pr.GetNumber())
	require.True(pr.GetDraft())
	require.Equal(updatedAt, pr.GetUpdatedAt().UTC())
	require.Equal(2, calls)
	require.Equal([]string{http.MethodPost, http.MethodPost}, methods)
	require.Equal([]string{"application/json", "application/json"}, contentTypes)
	require.NoError(readBodyErr)
	require.Len(requestBodies, 2)
	require.Contains(requestBodies[1], "convertPullRequestToDraft")
	require.Contains(requestBodies[1], "updatedAt")
	require.NotContains(requestBodies[1], "reviewDecision")
	require.NotContains(requestBodies[1], "databaseId")
}

func TestMarkPullRequestReadyForReviewReturnsTypedStaleStateError(t *testing.T) {
	require := require.New(t)
	call := 0
	var methods []string
	var contentTypes []string
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		call++
		methods = append(methods, r.Method)
		contentTypes = append(contentTypes, r.Header.Get("Content-Type"))
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"id":"PR_kwDOAAABc84"}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"markPullRequestReadyForReview":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a PullRequest with the global id of 'PR_kwDOAAABc84'."}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ghClient, err := newEnterpriseGHClient(srv.Client(), srv.URL+"/api/v3/", srv.URL+"/api/uploads/")
	require.NoError(err)

	c := &Client{now: time.Now, graphQLContext: func(ctx context.Context) context.Context { return ctx },
		gh:              ghClient,
		httpClient:      srv.Client(),
		graphQLEndpoint: srv.URL + "/graphql",
	}

	pr, err := c.MarkPullRequestReadyForReview(t.Context(), "wesm", "kenn-forge", 141)
	require.Nil(pr)
	require.Error(err)
	require.ErrorContains(err, "Could not resolve to a PullRequest")

	var statusErr interface{ StatusCode() int }
	require.ErrorAs(err, &statusErr, "expected status-bearing error, got %T", err)
	require.Equal(http.StatusNotFound, statusErr.StatusCode())

	var staleErr interface{ IsStaleState() bool }
	require.ErrorAs(err, &staleErr, "expected stale-state error, got %T", err)
	require.True(staleErr.IsStaleState())
	require.Equal(2, call)
	require.Equal([]string{http.MethodPost, http.MethodPost}, methods)
	require.Equal([]string{"application/json", "application/json"}, contentTypes)
}

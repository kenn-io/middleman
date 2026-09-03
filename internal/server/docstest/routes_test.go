package docstest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/docs"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
)

func setupDocsRouteServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	root := t.TempDir()
	mustWrite := func(rel, body string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	mustWrite("README.md", "# Readme\nbudget overview\n")
	mustWrite("notes/daily.md", "# Daily\nbudget item\n")
	mustWrite("notes/ideas.md", "# Ideas\n")
	mustWrite("notes/image.png", string([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}))

	cfg := &config.Config{
		Host: "127.0.0.1",
		Port: 8091,
		DocFolders: []config.DocFolder{
			{ID: "notes", Name: "Notes", Path: root, Daemon: "work"},
		},
	}
	srv := servertest.New(t, dbtest.Open(t), nil, nil, "/", cfg, server.ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
	})
	return srv, root
}

func setupPersistentDocsRouteServer(t *testing.T) (*server.Server, string, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		SyncInterval: "5m",
		Host:         "127.0.0.1",
		Port:         8091,
		DocFolders: []config.DocFolder{
			{ID: "notes", Name: "Notes", Path: root},
		},
	}
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, cfg.Save(cfgPath))
	loaded, err := config.Load(cfgPath)
	require.NoError(t, err)
	srv := servertest.NewWithConfig(t, dbtest.Open(t), nil, nil, nil, loaded, cfgPath, server.ServerOptions{
		HostCheckAllowLoopbackAnyPort: true,
	})
	return srv, root, cfgPath
}

func TestDocsFoldersEndpointListsConfiguredFolders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, root := setupDocsRouteServer(t)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body generated.ListDocsFoldersOutputBody
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	require.NotNil(body.Folders)
	require.Len(body.Folders, 1)
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(err)
	folder := body.Folders[0]
	assert.Equal("notes", folder.Id)
	assert.Equal("Notes", folder.Name)
	assert.Equal(canonicalRoot, folder.Path)
	require.NotNil(folder.Daemon)
	assert.Equal("work", *folder.Daemon)
}

func TestDocsFolderConfigEndpointsAddRenameRemoveAndPersist(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, cfgPath := setupPersistentDocsRouteServer(t)
	extraRoot := t.TempDir()

	addRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders", generated.CreateDocsFolderJSONRequestBody{
		Id:     new("extra"),
		Path:   new(extraRoot),
		Daemon: new("work"),
	})

	require.Equal(http.StatusCreated, addRR.Code, addRR.Body.String())
	var addedBody generated.DocsFolderOutputBody
	require.NoError(json.NewDecoder(addRR.Body).Decode(&addedBody))
	added := addedBody.Folder
	assert.Equal("extra", added.Id)
	assert.Equal(filepath.Base(extraRoot), added.Name)
	require.NotNil(added.Daemon)
	assert.Equal("work", *added.Daemon)
	wantExtraRoot, err := filepath.EvalSymlinks(extraRoot)
	require.NoError(err)
	assert.Equal(wantExtraRoot, added.Path)

	renameRR := testutil.DoJSON(t, srv, http.MethodPatch, "/api/v1/docs/folders/extra", generated.UpdateDocsFolderJSONRequestBody{
		Name: new("Reference"),
	})

	require.Equal(http.StatusOK, renameRR.Code, renameRR.Body.String())
	var renamedBody generated.DocsFolderOutputBody
	require.NoError(json.NewDecoder(renameRR.Body).Decode(&renamedBody))
	renamed := renamedBody.Folder
	assert.Equal("extra", renamed.Id)
	assert.Equal("Reference", renamed.Name)

	deleteRR := testutil.DoJSON(t, srv, http.MethodDelete, "/api/v1/docs/folders/notes", nil)
	require.Equal(http.StatusNoContent, deleteRR.Code, deleteRR.Body.String())

	listRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders", nil)
	require.Equal(http.StatusOK, listRR.Code, listRR.Body.String())
	var listBody generated.ListDocsFoldersOutputBody
	require.NoError(json.NewDecoder(listRR.Body).Decode(&listBody))
	require.NotNil(listBody.Folders)
	require.Len(listBody.Folders, 1)
	assert.Equal("extra", listBody.Folders[0].Id)
	assert.Equal("Reference", listBody.Folders[0].Name)

	reloaded, err := config.Load(cfgPath)
	require.NoError(err)
	require.Len(reloaded.DocFolders, 1)
	assert.Equal(config.DocFolder{ID: "extra", Name: "Reference", Path: wantExtraRoot, Daemon: "work"}, reloaded.DocFolders[0])
}

func TestDocsFolderAddRejectsNonLoopback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, _ := setupPersistentDocsRouteServer(t)

	body, err := json.Marshal(generated.CreateDocsFolderJSONRequestBody{Path: new("/tmp/whatever")})
	require.NoError(err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/docs/folders", bytes.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusForbidden, rr.Code, rr.Body.String())
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal(httpapi.CodeForbidden, problem.Code)
	assert.Equal("loopbackOnly", problem.Details["reason"])
	assert.Contains(problem.Detail, "docs mutations require a loopback client")
}

func TestDocsFolderAddDerivesIDAndRejectsInvalidRequests(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, _ := setupPersistentDocsRouteServer(t)
	extraRoot := filepath.Join(t.TempDir(), "Research Papers!")
	require.NoError(os.Mkdir(extraRoot, 0o755))

	addRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders", generated.CreateDocsFolderJSONRequestBody{
		Path: new(extraRoot),
	})

	require.Equal(http.StatusCreated, addRR.Code, addRR.Body.String())
	var addedBody generated.DocsFolderOutputBody
	require.NoError(json.NewDecoder(addRR.Body).Decode(&addedBody))
	added := addedBody.Folder
	assert.Equal("research-papers", added.Id)

	collidingRoot := filepath.Join(t.TempDir(), "Notes")
	require.NoError(os.Mkdir(collidingRoot, 0o755))
	collisionRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders", generated.CreateDocsFolderJSONRequestBody{
		Path: new(collidingRoot),
	})

	require.Equal(http.StatusCreated, collisionRR.Code, collisionRR.Body.String())
	var collisionBody generated.DocsFolderOutputBody
	require.NoError(json.NewDecoder(collisionRR.Body).Decode(&collisionBody))
	collision := collisionBody.Folder
	assert.Equal("notes-2", collision.Id)
	assert.Equal("Notes", collision.Name)

	spacedRoot := t.TempDir()
	wantSpacedRoot, err := filepath.EvalSymlinks(spacedRoot)
	require.NoError(err)
	spacedRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders", generated.CreateDocsFolderJSONRequestBody{
		Id:   new(" spaced "),
		Name: new(" Spaced "),
		Path: new(" " + spacedRoot + " "),
	})

	require.Equal(http.StatusCreated, spacedRR.Code, spacedRR.Body.String())
	var spacedBody generated.DocsFolderOutputBody
	require.NoError(json.NewDecoder(spacedRR.Body).Decode(&spacedBody))
	spaced := spacedBody.Folder
	assert.Equal("spaced", spaced.Id)
	assert.Equal("Spaced", spaced.Name)
	assert.Equal(wantSpacedRoot, spaced.Path)

	duplicateRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders", generated.CreateDocsFolderJSONRequestBody{
		Id:   new("notes"),
		Path: new(filepath.Join(t.TempDir(), "missing")),
	})

	assert.Equal(http.StatusConflict, duplicateRR.Code, duplicateRR.Body.String())

	missingRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders", generated.CreateDocsFolderJSONRequestBody{
		Id:   new("ghost"),
		Path: new(filepath.Join(t.TempDir(), "missing")),
	})

	assert.Equal(http.StatusNotFound, missingRR.Code, missingRR.Body.String())

	trimmedDuplicateRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders", generated.CreateDocsFolderJSONRequestBody{
		Id:   new(" notes "),
		Path: new(t.TempDir()),
	})

	assert.Equal(http.StatusConflict, trimmedDuplicateRR.Code, trimmedDuplicateRR.Body.String())

	blankPathRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders", generated.CreateDocsFolderJSONRequestBody{
		Id:   new("blank"),
		Path: new(" \t"),
	})

	assert.Equal(http.StatusBadRequest, blankPathRR.Code, blankPathRR.Body.String())

	blankNameRR := testutil.DoJSON(t, srv, http.MethodPatch, "/api/v1/docs/folders/notes", generated.UpdateDocsFolderJSONRequestBody{
		Name: new(" \t"),
	})

	assert.Equal(http.StatusBadRequest, blankNameRR.Code, blankNameRR.Body.String())

	// An explicit id that cannot be addressed as a single path segment is
	// rejected up front instead of persisting an unreachable folder.
	slashIDRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders", generated.CreateDocsFolderJSONRequestBody{
		Id:   new("team/docs"),
		Path: new(t.TempDir()),
	})

	require.Equal(http.StatusBadRequest, slashIDRR.Code, slashIDRR.Body.String())
	var slashIDProblem httpapi.ProblemError
	require.NoError(json.NewDecoder(slashIDRR.Body).Decode(&slashIDProblem))
	assert.Equal("invalidFolder", slashIDProblem.Details["reason"])
}

func TestDocsFolderMutationsRequireConfigPersistenceAndRollbackOnSaveFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)

	unavailableRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders", generated.CreateDocsFolderJSONRequestBody{
		Id:   new("extra"),
		Path: new(t.TempDir()),
	})

	require.Equal(http.StatusNotFound, unavailableRR.Code, unavailableRR.Body.String())
	var unavailable httpapi.ProblemError
	require.NoError(json.NewDecoder(unavailableRR.Body).Decode(&unavailable))
	assert.Equal(httpapi.CodeSettingsUnavailable, unavailable.Code)

	root := t.TempDir()
	cfg := &config.Config{
		SyncInterval: "5m",
		BasePath:     "/",
		Host:         "0.0.0.0",
		Port:         8091,
		DocFolders:   []config.DocFolder{{ID: "notes", Name: "Notes", Path: root}},
	}
	badPath := filepath.Join(t.TempDir(), "config.toml")
	failSrv := servertest.NewWithConfig(t, dbtest.Open(t), nil, nil, nil, cfg, badPath, server.ServerOptions{
		HostCheck: server.HostCheckOptions{
			Bind: config.HostKey{Host: "127.0.0.1", Port: "8091"},
		},
		HostCheckAllowLoopbackAnyPort: true,
	})

	rollbackRR := testutil.DoJSON(t, failSrv, http.MethodPost, "/api/v1/docs/folders", generated.CreateDocsFolderJSONRequestBody{
		Id:   new("rollback"),
		Path: new(t.TempDir()),
	})

	require.Equal(http.StatusInternalServerError, rollbackRR.Code, rollbackRR.Body.String())
	listRR := testutil.DoJSON(t, failSrv, http.MethodGet, "/api/v1/docs/folders", nil)
	require.Equal(http.StatusOK, listRR.Code, listRR.Body.String())
	var listBody generated.ListDocsFoldersOutputBody
	require.NoError(json.NewDecoder(listRR.Body).Decode(&listBody))
	require.NotNil(listBody.Folders)
	require.Len(listBody.Folders, 1)
	assert.Equal("notes", listBody.Folders[0].Id)
}

func TestDocsBrowseEndpointListsDirectoriesOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)
	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "alpha"), 0o755))
	require.NoError(os.MkdirAll(filepath.Join(root, ".hidden"), 0o755))
	require.NoError(os.WriteFile(filepath.Join(root, "skipme.md"), []byte("hi"), 0o644))

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/browse?path="+root, nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body generated.DocsBrowseOutputBody
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(root, body.Path)
	require.NotNil(body.Entries)
	byName := make(map[string]bool, len(body.Entries))
	for _, entry := range body.Entries {
		byName[entry.Name] = entry.Hidden
		assert.True(filepath.IsAbs(entry.Path))
	}
	_, hasAlpha := byName["alpha"]
	assert.True(hasAlpha)
	hidden, hasHidden := byName[".hidden"]
	require.True(hasHidden)
	assert.True(hidden)
	_, hasFile := byName["skipme.md"]
	assert.False(hasFile)
}

func TestDocsBrowseEndpointExpandsHomeShortcut(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/browse?path=~", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body generated.DocsBrowseOutputBody
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(home, body.Path)
}

func TestDocsBrowseEndpointRejectsNonLoopback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/docs/browse?path="+t.TempDir(), nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "203.0.113.7:54321"
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusForbidden, rr.Code, rr.Body.String())
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal(httpapi.CodeForbidden, problem.Code)
	assert.Equal("loopbackOnly", problem.Details["reason"])
}

func TestDocsTreeEndpointListsMarkdownOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/notes/tree", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	raw := rr.Body.String()
	var body docs.Node
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal("Notes", body.Name)
	assert.Contains(raw, `"rel_path":"README.md"`)
	assert.Contains(raw, `"rel_path":"notes/daily.md"`)
	assert.NotContains(raw, "image.png")
}

func TestDocsFileEndpointReadsAndWritesMarkdown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, root := setupDocsRouteServer(t)

	readRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/notes/file?path=notes/ideas.md", nil)
	require.Equal(http.StatusOK, readRR.Code, readRR.Body.String())
	var readBody generated.DocsReadFileOutputBody
	require.NoError(json.NewDecoder(readRR.Body).Decode(&readBody))
	assert.Equal("notes/ideas.md", readBody.RelPath)
	assert.Equal("# Ideas\n", readBody.Content)

	writeRR := testutil.DoJSON(t, srv, http.MethodPut, "/api/v1/docs/folders/notes/file?path=notes/ideas.md", generated.WriteDocsFileJSONRequestBody{
		Content: new("# Updated\n"),
	})

	require.Equal(http.StatusOK, writeRR.Code, writeRR.Body.String())
	var writeBody generated.DocsFileWriteBody
	require.NoError(json.NewDecoder(writeRR.Body).Decode(&writeBody))
	assert.Equal("notes/ideas.md", writeBody.RelPath)
	assert.Equal(int64(len("# Updated\n")), writeBody.Size)
	got, err := os.ReadFile(filepath.Join(root, "notes/ideas.md"))
	require.NoError(err)
	assert.Equal("# Updated\n", string(got))
}

func TestDocsSearchEndpointsReturnArrays(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)

	folderRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/notes/search?q=daily&limit=10", nil)
	require.Equal(http.StatusOK, folderRR.Code, folderRR.Body.String())
	var folderBody generated.DocsSearchOutputBody
	require.NoError(json.NewDecoder(folderRR.Body).Decode(&folderBody))
	assert.Equal("daily", folderBody.Query)
	require.NotNil(folderBody.Hits)
	assert.NotEmpty(folderBody.Hits)

	globalRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/search?q=budget&limit=10", nil)
	require.Equal(http.StatusOK, globalRR.Code, globalRR.Body.String())
	var globalBody generated.DocsSearchAllOutputBody
	require.NoError(json.NewDecoder(globalRR.Body).Decode(&globalBody))
	assert.Equal("budget", globalBody.Query)
	require.NotNil(globalBody.Hits)
	assert.NotEmpty(globalBody.Hits)
	assert.False(globalBody.Truncated)
}

func TestDocsFileCreateDeleteRenameAndBlob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, root := setupDocsRouteServer(t)

	createRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/notes/file?path=notes/new.md", generated.CreateDocsFileJSONRequestBody{
		Content: new("# New\n"),
	})

	require.Equal(http.StatusCreated, createRR.Code, createRR.Body.String())
	var createBody generated.DocsFileWriteBody
	require.NoError(json.NewDecoder(createRR.Body).Decode(&createBody))
	assert.Equal("notes/new.md", createBody.RelPath)
	assert.Equal(int64(len("# New\n")), createBody.Size)

	duplicateRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/notes/file?path=notes/new.md", generated.CreateDocsFileJSONRequestBody{
		Content: new("# New\n"),
	})

	assert.Equal(http.StatusConflict, duplicateRR.Code, duplicateRR.Body.String())

	emptyRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/notes/file?path=notes/empty.md", generated.CreateDocsFileJSONRequestBody{})
	require.Equal(http.StatusCreated, emptyRR.Code, emptyRR.Body.String())
	emptyReadRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/notes/file?path=notes/empty.md", nil)
	require.Equal(http.StatusOK, emptyReadRR.Code, emptyReadRR.Body.String())
	var emptyReadBody generated.DocsReadFileOutputBody
	require.NoError(json.NewDecoder(emptyReadRR.Body).Decode(&emptyReadBody))
	assert.Empty(emptyReadBody.Content)

	renameRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/notes/file/actions/rename", generated.RenameDocsFileJSONRequestBody{
		From: new("notes/new.md"),
		To:   new("notes/renamed.md"),
	})

	require.Equal(http.StatusOK, renameRR.Code, renameRR.Body.String())
	var renameBody generated.DocsRenameFileOutputBody
	require.NoError(json.NewDecoder(renameRR.Body).Decode(&renameBody))
	assert.Equal("notes/new.md", renameBody.From)
	assert.Equal("notes/renamed.md", renameBody.To)

	renamedReadRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/notes/file?path=notes/renamed.md", nil)
	require.Equal(http.StatusOK, renamedReadRR.Code, renamedReadRR.Body.String())
	var renamedReadBody generated.DocsReadFileOutputBody
	require.NoError(json.NewDecoder(renamedReadRR.Body).Decode(&renamedReadBody))
	assert.Equal("# New\n", renamedReadBody.Content)

	blobRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/notes/blob?path=notes/image.png", nil)
	require.Equal(http.StatusOK, blobRR.Code, blobRR.Body.String())
	assert.Equal("image/png", blobRR.Header().Get("Content-Type"))
	assert.Equal("private, max-age=60", blobRR.Header().Get("Cache-Control"))
	assert.NotEmpty(blobRR.Body.Bytes())

	deleteRR := testutil.DoJSON(t, srv, http.MethodDelete, "/api/v1/docs/folders/notes/file?path=notes/renamed.md", nil)
	require.Equal(http.StatusNoContent, deleteRR.Code, deleteRR.Body.String())
	_, err := os.Stat(filepath.Join(root, "notes/renamed.md"))
	require.ErrorIs(err, os.ErrNotExist)
	deletedReadRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/notes/file?path=notes/renamed.md", nil)
	assert.Equal(http.StatusNotFound, deletedReadRR.Code, deletedReadRR.Body.String())
}

func TestDocsBlobOpenAPIResponseIsBinary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	doc := server.NewOpenAPI()
	item := doc.Paths["/docs/folders/{id}/blob"]
	require.NotNil(item)
	require.NotNil(item.Get)
	resp := item.Get.Responses["200"]
	require.NotNil(resp)

	assert.Contains(resp.Content, "application/octet-stream")
	assert.NotContains(resp.Content, "application/json")
	schema := resp.Content["application/octet-stream"].Schema
	require.NotNil(schema)
	assert.Equal("string", schema.Type)
	assert.Equal("binary", schema.Format)
}

func TestDocsFileEndpointRejectsInvalidPathsAndTypes(t *testing.T) {
	assert := assert.New(t)
	srv, _ := setupDocsRouteServer(t)

	unknownRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/missing/tree", nil)
	assert.Equal(http.StatusNotFound, unknownRR.Code, unknownRR.Body.String())

	traversalRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/notes/file?path=../../escape", nil)
	assert.Equal(http.StatusForbidden, traversalRR.Code, traversalRR.Body.String())

	createTextRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/notes/file?path=notes/bad.txt", generated.CreateDocsFileJSONRequestBody{
		Content: new("x"),
	})

	assert.Equal(http.StatusUnsupportedMediaType, createTextRR.Code, createTextRR.Body.String())

	deleteTextRR := testutil.DoJSON(t, srv, http.MethodDelete, "/api/v1/docs/folders/notes/file?path=notes/bad.txt", nil)
	assert.Equal(http.StatusUnsupportedMediaType, deleteTextRR.Code, deleteTextRR.Body.String())

	blobMarkdownRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/notes/blob?path=README.md", nil)
	assert.Equal(http.StatusUnsupportedMediaType, blobMarkdownRR.Code, blobMarkdownRR.Body.String())
}

func TestDocsSearchEndpointOmitsLineForFilenameOnlyHits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, root := setupDocsRouteServer(t)
	require.NoError(os.WriteFile(filepath.Join(root, "budget.md"), []byte("unrelated content\n"), 0o644))
	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/search?q=budget", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body generated.DocsSearchAllOutputBody
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	require.Len(body.Hits, 3)
	for _, hit := range body.Hits {
		if hit.RelPath == "budget.md" {
			assert.Equal("filename", hit.HitType)
			assert.Nil(hit.Line)
		} else {
			assert.Equal("body", hit.HitType)
			require.NotNil(hit.Line)
			assert.Equal(int64(2), *hit.Line)
		}
	}
}

func TestDocsSearchEndpointEmptyQueryReturnsEmptyArray(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/search?q=&limit=10", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body generated.DocsSearchAllOutputBody
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	require.NotNil(body.Hits)
	assert.Empty(body.Hits)
}

func TestDocsSearchEndpointTruncationAndFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)

	truncatedRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/search?q=budget&limit=1", nil)
	require.Equal(http.StatusOK, truncatedRR.Code, truncatedRR.Body.String())
	var truncatedBody generated.DocsSearchAllOutputBody
	require.NoError(json.NewDecoder(truncatedRR.Body).Decode(&truncatedBody))
	require.NotNil(truncatedBody.Hits)
	assert.Len(truncatedBody.Hits, 1)
	assert.True(truncatedBody.Truncated)

	missingA := filepath.Join(t.TempDir(), "missing-a")
	missingB := filepath.Join(t.TempDir(), "missing-b")
	failSrv := servertest.New(t, dbtest.Open(t), nil, nil, "/", &config.Config{
		Host: "127.0.0.1",
		Port: 8091,
		DocFolders: []config.DocFolder{
			{ID: "a", Name: "A", Path: missingA},
			{ID: "b", Name: "B", Path: missingB},
		},
	}, server.ServerOptions{HostCheckAllowLoopbackAnyPort: true})
	failRR := testutil.DoJSON(t, failSrv, http.MethodGet, "/api/v1/docs/search?q=budget&limit=10", nil)
	assert.Equal(http.StatusInternalServerError, failRR.Code, failRR.Body.String())
}

func TestDocsSearchEndpointSerializesPartialWarnings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	goodRoot := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(goodRoot, "hit.md"), []byte("budget partial\n"), 0o644))
	missingRoot := filepath.Join(t.TempDir(), "missing")
	srv := servertest.New(t, dbtest.Open(t), nil, nil, "/", &config.Config{
		Host: "127.0.0.1",
		Port: 8091,
		DocFolders: []config.DocFolder{
			{ID: "good", Name: "Good", Path: goodRoot},
			{ID: "missing", Name: "Missing", Path: missingRoot},
		},
	}, server.ServerOptions{HostCheckAllowLoopbackAnyPort: true})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/search?q=budget&limit=10", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body generated.DocsSearchAllOutputBody
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	require.NotNil(body.Hits)
	require.Len(body.Hits, 1)
	assert.Equal("good", body.Hits[0].Folder)
	require.NotNil(body.Warnings)
	assert.NotEmpty(*body.Warnings)
	assert.False(body.Truncated)
}

func TestDocsSearchEndpointFindsHitsAcrossFolders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	rootA := t.TempDir()
	rootB := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(rootA, "a.md"), []byte("budget alpha\n"), 0o644))
	require.NoError(os.WriteFile(filepath.Join(rootB, "b.md"), []byte("budget beta\n"), 0o644))
	srv := servertest.New(t, dbtest.Open(t), nil, nil, "/", &config.Config{
		Host: "127.0.0.1",
		Port: 8091,
		DocFolders: []config.DocFolder{
			{ID: "a", Name: "A", Path: rootA},
			{ID: "b", Name: "B", Path: rootB},
		},
	}, server.ServerOptions{HostCheckAllowLoopbackAnyPort: true})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/search?q=budget&limit=10", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body generated.DocsSearchAllOutputBody
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	require.NotNil(body.Hits)
	require.Len(body.Hits, 2)
	ids := []string{body.Hits[0].Folder, body.Hits[1].Folder}
	assert.ElementsMatch([]string{"a", "b"}, ids)
	assert.False(body.Truncated)
}

func TestDocsFileMutationsRejectNonLoopback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{
			name:   "write",
			method: http.MethodPut,
			path:   "/api/v1/docs/folders/notes/file?path=notes/ideas.md",
			body:   generated.WriteDocsFileJSONRequestBody{Content: new("blocked")},
		},
		{
			name:   "create",
			method: http.MethodPost,
			path:   "/api/v1/docs/folders/notes/file?path=notes/blocked.md",
			body:   generated.CreateDocsFileJSONRequestBody{Content: new("blocked")},
		},
		{
			name:   "delete",
			method: http.MethodDelete,
			path:   "/api/v1/docs/folders/notes/file?path=notes/ideas.md",
		},
		{
			name:   "rename",
			method: http.MethodPost,
			path:   "/api/v1/docs/folders/notes/file/actions/rename",
			body: generated.RenameDocsFileJSONRequestBody{
				From: new("notes/ideas.md"),
				To:   new("notes/blocked.md"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if tc.body != nil {
				require.NoError(json.NewEncoder(&buf).Encode(tc.body))
			}
			req := httptest.NewRequest(tc.method, tc.path, &buf)
			req.Host = "127.0.0.1"
			req.RemoteAddr = "203.0.113.7:54321"
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			require.Equal(http.StatusForbidden, rr.Code, rr.Body.String())
			var problem httpapi.ProblemError
			require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
			assert.Equal(httpapi.CodeForbidden, problem.Code)
			assert.Equal("loopbackOnly", problem.Details["reason"])
		})
	}
}

func TestDocsMutationsRejectBodyTooLarge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)
	huge := strings.Repeat("a", 5<<20)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{
			name:   "create folder",
			method: http.MethodPost,
			path:   "/api/v1/docs/folders",
			body:   generated.CreateDocsFolderJSONRequestBody{Path: new(huge)},
		},
		{
			name:   "update folder",
			method: http.MethodPatch,
			path:   "/api/v1/docs/folders/notes",
			body:   generated.UpdateDocsFolderJSONRequestBody{Name: new(huge)},
		},
		{
			name:   "write file",
			method: http.MethodPut,
			path:   "/api/v1/docs/folders/notes/file?path=notes/ideas.md",
			body:   generated.WriteDocsFileJSONRequestBody{Content: new(huge)},
		},
		{
			name:   "create file",
			method: http.MethodPost,
			path:   "/api/v1/docs/folders/notes/file?path=notes/large.md",
			body:   generated.CreateDocsFileJSONRequestBody{Content: new(huge)},
		},
		{
			name:   "rename file",
			method: http.MethodPost,
			path:   "/api/v1/docs/folders/notes/file/actions/rename",
			body: generated.RenameDocsFileJSONRequestBody{
				From: new("notes/ideas.md"),
				To:   new(huge),
			},
		},
		{
			name:   "publish git",
			method: http.MethodPost,
			path:   "/api/v1/docs/folders/notes/git/publish",
			body:   generated.PublishDocsGitJSONRequestBody{Message: new(huge)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			require.NoError(json.NewEncoder(&body).Encode(tc.body))
			req := httptest.NewRequest(tc.method, tc.path, &body)
			req.Host = "127.0.0.1"
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			require.Equal(http.StatusRequestEntityTooLarge, rr.Code, rr.Body.String())
			var problem httpapi.ProblemError
			require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
			assert.Equal(httpapi.CodePayloadTooLarge, problem.Code)
		})
	}
}

func TestDocsFileWriteAllowsBodyBelowEditorLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)
	content := strings.Repeat("a", 2<<20)
	body, err := json.Marshal(generated.WriteDocsFileJSONRequestBody{Content: new(content)})
	require.NoError(err)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/docs/folders/notes/file?path=notes/ideas.md",
		bytes.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var parsed generated.DocsFileWriteBody
	require.NoError(json.NewDecoder(rr.Body).Decode(&parsed))
	assert.Equal("notes/ideas.md", parsed.RelPath)
	assert.Equal(int64(len(content)), parsed.Size)
}

func TestDocsReadEndpointsRejectNonLoopback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := setupDocsRouteServer(t)

	cases := []struct {
		name string
		path string
	}{
		{name: "list", path: "/api/v1/docs/folders"},
		{name: "tree", path: "/api/v1/docs/folders/notes/tree"},
		{name: "file", path: "/api/v1/docs/folders/notes/file?path=notes/ideas.md"},
		{name: "blob", path: "/api/v1/docs/folders/notes/blob?path=notes/image.png"},
		{name: "folder search", path: "/api/v1/docs/folders/notes/search?q=ideas"},
		{name: "global search", path: "/api/v1/docs/search?q=budget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Host = "127.0.0.1"
			req.RemoteAddr = "203.0.113.7:54321"
			rr := httptest.NewRecorder()

			srv.ServeHTTP(rr, req)

			require.Equal(http.StatusForbidden, rr.Code, rr.Body.String())
			var problem httpapi.ProblemError
			require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
			assert.Equal(httpapi.CodeForbidden, problem.Code)
			assert.Equal("loopbackOnly", problem.Details["reason"])
		})
	}
}

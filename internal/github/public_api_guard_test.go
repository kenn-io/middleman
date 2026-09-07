package github

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicGitHubAPIGuardTransportBlocksAPIGitHub(t *testing.T) {
	assert := assert.New(t)
	baseCalls := 0
	transport := publicGitHubAPIGuardTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		baseCalls++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})}
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/rate_limit", nil)
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)

	require.ErrorIs(t, err, ErrPublicGitHubAPIBlocked)
	assert.Nil(resp)
	assert.Equal(0, baseCalls)
}

func TestPublicGitHubAPIGuardTransportAllowsOtherHosts(t *testing.T) {
	assert := assert.New(t)
	baseCalls := 0
	transport := publicGitHubAPIGuardTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		baseCalls++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})}
	req, err := http.NewRequest(http.MethodGet, "https://github.example.com/api/v3/rate_limit", nil)
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(http.StatusNoContent, resp.StatusCode)
	assert.Equal(1, baseCalls)
}

func TestNewClientBlocksPublicGitHubAPIInDefaultTests(t *testing.T) {
	require := require.New(t)

	client, err := NewClient(testTokenSource("fake-token"), "github.com", nil, nil)
	require.NoError(err)
	resp, err := client.ListReleases(t.Context(), "example", "project", 1)

	require.ErrorIs(err, ErrPublicGitHubAPIBlocked)
	require.Nil(resp)
}

func TestRoutedClientExplicitlyImplementsOwnerBearingClientMethods(t *testing.T) {
	// Optional client surfaces are covered too: they are reached by type
	// assertion, and an unrouted one fails that assertion silently on every
	// production host instead of failing to compile.
	interfaces := []struct {
		file string
		name string
	}{
		{file: "client.go", name: "Client"},
		{file: "../../platform/github/native_stacks.go", name: "NativeStackClient"},
	}
	files := token.NewFileSet()
	routerFile, err := parser.ParseFile(files, "auth_router.go", nil, 0)
	require.NoError(t, err)

	routedMethods := map[string]struct{}{}
	for _, decl := range routerFile.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		name, ok := star.X.(*ast.Ident)
		if ok && name.Name == "RoutedClient" {
			routedMethods[fn.Name.Name] = struct{}{}
		}
	}

	for _, target := range interfaces {
		t.Run(target.name, func(t *testing.T) {
			sourceFile, err := parser.ParseFile(files, target.file, nil, 0)
			require.NoError(t, err)
			var missing []string
			ast.Inspect(sourceFile, func(node ast.Node) bool {
				typeSpec, ok := node.(*ast.TypeSpec)
				if !ok || typeSpec.Name.Name != target.name {
					return true
				}
				iface, ok := typeSpec.Type.(*ast.InterfaceType)
				if !ok {
					return false
				}
				for _, field := range iface.Methods.List {
					if len(field.Names) != 1 {
						continue
					}
					fn, ok := field.Type.(*ast.FuncType)
					if !ok || !functionHasParameterNamed(fn, "owner", "repo") {
						continue
					}
					if _, ok := routedMethods[field.Names[0].Name]; !ok {
						missing = append(missing, field.Names[0].Name)
					}
				}
				return false
			})
			assert.Empty(t, missing, "owner-bearing methods must route explicitly")
		})
	}
}

func functionHasParameterNamed(fn *ast.FuncType, names ...string) bool {
	if fn == nil || fn.Params == nil {
		return false
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	for _, field := range fn.Params.List {
		for _, name := range field.Names {
			if _, ok := wanted[name.Name]; ok {
				return true
			}
		}
	}
	return false
}

func TestNewGraphQLFetcherBlocksPublicGitHubAPIInDefaultTests(t *testing.T) {
	fetcher := NewGraphQLFetcher(testTokenSource("fake-token"), "github.com", nil, nil)

	_, err := fetcher.FetchRepoPRs(t.Context(), "acme", "widgets", false)

	require.ErrorIs(t, err, ErrPublicGitHubAPIBlocked)
}

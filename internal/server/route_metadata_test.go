package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var allowedAPITags = map[string]struct{}{
	"Activity":      {},
	"Archive":       {},
	"Docs":          {},
	"Fleet":         {},
	"Issues":        {},
	"Kata":          {},
	"Projects":      {},
	"Pull Requests": {},
	"Repositories":  {},
	"Roborev":       {},
	"Runtime":       {},
	"Settings":      {},
	"Stacks":        {},
	"Sync":          {},
	"System":        {},
	"Workspaces":    {},
	"Workflows":     {},
}

// collectMetadataFailures walks an OpenAPI document and returns one entry per
// missing metadata field on every non-nil operation. The returned slice is
// sorted so failure output is stable across test runs.
//
// The walker checks each operation for a non-empty Summary, a non-empty
// OperationID, exactly one non-empty Tag from the API tag taxonomy, and a
// globally-unique OperationID.
// It deliberately does not consult huma's internal _convenience_summary and
// _convenience_id markers: those markers fire when an explicit value happens
// to match what huma would auto-generate ("List issues" for GET /issues), so
// they are not a reliable signal of "this was never set on purpose".
func collectMetadataFailures(openAPI *huma.OpenAPI) []string {
	var failures []string
	seen := map[string]string{}

	paths := make([]string, 0, len(openAPI.Paths))
	for p := range openAPI.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		item := openAPI.Paths[path]
		if item == nil {
			continue
		}
		for _, opRef := range []struct {
			method string
			op     *huma.Operation
		}{
			{http.MethodGet, item.Get},
			{http.MethodPut, item.Put},
			{http.MethodPost, item.Post},
			{http.MethodDelete, item.Delete},
			{http.MethodOptions, item.Options},
			{http.MethodHead, item.Head},
			{http.MethodPatch, item.Patch},
			{http.MethodTrace, item.Trace},
		} {
			op := opRef.op
			if op == nil {
				continue
			}
			label := fmt.Sprintf("%s %s", opRef.method, path)

			if strings.TrimSpace(op.Summary) == "" {
				failures = append(failures, label+": missing Summary")
			}
			if strings.TrimSpace(op.OperationID) == "" {
				failures = append(failures, label+": missing OperationID")
			}
			if len(op.Tags) < 1 {
				failures = append(failures, label+": missing Tags")
			} else {
				for _, tag := range op.Tags {
					if strings.TrimSpace(tag) == "" {
						failures = append(failures, label+": empty Tag")
					}
				}
			}
			if len(op.Tags) > 0 && !usesKnownSingleTag(op.Tags) {
				failures = append(failures,
					label+": expected exactly one tag from the API tag taxonomy")
			}
			if op.OperationID != "" {
				if prior, ok := seen[op.OperationID]; ok {
					failures = append(failures,
						label+": duplicate OperationID with "+prior)
				} else {
					seen[op.OperationID] = label
				}
			}
		}
	}
	return failures
}

func usesKnownSingleTag(tags []string) bool {
	if len(tags) != 1 {
		return false
	}
	_, ok := allowedAPITags[strings.TrimSpace(tags[0])]
	return ok
}

// TestHumaContractMetadata checks every live OpenAPI operation for non-empty,
// unique metadata from the API taxonomy.
func TestHumaContractMetadata(t *testing.T) {
	require := require.New(t)
	openAPI := NewOpenAPI()
	require.NotNil(openAPI)
	require.NotEmpty(openAPI.Paths, "OpenAPI document should expose paths")

	failures := collectMetadataFailures(openAPI)
	assert.Empty(t, failures, strings.Join(failures, "\n"))
}

// TestRouteMetadataWalkerCatchesUnannotatedRoute proves the live metadata
// guard does not regress into a no-op.
func TestRouteMetadataWalkerCatchesUnannotatedRoute(t *testing.T) {
	require := require.New(t)

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "0.0.0"))

	type emptyInput struct{}
	type emptyOutput struct{}
	huma.Get(api, "/unannotated", func(
		_ context.Context, _ *emptyInput,
	) (*emptyOutput, error) {
		return &emptyOutput{}, nil
	})

	failures := collectMetadataFailures(api.OpenAPI())
	require.NotEmpty(failures,
		"walker must flag unannotated routes; got no failures")
}

func TestRouteMetadataWalkerRejectsUnknownOrMultipleTags(t *testing.T) {
	require := require.New(t)

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "0.0.0"))

	type emptyInput struct{}
	type emptyOutput struct{}
	huma.Get(api, "/bad-tag", func(
		_ context.Context, _ *emptyInput,
	) (*emptyOutput, error) {
		return &emptyOutput{}, nil
	}, func(op *huma.Operation) {
		op.OperationID = "bad-tag"
		op.Summary = "Get bad tag"
		op.Tags = []string{"Pull Requests", "Not A Tag"}
	})

	failures := collectMetadataFailures(api.OpenAPI())
	require.Contains(failures,
		"GET /bad-tag: expected exactly one tag from the API tag taxonomy")
}

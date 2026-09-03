package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/federationauth"
)

func TestProviderRouteCoverage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	runParallelServerTest(t)

	registered, err := RegisteredTransportOperations()
	require.NoError(err)
	rules, err := providerRouteRules()
	require.NoError(err)

	seen := make(map[string]struct{}, len(registered))
	for _, operation := range registered {
		rule, ok := rules[operation.ID]
		require.Truef(ok, "operation %s has no ownership", operation.ID)
		_, duplicate := seen[operation.ID]
		require.Falsef(duplicate, "operation %s is registered twice", operation.ID)
		seen[operation.ID] = struct{}{}

		if rule.Owner != NodeLocal {
			assert.Contains([]federationauth.Scope{
				federationauth.ScopeProviderRead,
				federationauth.ScopeProviderWrite,
				federationauth.ScopeProviderHandoff,
			}, rule.PeerScope, operation.ID)
		}
		if operation.PeerCallable {
			require.NotEmpty(rule.PeerScope, operation.ID)
			assert.Equal(operation.PeerScope, rule.PeerScope, operation.ID)
		}
	}
	assert.Len(rules, len(registered))
}

func TestProviderRouteCoverageRejectsUnknownAndDuplicateOperations(t *testing.T) {
	runParallelServerTest(t)

	registered := []RegisteredTransportOperation{{ID: "known"}}
	_, err := buildProviderRouteRules(registered, []ProviderRouteRule{{
		OperationID: "known", Owner: NodeLocal,
	}, {
		OperationID: "known", Owner: NodeLocal,
	}})
	require.ErrorContains(t, err, "duplicate")

	_, err = buildProviderRouteRules(registered, nil)
	require.ErrorContains(t, err, "has no ownership")

	_, err = buildProviderRouteRules(registered, []ProviderRouteRule{{
		OperationID: "unknown", Owner: NodeLocal,
	}})
	require.ErrorContains(t, err, "unknown operation")
}

func TestProviderRouteOwnershipExamples(t *testing.T) {
	assert := assert.New(t)
	runParallelServerTest(t)

	rules, err := providerRouteRules()
	require.NoError(t, err)
	assert.Equal(ProviderWithLocalOverlay, rules["list-pulls"].Owner)
	assert.Equal(NodeLocal, rules["get-settings"].Owner)
	assert.Equal(ProviderHubOnly, rules["federation-get-provider-settings"].Owner)
	assert.Equal(ProviderHubOnly, rules["merge-pull"].Owner)
	assert.Equal(NodeLocal, rules["get-pull-diff"].Owner)
	assert.Equal(NodeLocal, rules["get-workspace"].Owner)
	assert.Equal(ProviderHubOnly, rules["list-workflows"].Owner)
	assert.Equal(federationauth.ScopeProviderRead, rules["list-workflows"].PeerScope)
	assert.Equal(ProviderHubOnly, rules["dispatch-workflow"].Owner)
	assert.Equal(federationauth.ScopeProviderWrite, rules["dispatch-workflow"].PeerScope)
}

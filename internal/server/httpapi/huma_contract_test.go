package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type collectionContractBody struct {
	Items []string `json:"items"`
}

type collectionContractOutput struct {
	Body collectionContractBody
}

func TestHumaCollectionsUseNonNullJSONArrays(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "0"))
	huma.Get(api, "/collections", func(context.Context, *struct{}) (*collectionContractOutput, error) {
		return &collectionContractOutput{Body: collectionContractBody{}}, nil
	})

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/collections", nil))

	assert.Equal(http.StatusOK, response.Code)
	assert.Contains(response.Body.String(), `"items":[]`)

	bodySchema := api.OpenAPI().Components.Schemas.SchemaFromRef(
		api.OpenAPI().Paths["/collections"].Get.Responses["200"].Content["application/json"].Schema.Ref,
	)
	require.NotNil(bodySchema)
	require.Contains(bodySchema.Properties, "items")
	assert.False(bodySchema.Properties["items"].Nullable)
}

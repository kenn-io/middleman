package server

import (
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
)

func TestArchiveReportResponseTransformSchemaNamesReportSchema(t *testing.T) {
	runParallelServerTest(t)

	property := &huma.Schema{}
	schema := &huma.Schema{
		Properties: map[string]*huma.Schema{"schema": property},
	}

	response := archiveReportResponse{}
	assert.Same(t, schema, response.TransformSchema(nil, schema))
	assert.Equal(t, "ReportSchema", property.Extensions["x-go-name"])
}

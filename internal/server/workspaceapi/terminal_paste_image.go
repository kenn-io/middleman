package workspaceapi

import (
	"context"
	"errors"
	"mime"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/terminalpaste"
)

type terminalPasteImageInput struct {
	ContentType string `header:"Content-Type"`
	RawBody     []byte `contentType:"application/octet-stream"`
}

type terminalPasteImageOutput struct {
	Body struct {
		Path string `json:"path" doc:"Absolute path to the cached image on the terminal host"`
	}
}

func (h *Handler) registerTerminalPasteImage(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID:   "store-terminal-paste-image",
		Method:        http.MethodPost,
		Path:          "/terminal/paste-image",
		DefaultStatus: http.StatusCreated,
		Summary:       "Store a browser clipboard image for terminal paste",
		Tags:          []string{"System"},
		MaxBodyBytes:  terminalpaste.MaxImageBytes,
	}, h.storeTerminalPasteImage)
}

func (h *Handler) storeTerminalPasteImage(
	_ context.Context,
	input *terminalPasteImageInput,
) (*terminalPasteImageOutput, error) {
	mediaType, _, err := mime.ParseMediaType(input.ContentType)
	if err != nil || mediaType != "application/octet-stream" {
		return nil, httpapi.NewProblem(http.StatusUnsupportedMediaType, httpapi.CodeBadRequest,
			"Content-Type must be application/octet-stream", nil)
	}
	if len(input.RawBody) > terminalpaste.MaxImageBytes {
		return nil, httpapi.PayloadTooLarge(
			"terminal paste image exceeds the size limit",
			terminalpaste.MaxImageBytes,
		)
	}
	if h.pasteImages == nil {
		return nil, httpapi.ServiceUnavailable(
			"terminal paste image storage is unavailable",
		)
	}
	path, err := h.pasteImages.Save(input.RawBody)
	if errors.Is(err, terminalpaste.ErrUnsupportedImage) {
		return nil, httpapi.BadRequest(
			httpapi.CodeBadRequest,
			"terminal paste image must be PNG, JPEG, or WebP",
			nil,
		)
	}
	if err != nil {
		return nil, httpapi.Internal("could not store terminal paste image")
	}
	output := &terminalPasteImageOutput{}
	output.Body.Path = path
	return output, nil
}

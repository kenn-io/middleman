package server

import (
	"context"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/platform"
)

type markdownImageOutput struct {
	ContentType        string `header:"Content-Type"`
	CacheControl       string `header:"Cache-Control"`
	ContentLength      string `header:"Content-Length"`
	ContentTypeOptions string `header:"X-Content-Type-Options"`
	Body               []byte
}

type markdownImageInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Source       string `query:"source"`
}

type markdownImageHostInput struct {
	Provider     string `path:"provider"`
	PlatformHost string `path:"platform_host"`
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Source       string `query:"source"`
}

func (s *Server) getMarkdownImage(ctx context.Context, input *markdownImageInput) (*markdownImageOutput, error) {
	return s.getMarkdownImageFor(ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, input.Source)
}

func (s *Server) getMarkdownImageOnHost(ctx context.Context, input *markdownImageHostInput) (*markdownImageOutput, error) {
	return s.getMarkdownImageFor(ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, input.Source)
}

func (s *Server) getMarkdownImageFor(
	ctx context.Context,
	provider, platformHost, owner, name, source string,
) (*markdownImageOutput, error) {
	repo, err := s.repoResolver.RequireRouteCapability(
		ctx, provider, platformHost, owner, name, capabilityReadMarkdownImages,
	)
	if err != nil {
		return nil, err
	}
	kind := httpapi.ProviderKind(*repo)
	host := httpapi.ProviderHost(*repo)
	reader, err := s.syncer.Registry().MarkdownImageReader(kind, host)
	if err != nil {
		return nil, markdownImageError(ctx, err, kind, host)
	}
	ref := httpapi.PlatformRepoRef(*repo)
	image, err := s.markdownImages.load(ctx, markdownImageCacheKey(ref, source), func(fetchCtx context.Context) (platform.MarkdownImage, error) {
		return reader.GetMarkdownImage(fetchCtx, ref, source)
	})
	if err != nil {
		return nil, markdownImageError(ctx, err, kind, host)
	}
	return &markdownImageOutput{
		ContentType:        image.ContentType,
		CacheControl:       markdownImageCacheControl(image),
		ContentLength:      strconv.Itoa(len(image.Content)),
		ContentTypeOptions: "nosniff",
		Body:               image.Content,
	}, nil
}

// markdownImageCacheKey uses the stable provider identity, not the owner/name
// route: a replacement repository at a reused route must never be served the
// previous occupant's private bytes.
func markdownImageCacheKey(ref platform.RepoRef, source string) string {
	return string(ref.Platform) + "\x00" + ref.Host + "\x00" + ref.PlatformExternalID + "\x00" + source
}

func markdownImageCacheControl(image platform.MarkdownImage) string {
	if image.Mutable {
		return "private, max-age=" + strconv.Itoa(int(markdownImageMutableTTL.Seconds()))
	}
	return "private, max-age=31536000, immutable"
}

func markdownImageError(ctx context.Context, err error, kind platform.Kind, host string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return httpapi.ProviderCallProblem(err, string(kind), host)
}

func markdownImageResponses() map[string]*huma.Response {
	return map[string]*huma.Response{
		"200": {
			Description: "Image response",
			Content: map[string]*huma.MediaType{
				"image/avif": {Schema: &huma.Schema{Type: "string", Format: "binary"}},
				"image/bmp":  {Schema: &huma.Schema{Type: "string", Format: "binary"}},
				"image/gif":  {Schema: &huma.Schema{Type: "string", Format: "binary"}},
				"image/jpeg": {Schema: &huma.Schema{Type: "string", Format: "binary"}},
				"image/png":  {Schema: &huma.Schema{Type: "string", Format: "binary"}},
				"image/webp": {Schema: &huma.Schema{Type: "string", Format: "binary"}},
			},
		},
	}
}

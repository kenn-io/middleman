package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/tokenauth"
)

// markdownImageClient carries the full repository identity even though an
// attachment URL is host-scoped: credential selection is per repository, so a
// repo-scoped route must be able to pick its own token for the fetch.
type markdownImageClient interface {
	GetMarkdownImage(
		ctx context.Context, owner, repo, sourceURL string,
	) (platform.MarkdownImage, error)
}

const maxMarkdownImageBytes = 25 << 20

var allowedMarkdownImageTypes = map[string]struct{}{
	"image/avif": {},
	"image/bmp":  {},
	"image/gif":  {},
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}

var errMarkdownImageTooLarge = fmt.Errorf("GitHub markdown image exceeds %d bytes", maxMarkdownImageBytes)

// GetMarkdownImage fetches a provider-hosted image with the repository's
// credential. Two source shapes are supported: private user-attachments
// uploads on the platform host, and files committed to the repository
// itself, referenced through blob or raw web URLs (or raw.githubusercontent.com
// on github.com). Anything else is rejected so the proxy cannot be pointed at
// arbitrary hosts.
func (c *liveClient) GetMarkdownImage(
	ctx context.Context,
	owner, repo, sourceURL string,
) (platform.MarkdownImage, error) {
	parsed, err := url.Parse(sourceURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return platform.MarkdownImage{}, c.invalidMarkdownImageSource(errors.New("unsupported markdown image URL"))
	}
	if segments, ok := markdownRepositoryFileSegments(parsed, c.platformHost, owner, repo); ok {
		return c.getRepositoryFileImage(ctx, owner, repo, segments)
	}
	if !strings.EqualFold(parsed.Host, c.platformHost) ||
		!strings.HasPrefix(parsed.EscapedPath(), "/user-attachments/assets/") {
		return platform.MarkdownImage{}, c.invalidMarkdownImageSource(errors.New("unsupported markdown image URL"))
	}
	return c.getAttachmentImage(ctx, owner, parsed)
}

func (c *liveClient) invalidMarkdownImageSource(err error) error {
	return &platform.Error{
		Code: platform.ErrCodeInvalidArgument, Provider: platform.KindGitHub,
		PlatformHost: c.platformHost, Field: "source", Err: err,
	}
}

// markdownRepositoryFileSegments recognizes image URLs that point at a file in
// the route's own repository and returns the ref-and-path segments that follow
// the repository name. Files in other repositories are not proxied: a public
// repository's file loads directly in the browser, and this route's credential
// is only known to be valid for the route's repository.
func markdownRepositoryFileSegments(parsed *url.URL, platformHost, owner, repo string) ([]string, bool) {
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) < 2 || !strings.EqualFold(segments[0], owner) || !strings.EqualFold(segments[1], repo) {
		return nil, false
	}
	var rest []string
	switch {
	case strings.EqualFold(parsed.Host, platformHost):
		if len(segments) < 3 || (segments[2] != "blob" && segments[2] != "raw") {
			return nil, false
		}
		rest = segments[3:]
	case platformHost == "github.com" && strings.EqualFold(parsed.Host, "raw.githubusercontent.com"):
		rest = segments[2:]
	default:
		return nil, false
	}
	if len(rest) < 2 {
		return nil, false
	}
	for _, segment := range rest {
		if segment == "" || segment == "." || segment == ".." {
			return nil, false
		}
	}
	return rest, true
}

// getRepositoryFileImage reads a committed file through the contents API. Web
// URLs do not delimit the ref from the file path, and branch names may contain
// slashes, so the split is resolved the way GitHub does: the shortest ref that
// yields the file wins, and 404s move on to the next candidate. The ref is
// usually a branch, so the result is marked mutable.
func (c *liveClient) getRepositoryFileImage(
	ctx context.Context,
	owner, repo string,
	segments []string,
) (platform.MarkdownImage, error) {
	for split := 1; split < len(segments); split++ {
		ref := strings.Join(segments[:split], "/")
		filePath := segments[split:]
		content, err := c.fetchRepositoryFile(ctx, owner, repo, ref, filePath)
		if err != nil {
			if githubStatusCode(err) == http.StatusNotFound {
				continue
			}
			return platform.MarkdownImage{}, c.repositoryFileError(err)
		}
		contentType, err := c.repositoryImageContentType(content, filePath[len(filePath)-1])
		if err != nil {
			return platform.MarkdownImage{}, err
		}
		return platform.MarkdownImage{Content: content, ContentType: contentType, Mutable: true}, nil
	}
	return platform.MarkdownImage{}, &platform.Error{
		Code: platform.ErrCodeNotFound, Provider: platform.KindGitHub, PlatformHost: c.platformHost,
	}
}

func (c *liveClient) fetchRepositoryFile(
	ctx context.Context,
	owner, repo, ref string,
	filePath []string,
) ([]byte, error) {
	escaped := make([]string, len(filePath))
	for i, segment := range filePath {
		escaped[i] = url.PathEscape(segment)
	}
	u := fmt.Sprintf(
		"repos/%s/%s/contents/%s?ref=%s",
		url.PathEscape(owner), url.PathEscape(repo), strings.Join(escaped, "/"), url.QueryEscape(ref),
	)
	req, err := c.gh.NewRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.raw+json")
	buf := &boundedBuffer{limit: maxMarkdownImageBytes}
	resp, err := c.gh.Do(req, buf)
	c.trackRate(resp)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c *liveClient) repositoryFileError(err error) error {
	if errors.Is(err, errMarkdownImageTooLarge) {
		return errMarkdownImageTooLarge
	}
	switch githubStatusCode(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return platform.PermissionDenied(platform.KindGitHub, c.platformHost, err)
	}
	return fmt.Errorf("fetch GitHub repository image: %w", err)
}

// repositoryImageContentType derives the image type from the bytes because the
// contents API labels every raw response with its own media type rather than
// the file's. The extension only decides formats sniffing cannot recognize.
func (c *liveClient) repositoryImageContentType(content []byte, fileName string) (string, error) {
	contentType, _, _ := mime.ParseMediaType(http.DetectContentType(content))
	if _, ok := allowedMarkdownImageTypes[contentType]; ok {
		return contentType, nil
	}
	if contentType == "application/octet-stream" {
		byExtension, _, _ := mime.ParseMediaType(mime.TypeByExtension(strings.ToLower(path.Ext(fileName))))
		if _, ok := allowedMarkdownImageTypes[byExtension]; ok {
			return byExtension, nil
		}
	}
	return "", c.invalidMarkdownImageSource(fmt.Errorf("unsupported image content type %q", contentType))
}

// boundedBuffer stops a contents download once it passes the proxy's size
// limit instead of buffering a file the route would reject anyway.
// The buffer is a field rather than embedded so io.Copy cannot bypass Write
// through bytes.Buffer's ReadFrom.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		return 0, errMarkdownImageTooLarge
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

func (c *liveClient) getAttachmentImage(
	ctx context.Context,
	owner string,
	parsed *url.URL,
) (platform.MarkdownImage, error) {
	// github.com/user-attachments accepts the user's credential but returns
	// 404 for installation tokens, even when the app can read the repository.
	authCtx := tokenauth.WithMutationAuth(tokenauth.WithGitHubOwner(ctx, owner))
	if c.source == nil {
		return platform.MarkdownImage{}, errors.New("GitHub markdown image token source is unavailable")
	}
	token, err := c.source.Token(authCtx)
	if err != nil {
		return platform.MarkdownImage{}, err
	}
	req, err := http.NewRequestWithContext(authCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return platform.MarkdownImage{}, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	client := c.markdownImageHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return platform.MarkdownImage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return platform.MarkdownImage{}, platform.PermissionDenied(platform.KindGitHub, c.platformHost, errors.New(resp.Status))
	}
	if resp.StatusCode == http.StatusNotFound {
		return platform.MarkdownImage{}, &platform.Error{Code: platform.ErrCodeNotFound, Provider: platform.KindGitHub, PlatformHost: c.platformHost}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return platform.MarkdownImage{}, fmt.Errorf("fetch GitHub markdown image: %s", resp.Status)
	}

	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return platform.MarkdownImage{}, fmt.Errorf("parse GitHub markdown image content type: %w", err)
	}
	if _, ok := allowedMarkdownImageTypes[contentType]; !ok {
		return platform.MarkdownImage{}, c.invalidMarkdownImageSource(fmt.Errorf("unsupported image content type %q", contentType))
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxMarkdownImageBytes+1))
	if err != nil {
		return platform.MarkdownImage{}, err
	}
	if len(content) > maxMarkdownImageBytes {
		return platform.MarkdownImage{}, errMarkdownImageTooLarge
	}
	return platform.MarkdownImage{Content: content, ContentType: contentType}, nil
}

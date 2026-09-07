package gitlab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.kenn.io/forge/platform"
)

const maxMarkdownImageBytes = 25 << 20

var allowedMarkdownImageTypes = map[string]struct{}{
	"image/avif": {},
	"image/bmp":  {},
	"image/gif":  {},
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}

func (c *Client) GetMarkdownImage(
	ctx context.Context,
	ref platform.RepoRef,
	sourceURL string,
) (platform.MarkdownImage, error) {
	ctx, cancel := c.withForegroundTimeout(ctx)
	defer cancel()
	_, ref, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.MarkdownImage{}, err
	}
	secret, filename, err := c.markdownUploadParts(ref, sourceURL)
	if err != nil {
		return platform.MarkdownImage{}, err
	}
	if ref.PlatformID <= 0 {
		return platform.MarkdownImage{}, &platform.Error{
			Code: platform.ErrCodeInvalidRepoRef, Provider: platform.KindGitLab,
			PlatformHost: c.host, Field: "platform_id", Err: errors.New("missing GitLab project ID"),
		}
	}

	endpoint := strings.TrimRight(c.baseURL, "/") + "/projects/" +
		strconv.FormatInt(ref.PlatformID, 10) + "/uploads/" +
		url.PathEscape(secret) + "/" + url.PathEscape(filename)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return platform.MarkdownImage{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return platform.MarkdownImage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var code platform.PlatformErrorCode
		switch resp.StatusCode {
		case http.StatusBadRequest:
			code = platform.ErrCodeInvalidRepoRef
		case http.StatusUnauthorized, http.StatusForbidden:
			code = platform.ErrCodePermissionDenied
		case http.StatusNotFound:
			code = platform.ErrCodeNotFound
		case http.StatusTooManyRequests:
			code = platform.ErrCodeRateLimited
		default:
			return platform.MarkdownImage{}, fmt.Errorf("GitLab markdown image request failed: %s", resp.Status)
		}
		return platform.MarkdownImage{}, &platform.Error{
			Code: code, Provider: platform.KindGitLab, PlatformHost: c.host,
			Capability: "read_markdown_images", Err: errors.New(resp.Status),
		}
	}
	if resp.ContentLength > maxMarkdownImageBytes {
		return platform.MarkdownImage{}, fmt.Errorf("GitLab markdown image exceeds %d bytes", maxMarkdownImageBytes)
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxMarkdownImageBytes+1))
	if err != nil {
		return platform.MarkdownImage{}, err
	}
	if len(content) > maxMarkdownImageBytes {
		return platform.MarkdownImage{}, fmt.Errorf("GitLab markdown image exceeds %d bytes", maxMarkdownImageBytes)
	}
	contentType := markdownImageContentType(resp.Header.Get("Content-Type"), content)
	if _, ok := allowedMarkdownImageTypes[contentType]; !ok {
		return platform.MarkdownImage{}, &platform.Error{
			Code: platform.ErrCodeInvalidArgument, Provider: platform.KindGitLab,
			PlatformHost: c.host, Field: "source", Err: fmt.Errorf("unsupported markdown image content type %q", contentType),
		}
	}
	return platform.MarkdownImage{Content: content, ContentType: contentType}, nil
}

func (c *Client) markdownUploadParts(ref platform.RepoRef, source string) (string, string, error) {
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", c.invalidMarkdownImageSource()
	}
	apiURL, err := url.Parse(c.baseURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, apiURL.Scheme) ||
		!strings.EqualFold(parsed.Host, apiURL.Host) {
		return "", "", c.invalidMarkdownImageSource()
	}
	prefixes := []string{"/" + escapePath(ref.RepoPath) + "/uploads/"}
	if ref.PlatformID > 0 {
		prefixes = append(prefixes, "/-/project/"+strconv.FormatInt(ref.PlatformID, 10)+"/uploads/")
	}
	prefix := ""
	for _, candidate := range prefixes {
		if strings.HasPrefix(parsed.EscapedPath(), candidate) {
			prefix = candidate
			break
		}
	}
	if ref.RepoPath == "" || prefix == "" {
		return "", "", c.invalidMarkdownImageSource()
	}
	parts := strings.Split(strings.TrimPrefix(parsed.EscapedPath(), prefix), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", c.invalidMarkdownImageSource()
	}
	secret, err := url.PathUnescape(parts[0])
	if err != nil || strings.ContainsAny(secret, "/\\") {
		return "", "", c.invalidMarkdownImageSource()
	}
	filename, err := url.PathUnescape(parts[1])
	if err != nil || strings.ContainsAny(filename, "/\\") {
		return "", "", c.invalidMarkdownImageSource()
	}
	return secret, filename, nil
}

func (c *Client) invalidMarkdownImageSource() error {
	return &platform.Error{
		Code: platform.ErrCodeInvalidArgument, Provider: platform.KindGitLab,
		PlatformHost: c.host, Field: "source", Err: errors.New("unsupported markdown image URL"),
	}
}

func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func markdownImageContentType(header string, content []byte) string {
	contentType, _, err := mime.ParseMediaType(header)
	if err == nil && contentType != "" && contentType != "application/octet-stream" {
		return contentType
	}
	return http.DetectContentType(content)
}

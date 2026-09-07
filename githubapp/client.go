package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/forge/platform"
)

// APIBaseForHost resolves the REST API base URL for a GitHub host:
// the public host uses api.github.com, GitHub Enterprise hosts serve
// the API under /api/v3.
func APIBaseForHost(host string) string {
	if host == "" || host == platform.DefaultGitHubHost {
		return "https://api.github.com"
	}
	return "https://" + host + "/api/v3"
}

// WebBaseForHost resolves the browser-facing base URL for a host.
func WebBaseForHost(host string) string {
	if host == "" {
		host = platform.DefaultGitHubHost
	}
	return "https://" + host
}

// Client is a minimal GitHub App management client. It speaks only
// the app-scoped endpoints the kenn-forge-github-app CLI and the
// installation token minter need; repository data access stays on the
// main provider clients.
type Client struct {
	apiBase    string
	httpClient *http.Client
}

func NewClient(host string) *Client {
	return NewClientWithBase(APIBaseForHost(host))
}

// NewClientWithBase constructs a client against an explicit API base
// URL. Tests point this at a local fake server.
func NewClientWithBase(apiBase string) *Client {
	return &Client{
		apiBase:    strings.TrimRight(apiBase, "/"),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// AppCredentials is the manifest conversion response: everything the
// new app needs to authenticate, returned exactly once by GitHub.
type AppCredentials struct {
	ID            int64   `json:"id"`
	Slug          string  `json:"slug"`
	Name          string  `json:"name"`
	ClientID      string  `json:"client_id"`
	ClientSecret  string  `json:"client_secret"`
	WebhookSecret *string `json:"webhook_secret"`
	PEM           string  `json:"pem"`
	HTMLURL       string  `json:"html_url"`
	Owner         Account `json:"owner"`
}

type Account struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// App is the GET /app response subset kenn-forge cares about.
type App struct {
	ID      int64   `json:"id"`
	Slug    string  `json:"slug"`
	Name    string  `json:"name"`
	HTMLURL string  `json:"html_url"`
	Owner   Account `json:"owner"`
}

// Installation is one account the app is installed on.
type Installation struct {
	ID                  int64   `json:"id"`
	Account             Account `json:"account"`
	RepositorySelection string  `json:"repository_selection"`
}

// InstallationToken is a minted installation access token.
type InstallationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RateLimit is the core REST rate budget for a credential.
type RateLimit struct {
	Limit     int   `json:"limit"`
	Remaining int   `json:"remaining"`
	Reset     int64 `json:"reset"`
}

// ConvertManifest exchanges a manifest flow code for the new app's
// credentials. The exchange needs no authentication and each code
// works exactly once, expiring after one hour.
func (c *Client) ConvertManifest(ctx context.Context, code string) (*AppCredentials, error) {
	if code == "" {
		return nil, fmt.Errorf("manifest conversion code is required")
	}
	var creds AppCredentials
	err := c.do(ctx, http.MethodPost,
		"/app-manifests/"+code+"/conversions", "", nil, &creds)
	if err != nil {
		return nil, fmt.Errorf("converting app manifest code: %w", err)
	}
	return &creds, nil
}

// GetApp returns the authenticated app for an app JWT.
func (c *Client) GetApp(ctx context.Context, appJWT string) (*App, error) {
	var app App
	if err := c.do(ctx, http.MethodGet, "/app", appJWT, nil, &app); err != nil {
		return nil, fmt.Errorf("getting app: %w", err)
	}
	return &app, nil
}

// ListInstallations lists the accounts the app is installed on.
func (c *Client) ListInstallations(ctx context.Context, appJWT string) ([]Installation, error) {
	var installs []Installation
	err := c.do(ctx, http.MethodGet,
		"/app/installations?per_page=100", appJWT, nil, &installs)
	if err != nil {
		return nil, fmt.Errorf("listing app installations: %w", err)
	}
	return installs, nil
}

// CreateInstallationToken mints an installation access token. Tokens
// expire after one hour.
func (c *Client) CreateInstallationToken(
	ctx context.Context, appJWT string, installationID int64,
) (*InstallationToken, error) {
	var token InstallationToken
	err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/app/installations/%d/access_tokens", installationID),
		appJWT, nil, &token)
	if err != nil {
		return nil, fmt.Errorf(
			"creating installation token for installation %d: %w", installationID, err,
		)
	}
	return &token, nil
}

// ListInstallationRepositories lists the full names ("owner/name") of
// every repository an installation token can reach. Authenticated
// with the installation token itself, not an app JWT. Used to verify
// that a "selected repositories" installation actually covers the
// repos kenn-forge is configured to sync.
func (c *Client) ListInstallationRepositories(
	ctx context.Context, installationToken string,
) ([]string, error) {
	var names []string
	for page := 1; ; page++ {
		var out struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page)
		if err := c.do(ctx, http.MethodGet, path, installationToken, nil, &out); err != nil {
			return nil, fmt.Errorf("listing installation repositories: %w", err)
		}
		for _, repo := range out.Repositories {
			names = append(names, repo.FullName)
		}
		if len(out.Repositories) == 0 || len(names) >= out.TotalCount {
			return names, nil
		}
	}
}

// DeleteInstallation uninstalls the app from an account.
func (c *Client) DeleteInstallation(
	ctx context.Context, appJWT string, installationID int64,
) error {
	err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/app/installations/%d", installationID), appJWT, nil, nil)
	if err != nil {
		return fmt.Errorf("deleting installation %d: %w", installationID, err)
	}
	return nil
}

// CoreRateLimit reports the core REST budget for a token.
func (c *Client) CoreRateLimit(ctx context.Context, token string) (*RateLimit, error) {
	var out struct {
		Resources struct {
			Core RateLimit `json:"core"`
		} `json:"resources"`
	}
	if err := c.do(ctx, http.MethodGet, "/rate_limit", token, nil, &out); err != nil {
		return nil, fmt.Errorf("reading rate limit: %w", err)
	}
	return &out.Resources.Core, nil
}

// StatusError is a non-2xx API response.
type StatusError struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func (e *StatusError) Error() string {
	msg := strings.TrimSpace(e.Body)
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return fmt.Sprintf("github api status %d: %s", e.StatusCode, msg)
}

// RetryDeadline returns the server-provided time when a rate-limited request
// may be retried. Retry-After takes precedence over the primary rate-limit
// reset header when both are present.
func (e *StatusError) RetryDeadline(now time.Time) time.Time {
	if e == nil {
		return time.Time{}
	}
	if value := strings.TrimSpace(e.Header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
			return now.Add(time.Duration(seconds) * time.Second)
		}
		if at, err := http.ParseTime(value); err == nil {
			return at
		}
	}
	rateExhausted := strings.TrimSpace(e.Header.Get("X-RateLimit-Remaining")) == "0"
	reset := strings.TrimSpace(e.Header.Get("X-RateLimit-Reset"))
	if (e.StatusCode == http.StatusTooManyRequests || rateExhausted) &&
		reset != "" {
		if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
			return time.Unix(epoch, 0).UTC()
		}
	}
	return time.Time{}
}

// IsStatus reports whether err is a StatusError with the given code.
func IsStatus(err error, code int) bool {
	var se *StatusError
	if !errors.As(err, &se) || se == nil {
		return false
	}
	return se.StatusCode == code
}

func (c *Client) do(
	ctx context.Context, method, path, bearer string, body, out any,
) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{
			StatusCode: resp.StatusCode,
			Header:     resp.Header.Clone(),
			Body:       string(data),
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

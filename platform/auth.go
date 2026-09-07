package platform

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type RoundTripFunc func(*http.Request) (*http.Response, error)

func (f RoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type HeaderSetter func(*http.Request, string)

// CredentialSource is explicit caller-owned credential resolution. The
// transport never discovers credentials or chooses a fallback itself.
type CredentialSource interface {
	Token(context.Context) (string, error)
	Invalidate(rejectedToken string)
}

func BearerAuthHeader(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
}

func TokenAuthHeader(req *http.Request, token string) {
	req.Header.Set("Authorization", "token "+token)
}

func PrivateTokenHeader(req *http.Request, token string) {
	req.Header.Set("Private-Token", token)
}

type AuthTransport struct {
	Source              CredentialSource
	Base                http.RoundTripper
	SetHeader           HeaderSetter
	RetryOnUnauthorized bool
	AllowedOrigin       string
	TokenContext        func(*http.Request) context.Context
}

func (t AuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	first, firstToken, err := t.authorizedRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := base.RoundTrip(first)
	if err != nil ||
		!t.RetryOnUnauthorized ||
		resp == nil ||
		resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	retry, ok := cloneForRetry(req)
	if !ok {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	t.Source.Invalidate(firstToken)
	second, _, err := t.authorizedRequest(retry)
	if err != nil {
		return nil, err
	}
	return base.RoundTrip(second)
}

func (t AuthTransport) authorizedRequest(req *http.Request) (*http.Request, string, error) {
	if err := t.validateOrigin(req); err != nil {
		return nil, "", err
	}
	if t.Source == nil {
		return nil, "", fmt.Errorf("%w: nil token source", ErrMissingToken)
	}
	ctx := req.Context()
	if t.TokenContext != nil {
		ctx = t.TokenContext(req)
	}
	token, err := t.Source.Token(ctx)
	if err != nil {
		return nil, "", err
	}
	clone := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		clone.Body = req.Body
	}
	t.SetHeader(clone, token)
	return clone, token, nil
}

func (t AuthTransport) validateOrigin(req *http.Request) error {
	if strings.TrimSpace(t.AllowedOrigin) == "" {
		return nil
	}
	want, err := normalizedOrigin(t.AllowedOrigin)
	if err != nil {
		return fmt.Errorf("auth allowed origin: %w", err)
	}
	got := originFromURL(req.URL)
	if got == want {
		return nil
	}
	return fmt.Errorf("refusing to attach auth to %s; expected %s", got, want)
}

func normalizedOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("missing scheme or host")
	}
	return originFromURL(u), nil
}

func originFromURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

func cloneForRetry(req *http.Request) (*http.Request, bool) {
	clone := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return clone, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	clone.Body = body
	return clone, true
}

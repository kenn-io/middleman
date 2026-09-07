package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	gh "github.com/google/go-github/v89/github"
)

// NativeStackHint is the compact stack membership GitHub includes on a pull
// request. A nil hint for an observed PR means GitHub returned no membership.
type NativeStackHint struct {
	ID       int64
	Number   int
	Size     int
	Position int
	BaseRef  string
}

// NativeStack is the complete read-only stack resource returned by GitHub.
type NativeStack struct {
	ID        int64
	Number    int
	BaseRef   string
	Open      bool
	CreatedAt time.Time
	Members   []NativeStackMember
}

type NativeStackMember struct {
	Position          int
	PullRequestNumber int
	State             string
	Draft             bool
	MergedAt          *time.Time
	HeadRef           string
	HeadSHA           string
}

type NativeStackPage struct {
	Stacks   []NativeStack
	NextPage int
}

// NativeStackClient is the optional private-preview surface implemented by the
// live GitHub client without widening the provider-neutral client contracts.
type NativeStackClient interface {
	ListOpenPullRequestsWithNativeStackHints(
		ctx context.Context,
		owner, repo string,
	) ([]*gh.PullRequest, map[int]*NativeStackHint, error)
	ListNativeStacksPage(
		ctx context.Context,
		owner, repo string,
		page int,
	) (NativeStackPage, error)
}

// nativePullRequestResource keeps the preview field out of the pull-request
// decode. The field rides along on the primary open-PR list, so a preview whose
// shape changes must not fail that list and stall ordinary synchronization; the
// raw value is decoded separately and a rejected hint simply leaves the pull
// request unclaimed, which forces the catalog to be refetched.
type nativePullRequestResource struct {
	gh.PullRequest
	Stack json.RawMessage `json:"stack"`
}

type nativeStackHintResource struct {
	ID       int64 `json:"id"`
	Number   int   `json:"number"`
	Size     int   `json:"size"`
	Position int   `json:"position"`
	Base     struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type nativeStackResource struct {
	ID        int64     `json:"id"`
	Number    int       `json:"number"`
	Open      bool      `json:"open"`
	CreatedAt time.Time `json:"created_at"`
	Base      struct {
		Ref string `json:"ref"`
	} `json:"base"`
	PullRequests []struct {
		Number   int        `json:"number"`
		State    string     `json:"state"`
		Draft    bool       `json:"draft"`
		MergedAt *time.Time `json:"merged_at"`
		Head     struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_requests"`
}

type nativePullRequestObservation struct {
	PR   *gh.PullRequest
	Hint *NativeStackHint
}

// decodeNativeStackHint reads one pull request's preview field. A value that no
// longer matches the expected shape yields no hint rather than an error: the
// pull request itself is still valid data, and an unclaimed pull request makes
// the catalog authoritative for that repository.
func (c *Client) decodeNativeStackHint(
	owner, repo string, resource *nativePullRequestResource,
) *NativeStackHint {
	raw := resource.Stack
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var hint nativeStackHintResource
	if err := json.Unmarshal(raw, &hint); err != nil {
		c.warn("ignore unreadable github native stack hint",
			"repo", owner+"/"+repo,
			"pull_request", resource.GetNumber(), "err", err)
		return nil
	}
	return &NativeStackHint{
		ID: hint.ID, Number: hint.Number,
		Size: hint.Size, Position: hint.Position,
		BaseRef: hint.Base.Ref,
	}
}

func (c *Client) ListOpenPullRequestsWithNativeStackHints(
	ctx context.Context,
	owner, repo string,
) ([]*gh.PullRequest, map[int]*NativeStackHint, error) {
	progress := c.progress(owner, repo, "pulls")
	observations, err := collectPagesWithProgress(ctx, func(options *gh.ListOptions) ([]nativePullRequestObservation, *gh.Response, error) {
		path := fmt.Sprintf(
			"repos/%s/%s/pulls?state=open&per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), options.PerPage, max(options.Page, 1),
		)
		req, err := c.gh.NewRequest(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, nil, err
		}
		var resources []nativePullRequestResource
		resp, err := c.gh.Do(req, &resources)
		if err != nil {
			return nil, resp, fmt.Errorf("listing open pull requests for %s/%s: %w", owner, repo, err)
		}
		page := make([]nativePullRequestObservation, 0, len(resources))
		for i := range resources {
			resource := &resources[i]
			observation := nativePullRequestObservation{
				PR:   &resource.PullRequest,
				Hint: c.decodeNativeStackHint(owner, repo, resource),
			}
			page = append(page, observation)
		}
		return page, resp, nil
	}, c.trackRate, progress.recordPage)
	if err != nil {
		return nil, nil, err
	}
	progress.done()

	prs := make([]*gh.PullRequest, 0, len(observations))
	hints := make(map[int]*NativeStackHint, len(observations))
	for _, observation := range observations {
		prs = append(prs, observation.PR)
		hints[observation.PR.GetNumber()] = observation.Hint
	}
	return prs, hints, nil
}

func (c *Client) ListNativeStacksPage(
	ctx context.Context,
	owner, repo string,
	page int,
) (NativeStackPage, error) {
	path := fmt.Sprintf(
		"repos/%s/%s/stacks?per_page=100&page=%d",
		url.PathEscape(owner), url.PathEscape(repo), max(page, 1),
	)
	req, err := c.gh.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return NativeStackPage{}, err
	}
	var resources []nativeStackResource
	resp, err := c.gh.Do(req, &resources)
	c.trackRate(resp)
	if err != nil {
		return NativeStackPage{}, fmt.Errorf("listing native stacks for %s/%s: %w", owner, repo, err)
	}
	result := NativeStackPage{Stacks: make([]NativeStack, 0, len(resources))}
	if resp != nil {
		result.NextPage = resp.NextPage
	}
	for i := range resources {
		result.Stacks = append(result.Stacks, resources[i].nativeStack())
	}
	return result, nil
}

func (resource nativeStackResource) nativeStack() NativeStack {
	stack := NativeStack{
		ID: resource.ID, Number: resource.Number, BaseRef: resource.Base.Ref,
		Open: resource.Open, CreatedAt: resource.CreatedAt,
		Members: make([]NativeStackMember, 0, len(resource.PullRequests)),
	}
	for i := range resource.PullRequests {
		member := resource.PullRequests[i]
		stack.Members = append(stack.Members, NativeStackMember{
			Position: i + 1, PullRequestNumber: member.Number,
			State: member.State, Draft: member.Draft, MergedAt: member.MergedAt,
			HeadRef: member.Head.Ref, HeadSHA: member.Head.SHA,
		})
	}
	return stack
}

func ValidateNativeStack(stack NativeStack) error {
	if stack.ID <= 0 || stack.Number <= 0 {
		return fmt.Errorf("invalid stack identity")
	}
	if len(stack.Members) == 0 {
		return fmt.Errorf("stack %d has no members", stack.Number)
	}
	seen := make(map[int]bool, len(stack.Members))
	for i, member := range stack.Members {
		if member.Position != i+1 {
			return fmt.Errorf("stack %d has invalid position %d", stack.Number, member.Position)
		}
		if member.PullRequestNumber <= 0 || seen[member.PullRequestNumber] {
			return fmt.Errorf("stack %d has invalid pull request number %d", stack.Number, member.PullRequestNumber)
		}
		seen[member.PullRequestNumber] = true
	}
	return nil
}

func NativeStackFingerprint(stack NativeStack) string {
	canonical := stack
	canonical.Members = slices.Clone(stack.Members)
	slices.SortFunc(canonical.Members, func(a, b NativeStackMember) int {
		return a.Position - b.Position
	})
	payload, _ := json.Marshal(canonical)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

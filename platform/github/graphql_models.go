package github

import (
	"strings"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/shurcooL/githubv4"
)

type GraphQLPR struct {
	DatabaseId     int64 `graphql:"databaseId"`
	Number         int
	Title          string
	State          string
	IsDraft        bool
	Locked         bool
	Body           string
	URL            string
	Author         struct{ Login string }
	CreatedAt      time.Time
	UpdatedAt      time.Time
	MergedAt       *time.Time
	MergedBy       *struct{ Login string }
	ClosedAt       *time.Time
	Additions      int
	Deletions      int
	Mergeable      string
	ReviewDecision string
	HeadRefName    string
	BaseRefName    string
	HeadRefOid     string `graphql:"headRefOid"`
	BaseRefOid     string `graphql:"baseRefOid"`
	HeadRepository *struct {
		URL string
	}
	Labels struct {
		Nodes []GraphQLLabel
	} `graphql:"labels(first: 100)"`
	Assignees struct {
		Nodes []GraphQLAssigneeID
	} `graphql:"assignees(first: 100)"`
	ReviewRequests struct {
		Nodes []GraphQLReviewRequest
	} `graphql:"reviewRequests(first: 100)"`
	Comments struct {
		Nodes    []GraphQLComment
		PageInfo GraphQLPageInfo
	} `graphql:"comments(first: 100)"`
	ReviewThreads struct {
		Nodes    []GraphQLReviewThread
		PageInfo GraphQLPageInfo
	} `graphql:"reviewThreads(first: 100)"`
	Reviews struct {
		Nodes    []GraphQLReview
		PageInfo GraphQLPageInfo
	} `graphql:"reviews(first: 100)"`
	AllCommits struct {
		Nodes    []GraphQLCommitNode
		PageInfo GraphQLPageInfo
	} `graphql:"allCommits: commits(first: 100)"`
	LastCommit struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					Contexts struct {
						Nodes    []GraphQLCheckContext
						PageInfo GraphQLPageInfo
					} `graphql:"contexts(first: 100)"`
				}
			}
		}
	} `graphql:"lastCommit: commits(last: 1)"`
	TimelineItems struct {
		Nodes    []GraphQLPullRequestTimelineItem
		PageInfo GraphQLPageInfo
	} `graphql:"timelineItems(itemTypes: [HEAD_REF_FORCE_PUSHED_EVENT, COMMENT_DELETED_EVENT, CROSS_REFERENCED_EVENT, RENAMED_TITLE_EVENT, BASE_REF_CHANGED_EVENT, ASSIGNED_EVENT, UNASSIGNED_EVENT, MERGED_EVENT, CLOSED_EVENT, REOPENED_EVENT], first: 100)"`
}

// GraphQLPRWithNativeStacks is a distinct query shape so preview-only fields are
// absent from GraphQL requests while the setting is disabled. An @include
// directive would still make servers without the preview schema validate the
// unknown fields.
type GraphQLPRWithNativeStacks struct {
	GraphQLPR
	Stack      *GraphQLNativeStack
	StackEntry *struct{ Position int }
}

type GraphQLNativeStack struct {
	ID          githubv4.ID
	Number      int
	Size        int
	BaseRefName string
}

// GraphQLReviewRequest carries the requested reviewer of a pending review
// request. Team review requests are intentionally skipped: kenn-forge only
// tracks user review requests.
type GraphQLReviewRequest struct {
	RequestedReviewer struct {
		Typename  string            `graphql:"__typename"`
		User      GraphQLAssigneeID `graphql:"... on User"`
		Mannequin GraphQLAssigneeID `graphql:"... on Mannequin"`
	} `graphql:"requestedReviewer"`
}

func (r GraphQLReviewRequest) Login() string {
	switch r.RequestedReviewer.Typename {
	case "User":
		return r.RequestedReviewer.User.Login
	case "Mannequin":
		return r.RequestedReviewer.Mannequin.Login
	default:
		return ""
	}
}

type GraphQLComment struct {
	DatabaseId      int64
	FullDatabaseId  GraphQLInt64
	Author          struct{ Login string }
	Body            string
	URL             string `graphql:"url"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
	IsMinimized     bool
	MinimizedReason *githubv4.ReportedContentClassifiers
}

type GraphQLCommentVisibilityNode struct {
	DatabaseId      int64
	FullDatabaseId  GraphQLInt64
	IsMinimized     bool
	MinimizedReason *githubv4.ReportedContentClassifiers
}

type GraphQLReviewThread struct {
	ID                githubv4.ID `graphql:"id"`
	IsResolved        bool
	IsOutdated        bool
	Path              string
	Line              int
	OriginalLine      int
	StartLine         *int
	OriginalStartLine *int
	DiffSide          string
	Comments          struct {
		Nodes    []GraphQLReviewThreadComment
		PageInfo GraphQLPageInfo
	} `graphql:"comments(first: 100)"`
}

type GraphQLReviewThreadComment struct {
	ID             githubv4.ID `graphql:"id"`
	DatabaseId     int64
	FullDatabaseId GraphQLInt64
	Body           string
	Path           string
	Line           int
	OriginalLine   int
	SubjectType    string
	DiffHunk       string
	URL            string `graphql:"url"`
	Author         struct{ Login string }
	Commit         *struct {
		OID string `graphql:"oid"`
	}
	OriginalCommit *struct {
		OID string `graphql:"oid"`
	}
	PullRequestReview *struct {
		DatabaseId int64
	}
	IsMinimized     bool
	MinimizedReason *githubv4.ReportedContentClassifiers
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type GraphQLReview struct {
	DatabaseId  int64
	Author      struct{ Login string }
	Body        string
	State       string
	SubmittedAt time.Time
}

type GraphQLCommitNode struct {
	Commit GraphQLCommit
}

type GraphQLCommit struct {
	OID     string `graphql:"oid"`
	Message string
	Author  struct {
		Name string
		Date *time.Time
		User *struct{ Login string }
	}
	Committer struct {
		Name string
		Date *time.Time
		User *struct{ Login string }
	}
}

type GraphQLPullRequestTimelineItem struct {
	Typename                string                         `graphql:"__typename"`
	Node                    GraphQLNodeFragment            `graphql:"... on Node"`
	HeadRefForcePushedEvent GraphQLHeadRefForcePushedEvent `graphql:"... on HeadRefForcePushedEvent"`
	CommentDeletedEvent     GraphQLCommentDeletedEvent     `graphql:"... on CommentDeletedEvent"`
	CrossReferencedEvent    GraphQLCrossReferencedEvent    `graphql:"... on CrossReferencedEvent"`
	RenamedTitleEvent       GraphQLRenamedTitleEvent       `graphql:"... on RenamedTitleEvent"`
	BaseRefChangedEvent     GraphQLBaseRefChangedEvent     `graphql:"... on BaseRefChangedEvent"`
	AssignedEvent           GraphQLAssignedEvent           `graphql:"... on AssignedEvent"`
	UnassignedEvent         GraphQLAssignedEvent           `graphql:"... on UnassignedEvent"`
	MergedEvent             GraphQLLifecycleEvent          `graphql:"... on MergedEvent"`
	ClosedEvent             GraphQLLifecycleEvent          `graphql:"... on ClosedEvent"`
	ReopenedEvent           GraphQLLifecycleEvent          `graphql:"... on ReopenedEvent"`
}

type GraphQLIssueTimelineItem struct {
	Typename             string                      `graphql:"__typename"`
	Node                 GraphQLNodeFragment         `graphql:"... on Node"`
	CrossReferencedEvent GraphQLCrossReferencedEvent `graphql:"... on CrossReferencedEvent"`
	AssignedEvent        GraphQLAssignedEvent        `graphql:"... on AssignedEvent"`
	UnassignedEvent      GraphQLAssignedEvent        `graphql:"... on UnassignedEvent"`
	ClosedEvent          GraphQLLifecycleEvent       `graphql:"... on ClosedEvent"`
	ReopenedEvent        GraphQLLifecycleEvent       `graphql:"... on ReopenedEvent"`
}

type GraphQLNodeFragment struct {
	ID string
}

type GraphQLActorRef struct {
	Login string
}

type GraphQLHeadRefForcePushedEvent struct {
	Actor        *GraphQLActorRef
	BeforeCommit *struct {
		OID string `graphql:"oid"`
	}
	AfterCommit *struct {
		OID string `graphql:"oid"`
	}
	CreatedAt time.Time
	Ref       *struct {
		Name string
	}
}

type GraphQLCommentDeletedEvent struct {
	Actor                *GraphQLActorRef
	CreatedAt            time.Time
	DeletedCommentAuthor *GraphQLActorRef
}

type GraphQLCrossReferencedEvent struct {
	Actor             *GraphQLActorRef
	CreatedAt         time.Time
	IsCrossRepository bool
	WillCloseTarget   bool
	Source            GraphQLReferencedSubject
}

type GraphQLReferencedSubject struct {
	Typename    string                     `graphql:"__typename"`
	Issue       GraphQLReferencedIssueOrPR `graphql:"... on Issue"`
	PullRequest GraphQLReferencedIssueOrPR `graphql:"... on PullRequest"`
}

type GraphQLReferencedIssueOrPR struct {
	Number     int
	Title      string
	URL        string `graphql:"url"`
	Repository struct {
		Owner struct {
			Login string
		}
		Name string
	}
}

type GraphQLRenamedTitleEvent struct {
	Actor         *GraphQLActorRef
	CreatedAt     time.Time
	PreviousTitle string
	CurrentTitle  string
}

type GraphQLBaseRefChangedEvent struct {
	Actor           *GraphQLActorRef
	CreatedAt       time.Time
	PreviousRefName string
	CurrentRefName  string
}

type GraphQLAssignedEvent struct {
	Actor     *GraphQLActorRef
	Assignee  GraphQLAssignee
	CreatedAt time.Time
}

type GraphQLLifecycleEvent struct {
	Actor     *GraphQLActorRef
	CreatedAt time.Time
}

type GraphQLAssignee struct {
	Typename     string            `graphql:"__typename"`
	Bot          GraphQLAssigneeID `graphql:"... on Bot"`
	Mannequin    GraphQLAssigneeID `graphql:"... on Mannequin"`
	Organization GraphQLAssigneeID `graphql:"... on Organization"`
	User         GraphQLAssigneeID `graphql:"... on User"`
}

type GraphQLAssigneeID struct {
	Login string
}

func (a GraphQLAssignee) Login() string {
	switch a.Typename {
	case "Bot":
		return a.Bot.Login
	case "Mannequin":
		return a.Mannequin.Login
	case "Organization":
		return a.Organization.Login
	case "User":
		return a.User.Login
	default:
		return ""
	}
}

type GraphQLIssue struct {
	DatabaseId int64 `graphql:"databaseId"`
	Number     int
	Title      string
	State      string
	Body       string
	URL        string `graphql:"url"`
	Author     GraphQLIssueAuthor
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ClosedAt   *time.Time
	Labels     struct {
		Nodes []GraphQLLabel
	} `graphql:"labels(first: 100)"`
	Comments struct {
		TotalCount int
		Nodes      []GraphQLComment
		PageInfo   GraphQLPageInfo
	} `graphql:"comments(first: 100)"`
	Assignees struct {
		Nodes []GraphQLAssigneeID
	} `graphql:"assignees(first: 100)"`
	TimelineItems struct {
		Nodes    []GraphQLIssueTimelineItem
		PageInfo GraphQLPageInfo
	} `graphql:"timelineItems(itemTypes: [ASSIGNED_EVENT, UNASSIGNED_EVENT, CROSS_REFERENCED_EVENT, CLOSED_EVENT, REOPENED_EVENT], first: 100)"`
}

type GraphQLIssueAuthor struct {
	Login    string
	Typename string `graphql:"__typename"`
}

type GraphQLLabel struct {
	Name        string
	Color       string
	Description string
	IsDefault   bool
}

type GraphQLCheckContext struct {
	Typename      string                     `graphql:"__typename"`
	CheckRun      GraphQLCheckRunFields      `graphql:"... on CheckRun"`
	StatusContext GraphQLStatusContextFields `graphql:"... on StatusContext"`
}

type GraphQLCheckRunFields struct {
	Name        string
	Status      string
	Conclusion  string
	DetailsURL  string `graphql:"detailsUrl"`
	StartedAt   *time.Time
	CompletedAt *time.Time
	CheckSuite  struct {
		CreatedAt *time.Time
		App       struct {
			Name string
		}
	}
}

type GraphQLStatusContextFields struct {
	Context   string
	State     string
	TargetURL string `graphql:"targetUrl"`
}

func AdaptPR(gql *GraphQLPR) *gh.PullRequest {
	state := StateToREST(gql.State)
	pr := &gh.PullRequest{
		ID:        new(gql.DatabaseId),
		Number:    new(gql.Number),
		Title:     new(gql.Title),
		State:     new(state),
		Draft:     new(gql.IsDraft),
		Locked:    new(gql.Locked),
		Body:      new(gql.Body),
		HTMLURL:   new(gql.URL),
		Additions: new(gql.Additions),
		Deletions: new(gql.Deletions),
		User:      &gh.User{Login: new(gql.Author.Login)},
		Head: &gh.PullRequestBranch{
			Ref: new(gql.HeadRefName),
			SHA: new(gql.HeadRefOid),
		},
		Base: &gh.PullRequestBranch{
			Ref: new(gql.BaseRefName),
			SHA: new(gql.BaseRefOid),
		},
		MergeableState: new(MergeableToREST(gql.Mergeable)),
	}

	created := gh.Timestamp{Time: gql.CreatedAt}
	updated := gh.Timestamp{Time: gql.UpdatedAt}
	pr.CreatedAt = &created
	pr.UpdatedAt = &updated

	if gql.MergedAt != nil {
		t := gh.Timestamp{Time: *gql.MergedAt}
		pr.MergedAt = &t
		pr.Merged = new(true)
	}
	if gql.MergedBy != nil {
		pr.MergedBy = &gh.User{Login: new(gql.MergedBy.Login)}
	}
	if gql.ClosedAt != nil {
		t := gh.Timestamp{Time: *gql.ClosedAt}
		pr.ClosedAt = &t
	}
	pr.Labels = AdaptLabels(gql.Labels.Nodes)
	pr.Assignees = AdaptAssignees(gql.Assignees.Nodes)
	pr.RequestedReviewers = AdaptRequestedReviewers(gql.ReviewRequests.Nodes)

	if gql.HeadRepository != nil {
		cloneURL := gql.HeadRepository.URL
		if !strings.HasSuffix(cloneURL, ".git") {
			cloneURL += ".git"
		}
		pr.Head.Repo = &gh.Repository{
			CloneURL: new(cloneURL),
		}
	}

	return pr
}

func AdaptRequestedReviewers(nodes []GraphQLReviewRequest) []*gh.User {
	out := make([]*gh.User, 0, len(nodes))
	for _, n := range nodes {
		login := n.Login()
		if login == "" {
			continue
		}
		out = append(out, &gh.User{Login: &login})
	}
	return out
}

func AdaptAssignees(nodes []GraphQLAssigneeID) []*gh.User {
	out := make([]*gh.User, 0, len(nodes))
	for _, n := range nodes {
		login := n.Login
		out = append(out, &gh.User{Login: &login})
	}
	return out
}

func AdaptLabels(labels []GraphQLLabel) []*gh.Label {
	out := make([]*gh.Label, 0, len(labels))
	for _, label := range labels {
		out = append(out, &gh.Label{
			Name:        new(label.Name),
			Color:       new(label.Color),
			Description: new(label.Description),
			Default:     new(label.IsDefault),
		})
	}
	return out
}

func StateToREST(graphqlState string) string {
	switch graphqlState {
	case "MERGED":
		return "closed"
	case "CLOSED":
		return "closed"
	default:
		return "open"
	}
}

func MergeableToREST(mergeable string) string {
	switch mergeable {
	case "MERGEABLE":
		return "clean"
	case "CONFLICTING":
		return "dirty"
	default:
		return "unknown"
	}
}

func GraphQLReviewThreadsComplete(threads []GraphQLReviewThread, hasNextPage bool) bool {
	if hasNextPage {
		return false
	}
	for i := range threads {
		if threads[i].Comments.PageInfo.HasNextPage {
			return false
		}
	}
	return true
}

// GraphQLPageInfo holds GraphQL pagination state from a connection's
// GraphQLPageInfo field.
type GraphQLPageInfo struct {
	HasNextPage bool
	EndCursor   string
}

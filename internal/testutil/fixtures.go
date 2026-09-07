package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	platformgithub "go.kenn.io/forge/platform/github"
)

// SeedResult holds references to seeded data for use in E2E tests.
type SeedResult struct {
	// FixtureClient builds a FixtureClient populated with the seeded open items.
	FixtureClient func() *FixtureClient
}

// SeedFixtures populates d with a synthetic data set for E2E tests and returns
// a SeedResult containing a FixtureClient factory for the seeded open items.
func SeedFixtures(ctx context.Context, d *db.DB) (*SeedResult, error) {
	now := time.Now().UTC()

	// --- Repos ---
	widgetsID, err := d.UpsertRepo(ctx, fixtureRepoIdentity("acme", "widgets"))
	if err != nil {
		return nil, fmt.Errorf("upsert acme/widgets: %w", err)
	}
	if err := d.UpdateRepoViewerCanMerge(ctx, widgetsID, true); err != nil {
		return nil, fmt.Errorf("grant fixture merge permission for acme/widgets: %w", err)
	}
	toolsID, err := d.UpsertRepo(ctx, fixtureRepoIdentity("acme", "tools"))
	if err != nil {
		return nil, fmt.Errorf("upsert acme/tools: %w", err)
	}
	if err := d.UpdateRepoViewerCanMerge(ctx, toolsID, true); err != nil {
		return nil, fmt.Errorf("grant fixture merge permission for acme/tools: %w", err)
	}
	const toolsCloneURL = "https://github.com/acme/tools.git"
	if err := d.UpdateRepoProviderMetadata(ctx, toolsID, db.RepoProviderMetadata{
		WebURL:        "https://github.com/acme/tools",
		CloneURL:      toolsCloneURL,
		DefaultBranch: "main",
	}); err != nil {
		return nil, fmt.Errorf("update acme/tools metadata: %w", err)
	}
	_, err = d.UpsertRepo(ctx, fixtureRepoIdentity("acme", "archived"))
	if err != nil {
		return nil, fmt.Errorf("upsert acme/archived: %w", err)
	}

	// --- Pull Requests ---

	const (
		widgetsPR1HeadSHA = "1111111111111111111111111111111111111111"
		widgetsPR7HeadSHA = "7777777777777777777777777777777777777777"
		toolsPR1HeadSHA   = "2001200120012001200120012001200120012001"
	)
	buildDuration := int64(90)
	lintDuration := int64(45)
	testDuration := int64(120)
	checkStarted := now.Add(-10 * time.Minute)

	ciChecksJSON, err := json.Marshal([]db.CICheck{
		{
			Name:            "build",
			Status:          "completed",
			Conclusion:      "success",
			URL:             "https://github.com/acme/widgets/actions/runs/1/job/1",
			App:             "GitHub Actions",
			DurationSeconds: &buildDuration,
		},
		{
			Name:            "lint",
			Status:          "completed",
			Conclusion:      "success",
			URL:             "https://github.com/acme/widgets/actions/runs/1/job/3",
			App:             "GitHub Actions",
			DurationSeconds: &lintDuration,
		},
		{
			Name:       "roborev",
			Status:     "in_progress",
			Conclusion: "",
			URL:        "",
			App:        "roborev",
		},
		{
			Name:            "test",
			Status:          "completed",
			Conclusion:      "success",
			URL:             "https://github.com/acme/widgets/actions/runs/1/job/2",
			App:             "GitHub Actions",
			DurationSeconds: &testDuration,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal widgets#1 ci checks: %w", err)
	}

	// widgets#1: open, alice, has reviews+comments+4 commits
	w1Created := now.Add(-10 * 24 * time.Hour)
	w1Body := "## Summary\n\n" +
		"Introduce an LRU cache in front of the widget store.\n\n" +
		"## Test plan\n\n" +
		"- [ ] Cmd+K opens palette, focus lands in the search input\n" +
		"- [ ] Tab/Shift+Tab cycles within the palette dialog only\n" +
		"- [ ] `>settings` + Enter navigates to /settings\n" +
		"- [x] Cache invalidates on widget update\n" +
		"- [x] Small, scoped PR (< 500 total lines excluding tests); or opened as Draft with a plan on how to break it into smaller pieces\n"
	w1ID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:            widgetsID,
		PlatformID:        1001,
		Number:            1,
		URL:               "https://github.com/acme/widgets/pull/1",
		Title:             "Add widget caching layer",
		Author:            "alice",
		AuthorDisplayName: "Alice",
		State:             "open",
		Body:              w1Body,
		HeadBranch:        "feature/caching",
		BaseBranch:        "main",
		HeadRepoCloneURL:  "https://github.com/acme/widgets.git",
		PlatformHeadSHA:   widgetsPR1HeadSHA,
		Additions:         240,
		Deletions:         30,
		CommentCount:      3,
		ReviewDecision:    "approved",
		CIStatus:          "success",
		CIChecksJSON:      string(ciChecksJSON),
		CreatedAt:         w1Created,
		UpdatedAt:         now.Add(-2 * time.Hour),
		LastActivityAt:    now.Add(-2 * time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets#1: %w", err)
	}
	if err := d.UpsertMREvents(ctx, []db.MREvent{
		{
			MergeRequestID: w1ID,
			PlatformID:     new(int64(5011)),
			EventType:      "review",
			Author:         "alice",
			Summary:        "APPROVED",
			CreatedAt:      w1Created.Add(2 * time.Hour),
			DedupeKey:      "review-5011",
		},
		{
			MergeRequestID: w1ID,
			PlatformID:     new(int64(5012)),
			EventType:      "review",
			Author:         "bob",
			Summary:        "CHANGES_REQUESTED",
			CreatedAt:      w1Created.Add(3 * time.Hour),
			DedupeKey:      "review-5012",
		},
		{
			MergeRequestID: w1ID,
			PlatformID:     new(int64(5013)),
			EventType:      "review",
			Author:         "bob",
			Summary:        "APPROVED",
			CreatedAt:      w1Created.Add(4 * time.Hour),
			DedupeKey:      "review-5013",
		},
		{
			MergeRequestID: w1ID,
			PlatformID:     new(int64(5014)),
			EventType:      "review",
			Author:         "carol",
			Summary:        "APPROVED",
			CreatedAt:      w1Created.Add(5 * time.Hour),
			DedupeKey:      "review-5014",
		},
	}); err != nil {
		return nil, fmt.Errorf("upsert widgets#1 review events: %w", err)
	}
	if err := d.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: w1ID,
		PlatformID:     new(int64(5014)),
		EventType:      "review",
		Author:         "carol",
		Summary:        "DISMISSED",
		CreatedAt:      w1Created.Add(6 * time.Hour),
		DedupeKey:      "review-5014",
	}}); err != nil {
		return nil, fmt.Errorf("dismiss widgets#1 carol review event: %w", err)
	}

	// widgets#2: open, bob, dirty merge state
	w2Created := now.Add(-8 * 24 * time.Hour)
	w2ID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:            widgetsID,
		PlatformID:        1002,
		Number:            2,
		URL:               "https://github.com/acme/widgets/pull/2",
		Title:             "Fix race condition in event loop",
		Author:            "bob",
		AuthorDisplayName: "Bob",
		State:             "open",
		HeadBranch:        "fix/race-condition",
		BaseBranch:        "main",
		Additions:         55,
		Deletions:         12,
		CommentCount:      2,
		ReviewDecision:    "CHANGES_REQUESTED",
		MergeableState:    "dirty",
		CreatedAt:         w2Created,
		UpdatedAt:         now.Add(-20 * time.Hour),
		LastActivityAt:    now.Add(-20 * time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets#2: %w", err)
	}

	// widgets#3: merged 4d ago, carol
	w3Merged := now.Add(-4 * 24 * time.Hour)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:            widgetsID,
		PlatformID:        1003,
		Number:            3,
		URL:               "https://github.com/acme/widgets/pull/3",
		Title:             "Upgrade dependency versions",
		Author:            "carol",
		AuthorDisplayName: "Carol",
		State:             "merged",
		HeadBranch:        "chore/deps",
		BaseBranch:        "main",
		Additions:         80,
		Deletions:         80,
		CreatedAt:         now.Add(-10 * 24 * time.Hour),
		UpdatedAt:         w3Merged,
		LastActivityAt:    w3Merged,
		MergedAt:          &w3Merged,
		ClosedAt:          &w3Merged,
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets#3: %w", err)
	}

	// widgets#4: merged 25d ago, alice
	w4Merged := now.Add(-25 * 24 * time.Hour)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:            widgetsID,
		PlatformID:        1004,
		Number:            4,
		URL:               "https://github.com/acme/widgets/pull/4",
		Title:             "Refactor storage backend",
		Author:            "alice",
		AuthorDisplayName: "Alice",
		State:             "merged",
		HeadBranch:        "refactor/storage",
		BaseBranch:        "main",
		Additions:         420,
		Deletions:         310,
		CreatedAt:         now.Add(-30 * 24 * time.Hour),
		UpdatedAt:         w4Merged,
		LastActivityAt:    w4Merged,
		MergedAt:          &w4Merged,
		ClosedAt:          &w4Merged,
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets#4: %w", err)
	}

	// widgets#5: closed not merged, 5d ago, bob
	w5Closed := now.Add(-5 * 24 * time.Hour)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:            widgetsID,
		PlatformID:        1005,
		Number:            5,
		URL:               "https://github.com/acme/widgets/pull/5",
		Title:             "Experimental new API",
		Author:            "bob",
		AuthorDisplayName: "Bob",
		State:             "closed",
		IsLocked:          true,
		HeadBranch:        "experiment/new-api",
		BaseBranch:        "main",
		Additions:         900,
		Deletions:         0,
		CreatedAt:         now.Add(-15 * 24 * time.Hour),
		UpdatedAt:         w5Closed,
		LastActivityAt:    w5Closed,
		ClosedAt:          &w5Closed,
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets#5: %w", err)
	}

	// widgets#6: open draft, carol
	w6Created := now.Add(-3 * 24 * time.Hour)
	w6ID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:            widgetsID,
		PlatformID:        1006,
		Number:            6,
		URL:               "https://github.com/acme/widgets/pull/6",
		Title:             "WIP: new dashboard layout",
		Author:            "carol",
		AuthorDisplayName: "Carol",
		State:             "open",
		IsDraft:           true,
		HeadBranch:        "wip/dashboard",
		BaseBranch:        "main",
		Additions:         150,
		Deletions:         40,
		CreatedAt:         w6Created,
		UpdatedAt:         now.Add(-12 * time.Hour),
		LastActivityAt:    now.Add(-12 * time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets#6: %w", err)
	}

	// widgets#7: open, dependabot[bot], opened recently with no other ledger
	// events so Activity treats it as recent parent activity without a
	// selectable event row.
	w7Created := now.Add(-6 * time.Hour)
	_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:            widgetsID,
		PlatformID:        1007,
		Number:            7,
		URL:               "https://github.com/acme/widgets/pull/7",
		Title:             "Bump lodash from 4.17.20 to 4.17.21",
		Author:            "dependabot[bot]",
		AuthorDisplayName: "dependabot[bot]",
		State:             "open",
		HeadBranch:        "dependabot/npm_and_yarn/lodash-4.17.21",
		BaseBranch:        "main",
		PlatformHeadSHA:   widgetsPR7HeadSHA,
		Additions:         1,
		Deletions:         1,
		CreatedAt:         w7Created,
		UpdatedAt:         now.Add(-6 * time.Hour),
		LastActivityAt:    now.Add(-6 * time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets#7: %w", err)
	}

	// tools#1: open, dave
	t1Created := now.Add(-6 * 24 * time.Hour)
	t1ID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:            toolsID,
		PlatformID:        2001,
		Number:            1,
		URL:               "https://github.com/acme/tools/pull/1",
		Title:             "Add CLI flag parser",
		Author:            "dave",
		AuthorDisplayName: "Dave",
		State:             "open",
		HeadBranch:        "feature/cli-flags",
		BaseBranch:        "main",
		PlatformHeadSHA:   toolsPR1HeadSHA,
		Additions:         180,
		Deletions:         20,
		CreatedAt:         t1Created,
		UpdatedAt:         now.Add(-18 * time.Hour),
		LastActivityAt:    now.Add(-18 * time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("upsert tools#1: %w", err)
	}

	// tools#2: merged 60d ago, alice
	t2Merged := now.Add(-60 * 24 * time.Hour)
	t2ID, err := d.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:            toolsID,
		PlatformID:        2002,
		Number:            2,
		URL:               "https://github.com/acme/tools/pull/2",
		Title:             "Initial project setup",
		Author:            "alice",
		AuthorDisplayName: "Alice",
		State:             "merged",
		HeadBranch:        "init",
		BaseBranch:        "main",
		Additions:         500,
		Deletions:         0,
		CreatedAt:         now.Add(-62 * 24 * time.Hour),
		UpdatedAt:         t2Merged,
		LastActivityAt:    t2Merged,
		MergedAt:          &t2Merged,
		ClosedAt:          &t2Merged,
	})
	if err != nil {
		return nil, fmt.Errorf("upsert tools#2: %w", err)
	}

	// tools#10/#11/#12: open stacked PR chain (auth refactor).
	// Forms: main <- feat/auth-base <- feat/auth-retry <- feat/auth-ui
	stackBase := now.Add(-4 * 24 * time.Hour)
	stackPRs := []struct {
		num                int
		title, head, base  string
		ci, review, author string
		mergeableState     string
	}{
		{10, "Auth: extract token refresh helper", "feat/auth-base", "main", "success", "APPROVED", "alice", "dirty"},
		{11, "Auth: add retry with backoff", "feat/auth-retry", "feat/auth-base", "success", "", "alice", ""},
		{12, "Auth: error handling UI", "feat/auth-ui", "feat/auth-retry", "pending", "", "alice", ""},
	}
	buildToolsStackFixturePR := func(i int) *gh.PullRequest {
		pr := stackPRs[i]
		created := stackBase.Add(time.Duration(i) * time.Hour)
		return setPRBranches(
			setPRStats(
				buildGHPR("acme", "tools", int64(2010+i), pr.num, pr.title, pr.author, "open", false, pr.mergeableState, created, created),
				50+i*10,
				5,
			),
			pr.head,
			pr.base,
		)
	}
	for i, pr := range stackPRs {
		created := stackBase.Add(time.Duration(i) * time.Hour)
		_, err = d.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID:            toolsID,
			PlatformID:        int64(2010 + i),
			Number:            pr.num,
			URL:               fmt.Sprintf("https://github.com/acme/tools/pull/%d", pr.num),
			Title:             pr.title,
			Author:            pr.author,
			AuthorDisplayName: "Alice",
			State:             "open",
			HeadBranch:        pr.head,
			BaseBranch:        pr.base,
			HeadRepoCloneURL:  toolsCloneURL,
			CIStatus:          pr.ci,
			ReviewDecision:    pr.review,
			MergeableState:    pr.mergeableState,
			Additions:         50 + i*10,
			Deletions:         5,
			CreatedAt:         created,
			UpdatedAt:         created,
			LastActivityAt:    created,
		})
		if err != nil {
			return nil, fmt.Errorf("upsert tools#%d: %w", pr.num, err)
		}
	}

	// --- Issues ---

	// widgets#10: open, eve
	wi10Created := now.Add(-5 * 24 * time.Hour)
	wi10Body := "Safari users report the widget panel going blank after resizing the page.\n\n" +
		"Observed on Safari 17.4 with the default widget theme. Chrome and Firefox keep rendering the panel.\n\n" +
		"## Triage notes\n\n" +
		"- Reproduces after opening the dashboard, resizing below 900px, then returning to desktop width\n" +
		"- Suspect CSS containment around the widget frame\n" +
		"- Needs a reduced test case before assigning implementation\n"
	wi10ID, err := d.UpsertIssue(ctx, &db.Issue{
		RepoID:         widgetsID,
		PlatformID:     3010,
		Number:         10,
		URL:            "https://github.com/acme/widgets/issues/10",
		Title:          "Widget rendering broken on Safari",
		Author:         "eve",
		State:          "open",
		Body:           wi10Body,
		CommentCount:   2,
		CreatedAt:      wi10Created,
		UpdatedAt:      now.Add(-4 * time.Hour),
		LastActivityAt: now.Add(-4 * time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets issue#10: %w", err)
	}

	// widgets#11: open, alice, older (20d ago)
	wi11Created := now.Add(-20 * 24 * time.Hour)
	wi11Body := "## Acceptance criteria\n\n" +
		"- [ ] System preference detected on first launch\n" +
		"- [ ] Manual toggle in settings overrides system\n" +
		"- [ ] Choice persists across reloads\n" +
		"- [x] Theme tokens defined in design system\n"
	wi11ID, err := d.UpsertIssue(ctx, &db.Issue{
		RepoID:         widgetsID,
		PlatformID:     3011,
		Number:         11,
		URL:            "https://github.com/acme/widgets/issues/11",
		Title:          "Add dark mode support",
		Author:         "alice",
		State:          "open",
		Body:           wi11Body,
		CommentCount:   0,
		CreatedAt:      wi11Created,
		UpdatedAt:      wi11Created,
		LastActivityAt: wi11Created,
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets issue#11: %w", err)
	}

	// widgets#12: closed 3d ago, bob
	wi12Closed := now.Add(-3 * 24 * time.Hour)
	wi12ID, err := d.UpsertIssue(ctx, &db.Issue{
		RepoID:         widgetsID,
		PlatformID:     3012,
		Number:         12,
		URL:            "https://github.com/acme/widgets/issues/12",
		Title:          "Crash on empty input",
		Author:         "bob",
		State:          "closed",
		CommentCount:   1,
		CreatedAt:      now.Add(-10 * 24 * time.Hour),
		UpdatedAt:      wi12Closed,
		LastActivityAt: wi12Closed,
		ClosedAt:       &wi12Closed,
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets issue#12: %w", err)
	}

	// widgets#13: open, dependabot[bot]
	wi13Created := now.Add(-2 * 24 * time.Hour)
	wi13ID, err := d.UpsertIssue(ctx, &db.Issue{
		RepoID:         widgetsID,
		PlatformID:     3013,
		Number:         13,
		URL:            "https://github.com/acme/widgets/issues/13",
		Title:          "Security advisory: prototype pollution",
		Author:         "dependabot[bot]",
		State:          "open",
		CommentCount:   0,
		CreatedAt:      wi13Created,
		UpdatedAt:      wi13Created,
		LastActivityAt: wi13Created,
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets issue#13: %w", err)
	}

	// tools#5: open, dave
	ti5Created := now.Add(-7 * 24 * time.Hour)
	ti5ID, err := d.UpsertIssue(ctx, &db.Issue{
		RepoID:         toolsID,
		PlatformID:     4005,
		Number:         5,
		URL:            "https://github.com/acme/tools/issues/5",
		Title:          "Support config file loading",
		Author:         "dave",
		State:          "open",
		CommentCount:   1,
		CreatedAt:      ti5Created,
		UpdatedAt:      now.Add(-16 * time.Hour),
		LastActivityAt: now.Add(-16 * time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("upsert tools issue#5: %w", err)
	}

	// --- PR Events ---

	// widgets PR#1: 2 comments (bob, carol), 1 review (bob APPROVED), commits around a force-push
	commitBase := now.Add(-9 * 24 * time.Hour)
	w1Commit1 := "abc1111111111111111111111111111111111111"
	w1Commit2 := "abc2222222222222222222222222222222222222"
	w1Commit3 := "abc3333333333333333333333333333333333333"
	w1OldHead := "abc4444444444444444444444444444444444444"
	w1NewCommit := "def3333333333333333333333333333333333333"
	w1NewHead := "def5555555555555555555555555555555555555"
	w1SecondMissingBefore := "abc9999999999999999999999999999999999999"
	w1SecondHead := "def7777777777777777777777777777777777777"
	w1FollowUp := "def6666666666666666666666666666666666666"
	w1BobCommentUTC := time.Date(now.Year(), now.Month(), now.Day(), 1, 30, 0, 0, time.UTC).Add(-8 * 24 * time.Hour)
	w1BobComment, err := time.Parse(
		time.RFC3339,
		w1BobCommentUTC.Add(-4*time.Hour).Format("2006-01-02T15:04:05")+"-04:00",
	)
	if err != nil {
		return nil, fmt.Errorf("build widgets PR#1 non-UTC comment timestamp: %w", err)
	}
	err = d.UpsertMREvents(ctx, []db.MREvent{
		{
			MergeRequestID: w1ID,
			EventType:      "issue_comment",
			Author:         "bob",
			Body:           "Looks like a solid approach. Minor nit on naming.",
			CreatedAt:      w1BobComment,
			DedupeKey:      "w1-comment-bob-1",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "issue_comment",
			Author:         "carol",
			Body:           "I agree, caching here will help a lot.",
			CreatedAt:      now.Add(-6 * 24 * time.Hour),
			DedupeKey:      "w1-comment-carol-1",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "review",
			Author:         "bob",
			Summary:        "APPROVED",
			Body:           "LGTM after addressing the naming nit.",
			CreatedAt:      now.Add(-5 * 24 * time.Hour),
			DedupeKey:      "w1-review-bob-1",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "commit",
			Author:         "alice",
			Summary:        w1Commit1,
			Body:           "feat: add cache store\n\nCache entries now expire when pull request detail data is refreshed.",
			CreatedAt:      commitBase,
			DedupeKey:      "w1-commit-1",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "commit",
			Author:         "alice",
			Summary:        w1Commit2,
			Body:           "feat: wire cache into handler",
			CreatedAt:      commitBase.Add(2 * time.Hour),
			DedupeKey:      "w1-commit-2",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "commit",
			Author:         "alice",
			Summary:        w1Commit3,
			Body:           "test: add cache unit tests",
			CreatedAt:      commitBase.Add(4 * time.Hour),
			DedupeKey:      "w1-commit-3",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "commit",
			Author:         "alice",
			Summary:        w1OldHead,
			Body:           "fix: guard nil cache before rebase",
			CreatedAt:      commitBase.Add(6 * time.Hour),
			DedupeKey:      "w1-commit-4",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "force_push",
			Author:         "alice",
			Summary:        "abc4444 -> def5555",
			MetadataJSON:   fmt.Sprintf(`{"before_sha":%q,"after_sha":%q,"ref":"feature/caching"}`, w1OldHead, w1NewHead),
			CreatedAt:      commitBase.Add(8 * time.Hour),
			DedupeKey:      "w1-force-push-1",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "commit",
			Author:         "alice",
			Summary:        w1NewCommit,
			Body:           "test: add cache unit tests after rebase",
			CreatedAt:      commitBase.Add(4 * time.Hour),
			DedupeKey:      "w1-commit-5",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "commit",
			Author:         "alice",
			Summary:        w1NewHead,
			Body:           "fix: guard nil cache after rebase",
			CreatedAt:      commitBase.Add(6 * time.Hour),
			DedupeKey:      "w1-commit-6",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "commit",
			Author:         "alice",
			Summary:        w1SecondHead,
			Body:           "fix: finish cache rebase after follow-up force push",
			CreatedAt:      commitBase.Add(6 * time.Hour),
			DedupeKey:      "w1-commit-8",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "review",
			Author:         "bob",
			Summary:        "COMMENTED",
			Body:           "Same timestamp reviewer note between force-push IDs.",
			CreatedAt:      commitBase.Add(8 * time.Hour),
			DedupeKey:      "review-5015",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "force_push",
			Author:         "alice",
			Summary:        "abc9999 -> def7777",
			MetadataJSON:   fmt.Sprintf(`{"before_sha":%q,"after_sha":%q,"ref":"feature/caching"}`, w1SecondMissingBefore, w1SecondHead),
			CreatedAt:      commitBase.Add(8 * time.Hour),
			DedupeKey:      "w1-force-push-2",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "cross_referenced",
			Author:         "carol",
			Summary:        "Referenced from acme/widgets#10",
			MetadataJSON:   `{"source_type":"Issue","source_owner":"acme","source_repo":"widgets","source_number":10,"source_title":"Widget rendering broken on Safari","source_url":"https://github.com/acme/widgets/issues/10","is_cross_repository":false,"will_close_target":false}`,
			CreatedAt:      commitBase.Add(9 * time.Hour),
			DedupeKey:      "w1-cross-reference-1",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "cross_referenced",
			Author:         "dave",
			Summary:        "Referenced from acme/tools#1",
			MetadataJSON:   `{"source_type":"PullRequest","source_owner":"acme","source_repo":"tools","source_number":1,"source_title":"Add CLI flag parser","source_url":"https://github.com/acme/tools/pull/1","is_cross_repository":true,"will_close_target":false}`,
			CreatedAt:      commitBase.Add(9*time.Hour + 15*time.Minute),
			DedupeKey:      "w1-cross-reference-tools-1",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "cross_referenced",
			Author:         "mallory",
			Summary:        "Referenced from other/repo#77",
			MetadataJSON:   `{"source_type":"PullRequest","source_owner":"other","source_repo":"repo","source_number":77,"source_title":"External follow-up PR","source_url":"https://github.com/other/repo/pull/77","is_cross_repository":true,"will_close_target":false}`,
			CreatedAt:      commitBase.Add(9*time.Hour + 30*time.Minute),
			DedupeKey:      "w1-cross-reference-other-77",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "renamed_title",
			Author:         "alice",
			Summary:        `"Add widget cache" -> "Add widget caching layer"`,
			MetadataJSON:   `{"previous_title":"Add widget cache","current_title":"Add widget caching layer"}`,
			CreatedAt:      commitBase.Add(10 * time.Hour),
			DedupeKey:      "w1-renamed-title-1",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "commit",
			Author:         "alice",
			Summary:        w1FollowUp,
			Body:           "chore: tune cache eviction metrics",
			CreatedAt:      commitBase.Add(10*time.Hour + 30*time.Minute),
			DedupeKey:      "w1-commit-7",
		},
		{
			MergeRequestID: w1ID,
			EventType:      "base_ref_changed",
			Author:         "alice",
			Summary:        "develop -> main",
			MetadataJSON:   `{"previous_ref_name":"develop","current_ref_name":"main"}`,
			CreatedAt:      commitBase.Add(11 * time.Hour),
			DedupeKey:      "w1-base-ref-changed-1",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets PR#1 events: %w", err)
	}

	// widgets PR#2: imported after a force-push, so only the new commit generation is present
	w2MissingOldHead := "2222aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	w2CurrentCommit := "2222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	w2CurrentHead := "2222cccccccccccccccccccccccccccccccccccc"
	w2CommitBase := now.Add(-6 * 24 * time.Hour)
	err = d.UpsertMREvents(ctx, []db.MREvent{
		{
			MergeRequestID: w2ID,
			EventType:      "commit",
			Author:         "alice",
			Summary:        w2CurrentCommit,
			Body:           "test: reproduce widget race after import",
			CreatedAt:      w2CommitBase,
			DedupeKey:      "w2-commit-1",
		},
		{
			MergeRequestID: w2ID,
			EventType:      "commit",
			Author:         "alice",
			Summary:        w2CurrentHead,
			Body:           "fix: guard widget race after import",
			CreatedAt:      w2CommitBase.Add(1 * time.Hour),
			DedupeKey:      "w2-commit-2",
		},
		{
			MergeRequestID: w2ID,
			EventType:      "force_push",
			Author:         "alice",
			Summary:        "2222aaa -> 2222ccc",
			MetadataJSON:   fmt.Sprintf(`{"before_sha":%q,"after_sha":%q,"ref":"feature/race"}`, w2MissingOldHead, w2CurrentHead),
			CreatedAt:      w2CommitBase.Add(2 * time.Hour),
			DedupeKey:      "w2-force-push-1",
		},
		{
			MergeRequestID: w2ID,
			EventType:      "issue_comment",
			Author:         "alice",
			Body:           "Have you considered using a mutex here instead?",
			CreatedAt:      now.Add(-6 * 24 * time.Hour),
			DedupeKey:      "w2-comment-alice-1",
		},
		{
			MergeRequestID: w2ID,
			EventType:      "review",
			Author:         "alice",
			Summary:        "CHANGES_REQUESTED",
			Body:           "Please add a test that reproduces the race condition.",
			CreatedAt:      now.Add(-5 * 24 * time.Hour),
			DedupeKey:      "w2-review-alice-1",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets PR#2 events: %w", err)
	}

	// widgets PR#6: a rewind splits the old lineage before replacement and full-head restoration
	w6OldCommit1 := "6666a11111111111111111111111111111111111"
	w6OldCommit2 := "6666a22222222222222222222222222222222222"
	w6OldCommit3 := "6666a33333333333333333333333333333333333"
	w6OldCommit4 := "6666a44444444444444444444444444444444444"
	w6OldCommit5 := "6666a55555555555555555555555555555555555"
	w6NewCommit1 := "6666b11111111111111111111111111111111111"
	w6NewCommit2 := "6666b22222222222222222222222222222222222"
	w6NewCommit3 := "6666b33333333333333333333333333333333333"
	err = d.UpsertMREvents(ctx, []db.MREvent{
		{
			MergeRequestID: w6ID,
			EventType:      "commit",
			Author:         "carol",
			Summary:        w6OldCommit1,
			Body:           "dashboard original base restored",
			MetadataJSON:   `{"commit_order":1,"commit_order_key":1}`,
			CreatedAt:      w6Created.Add(time.Hour),
			DedupeKey:      "w6-old-commit-1",
		},
		{
			MergeRequestID: w6ID,
			EventType:      "commit",
			Author:         "carol",
			Summary:        w6OldCommit2,
			Body:           "dashboard original filters restored",
			MetadataJSON:   `{"commit_order":2,"commit_order_key":2}`,
			CreatedAt:      w6Created.Add(2 * time.Hour),
			DedupeKey:      "w6-old-commit-2",
		},
		{
			MergeRequestID: w6ID,
			EventType:      "commit",
			Author:         "carol",
			Summary:        w6OldCommit3,
			Body:           "dashboard original widgets restored",
			MetadataJSON:   `{"commit_order":3,"commit_order_key":3}`,
			CreatedAt:      w6Created.Add(3 * time.Hour),
			DedupeKey:      "w6-old-commit-3",
		},
		{
			MergeRequestID: w6ID,
			EventType:      "commit",
			Author:         "carol",
			Summary:        w6OldCommit4,
			Body:           "dashboard original charts restored",
			MetadataJSON:   `{"commit_order":4,"commit_order_key":4}`,
			CreatedAt:      w6Created.Add(4 * time.Hour),
			DedupeKey:      "w6-old-commit-4",
		},
		{
			MergeRequestID: w6ID,
			EventType:      "commit",
			Author:         "carol",
			Summary:        w6OldCommit5,
			Body:           "dashboard original head restored",
			MetadataJSON:   `{"commit_order":5,"commit_order_key":5}`,
			CreatedAt:      w6Created.Add(5 * time.Hour),
			DedupeKey:      "w6-old-commit-5",
		},
		{
			MergeRequestID: w6ID,
			EventType:      "commit",
			Author:         "carol",
			Summary:        w6NewCommit1,
			Body:           "dashboard replacement base displaced",
			MetadataJSON:   `{"commit_order":1,"commit_order_key":6,"obsolete":true}`,
			CreatedAt:      w6Created.Add(6 * time.Hour),
			DedupeKey:      "w6-new-commit-1",
		},
		{
			MergeRequestID: w6ID,
			EventType:      "commit",
			Author:         "carol",
			Summary:        w6NewCommit2,
			Body:           "dashboard replacement filters displaced",
			MetadataJSON:   `{"commit_order":2,"commit_order_key":7,"obsolete":true}`,
			CreatedAt:      w6Created.Add(7 * time.Hour),
			DedupeKey:      "w6-new-commit-2",
		},
		{
			MergeRequestID: w6ID,
			EventType:      "commit",
			Author:         "carol",
			Summary:        w6NewCommit3,
			Body:           "dashboard replacement widgets displaced",
			MetadataJSON:   `{"commit_order":3,"commit_order_key":8,"obsolete":true}`,
			CreatedAt:      w6Created.Add(8 * time.Hour),
			DedupeKey:      "w6-new-commit-3",
		},
		{
			MergeRequestID: w6ID,
			EventType:      "force_push",
			Author:         "carol",
			Summary:        "6666a55 -> 6666a33",
			MetadataJSON:   fmt.Sprintf(`{"before_sha":%q,"after_sha":%q,"ref":"wip/dashboard"}`, w6OldCommit5, w6OldCommit3),
			CreatedAt:      w6Created.Add(9 * time.Hour),
			DedupeKey:      "w6-force-push-1",
		},
		{
			MergeRequestID: w6ID,
			EventType:      "force_push",
			Author:         "carol",
			Summary:        "6666a33 -> 6666b33",
			MetadataJSON:   fmt.Sprintf(`{"before_sha":%q,"after_sha":%q,"ref":"wip/dashboard"}`, w6OldCommit3, w6NewCommit3),
			CreatedAt:      w6Created.Add(10 * time.Hour),
			DedupeKey:      "w6-force-push-2",
		},
		{
			MergeRequestID: w6ID,
			EventType:      "force_push",
			Author:         "carol",
			Summary:        "6666b33 -> 6666a55",
			MetadataJSON:   fmt.Sprintf(`{"before_sha":%q,"after_sha":%q,"ref":"wip/dashboard"}`, w6NewCommit3, w6OldCommit5),
			CreatedAt:      w6Created.Add(11 * time.Hour),
			DedupeKey:      "w6-force-push-3",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets PR#6 events: %w", err)
	}

	// tools PR#1: 1 comment (alice)
	err = d.UpsertMREvents(ctx, []db.MREvent{
		{
			MergeRequestID: t1ID,
			EventType:      "issue_comment",
			Author:         "alice",
			Body:           "Nice work! Should we support short flags too?",
			CreatedAt:      now.Add(-4 * 24 * time.Hour),
			DedupeKey:      "t1-comment-alice-1",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("upsert tools PR#1 events: %w", err)
	}

	// tools PR#2: duplicate lifecycle rows from provider/import paths collapse to one merge row in the UI.
	err = d.UpsertMREvents(ctx, []db.MREvent{
		{
			MergeRequestID: t2ID,
			EventType:      "merged",
			Author:         "",
			Summary:        "merged this",
			CreatedAt:      t2Merged,
			DedupeKey:      "t2-merged-fallback",
		},
		{
			MergeRequestID: t2ID,
			EventType:      "closed",
			Author:         "alice",
			Summary:        "closed this",
			CreatedAt:      t2Merged.Add(15 * time.Second),
			DedupeKey:      "t2-closed-provider",
		},
		{
			MergeRequestID: t2ID,
			EventType:      "merged",
			Author:         "alice",
			Summary:        "merged this",
			CreatedAt:      t2Merged.Add(15 * time.Second),
			DedupeKey:      "t2-merged-provider",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("upsert tools PR#2 lifecycle events: %w", err)
	}

	// --- Issue Events ---

	// widgets issue#10: 2 comments (alice, bob)
	err = d.UpsertIssueEvents(ctx, []db.IssueEvent{
		{
			IssueID:   wi10ID,
			EventType: "issue_comment",
			Author:    "alice",
			Body:      "Confirmed on Safari 17. Looks like a CSS isolation bug.",
			CreatedAt: now.Add(-3 * 24 * time.Hour),
			DedupeKey: "wi10-comment-alice-1",
		},
		{
			IssueID:   wi10ID,
			EventType: "issue_comment",
			Author:    "bob",
			Body:      "I can reproduce too. Will take a look.",
			CreatedAt: now.Add(-2 * 24 * time.Hour),
			DedupeKey: "wi10-comment-bob-1",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets issue#10 events: %w", err)
	}

	// widgets issue#12: 1 comment (carol)
	err = d.UpsertIssueEvents(ctx, []db.IssueEvent{
		{
			IssueID:   wi12ID,
			EventType: "issue_comment",
			Author:    "carol",
			Body:      "Fixed in PR#3.",
			CreatedAt: now.Add(-3 * 24 * time.Hour),
			DedupeKey: "wi12-comment-carol-1",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("upsert widgets issue#12 events: %w", err)
	}

	// tools issue#5: 1 comment (dave)
	err = d.UpsertIssueEvents(ctx, []db.IssueEvent{
		{
			IssueID:   ti5ID,
			EventType: "issue_comment",
			Author:    "dave",
			Body:      "I'll start with TOML support first.",
			CreatedAt: now.Add(-5 * 24 * time.Hour),
			DedupeKey: "ti5-comment-dave-1",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("upsert tools issue#5 events: %w", err)
	}

	notifPRNumber := 1
	notifIssueNumber := 5
	seededNotifications := []db.Notification{
		{
			Platform:               "github",
			PlatformHost:           "github.com",
			PlatformNotificationID: "notif-widgets-1",
			RepoID:                 &widgetsID,
			RepoOwner:              "acme",
			RepoName:               "widgets",
			SubjectType:            "PullRequest",
			SubjectTitle:           "Add widget caching layer",
			WebURL:                 "https://github.com/acme/widgets/pull/1",
			ItemNumber:             &notifPRNumber,
			ItemType:               "pr",
			ItemAuthor:             "alice",
			Reason:                 "review_requested",
			Unread:                 true,
			Participating:          true,
			SourceUpdatedAt:        now.Add(-2 * time.Hour),
			SyncedAt:               now,
		},
		{
			Platform:               "github",
			PlatformHost:           "github.com",
			PlatformNotificationID: "notif-tools-5",
			RepoID:                 &toolsID,
			RepoOwner:              "acme",
			RepoName:               "tools",
			SubjectType:            "Issue",
			SubjectTitle:           "Support config file loading",
			WebURL:                 "https://github.com/acme/tools/issues/5",
			ItemNumber:             &notifIssueNumber,
			ItemType:               "issue",
			ItemAuthor:             "dave",
			Reason:                 "mention",
			Unread:                 true,
			Participating:          true,
			SourceUpdatedAt:        now.Add(-16 * time.Hour),
			SyncedAt:               now,
		},
	}
	if err := d.UpsertNotifications(ctx, seededNotifications); err != nil {
		return nil, fmt.Errorf("upsert notifications: %w", err)
	}
	fixtureNotifications := []ghclient.NotificationThread{
		{
			ID:            "notif-widgets-1",
			RepoOwner:     "acme",
			RepoName:      "widgets",
			SubjectType:   "PullRequest",
			SubjectTitle:  "Add widget caching layer",
			WebURL:        "https://github.com/acme/widgets/pull/1",
			ItemNumber:    &notifPRNumber,
			ItemType:      "pr",
			ItemAuthor:    "alice",
			Reason:        "review_requested",
			Unread:        true,
			Participating: true,
			UpdatedAt:     now.Add(-2 * time.Hour),
		},
		{
			ID:            "notif-tools-5",
			RepoOwner:     "acme",
			RepoName:      "tools",
			SubjectType:   "Issue",
			SubjectTitle:  "Support config file loading",
			WebURL:        "https://github.com/acme/tools/issues/5",
			ItemNumber:    &notifIssueNumber,
			ItemType:      "issue",
			ItemAuthor:    "dave",
			Reason:        "mention",
			Unread:        true,
			Participating: true,
			UpdatedAt:     now.Add(-16 * time.Hour),
		},
	}

	// --- Build FixtureClient open items ---

	openPRs := map[string][]*gh.PullRequest{
		"acme/widgets": {
			// Head branch must match SeedFixtures' DB row and the diff repo's
			// real branch: a provider sync would otherwise rewrite the DB head
			// branch to buildGHPR's placeholder and break workspace worktree
			// provisioning against the git fixture.
			setPRBranches(setPRBody(setPRHeadSHA(setPRStats(buildGHPR("acme", "widgets", 1001, 1, "Add widget caching layer", "alice", "open", false, "", w1Created, now.Add(-2*time.Hour)), 240, 30), widgetsPR1HeadSHA), w1Body), "feature/caching", "main"),
			setPRStats(buildGHPR("acme", "widgets", 1002, 2, "Fix race condition in event loop", "bob", "open", false, "dirty", w2Created, now.Add(-20*time.Hour)), 55, 12),
			setPRStats(buildGHPR("acme", "widgets", 1006, 6, "WIP: new dashboard layout", "carol", "open", true, "", w6Created, now.Add(-12*time.Hour)), 150, 40),
			setPRHeadSHA(setPRStats(buildGHPR("acme", "widgets", 1007, 7, "Bump lodash from 4.17.20 to 4.17.21", "dependabot[bot]", "open", false, "", w7Created, now.Add(-6*time.Hour)), 1, 1), widgetsPR7HeadSHA),
		},
		"acme/tools": {
			setPRHeadSHA(setPRStats(buildGHPR("acme", "tools", 2001, 1, "Add CLI flag parser", "dave", "open", false, "", t1Created, now.Add(-18*time.Hour)), 180, 20), toolsPR1HeadSHA),
			buildToolsStackFixturePR(0),
			buildToolsStackFixturePR(1),
			buildToolsStackFixturePR(2),
		},
	}

	allPRs := map[string][]*gh.PullRequest{
		"acme/widgets": {
			// Head branch must match SeedFixtures' DB row and the diff repo's
			// real branch: a provider sync would otherwise rewrite the DB head
			// branch to buildGHPR's placeholder and break workspace worktree
			// provisioning against the git fixture.
			setPRBranches(setPRBody(setPRHeadSHA(setPRStats(buildGHPR("acme", "widgets", 1001, 1, "Add widget caching layer", "alice", "open", false, "", w1Created, now.Add(-2*time.Hour)), 240, 30), widgetsPR1HeadSHA), w1Body), "feature/caching", "main"),
			setPRStats(buildGHPR("acme", "widgets", 1002, 2, "Fix race condition in event loop", "bob", "open", false, "dirty", w2Created, now.Add(-20*time.Hour)), 55, 12),
			setPRStats(buildGHPR("acme", "widgets", 1003, 3, "Upgrade dependency versions", "carol", "merged", false, "", now.Add(-10*24*time.Hour), w3Merged), 80, 80),
			setPRStats(buildGHPR("acme", "widgets", 1004, 4, "Refactor storage backend", "alice", "merged", false, "", now.Add(-30*24*time.Hour), w4Merged), 420, 310),
			setPRLocked(setPRStats(buildGHPR("acme", "widgets", 1005, 5, "Experimental new API", "bob", "closed", false, "", now.Add(-15*24*time.Hour), w5Closed), 900, 0)),
			setPRStats(buildGHPR("acme", "widgets", 1006, 6, "WIP: new dashboard layout", "carol", "open", true, "", w6Created, now.Add(-12*time.Hour)), 150, 40),
			setPRHeadSHA(setPRStats(buildGHPR("acme", "widgets", 1007, 7, "Bump lodash from 4.17.20 to 4.17.21", "dependabot[bot]", "open", false, "", w7Created, now.Add(-6*time.Hour)), 1, 1), widgetsPR7HeadSHA),
		},
		"acme/tools": {
			setPRHeadSHA(setPRStats(buildGHPR("acme", "tools", 2001, 1, "Add CLI flag parser", "dave", "open", false, "", t1Created, now.Add(-18*time.Hour)), 180, 20), toolsPR1HeadSHA),
			setPRStats(buildGHPR("acme", "tools", 2002, 2, "Initial project setup", "alice", "merged", false, "", now.Add(-62*24*time.Hour), t2Merged), 500, 0),
			buildToolsStackFixturePR(0),
			buildToolsStackFixturePR(1),
			buildToolsStackFixturePR(2),
		},
	}

	openIssues := map[string][]*gh.Issue{
		"acme/widgets": {
			setIssueComments(setIssueBody(buildGHIssue("acme", "widgets", 3010, 10, "Widget rendering broken on Safari", "eve", "open", wi10Created, now.Add(-4*time.Hour)), wi10Body), 2),
			setIssueBody(buildGHIssue("acme", "widgets", 3011, 11, "Add dark mode support", "alice", "open", wi11Created, wi11Created), wi11Body),
			buildGHIssue("acme", "widgets", 3013, 13, "Security advisory: prototype pollution", "dependabot[bot]", "open", wi13Created, wi13Created),
		},
		"acme/tools": {
			buildGHIssue("acme", "tools", 4005, 5, "Support config file loading", "dave", "open", ti5Created, now.Add(-16*time.Hour)),
		},
	}

	sameTimestampReview := buildGHReview(
		5015, "reviewer", "COMMENTED", commitBase.Add(8*time.Hour),
	)
	sameTimestampReview.Body = new("Same timestamp reviewer note between force-push IDs.")
	reviews := map[string][]*gh.PullRequestReview{
		issueKey("acme", "widgets", 1): {
			buildGHReview(5011, "alice", "APPROVED", w1Created.Add(2*time.Hour)),
			buildGHReview(5012, "bob", "CHANGES_REQUESTED", w1Created.Add(3*time.Hour)),
			buildGHReview(5013, "bob", "APPROVED", w1Created.Add(4*time.Hour)),
			buildGHReview(5014, "carol", "DISMISSED", w1Created.Add(6*time.Hour)),
			sameTimestampReview,
		},
	}
	reviewThreads := map[string][]platformgithub.PullRequestReviewThread{
		issueKey("acme", "widgets", 1): {
			{
				NodeID:     "PRRT_cache_guard",
				IsResolved: false,
				Path:       "src/cache/store.ts",
				Side:       "RIGHT",
				Line:       42,
				Comments: []platformgithub.PullRequestReviewThreadComment{
					{
						NodeID:           "PRRC_cache_guard",
						DatabaseID:       6011,
						ReviewDatabaseID: 5012,
						SubjectType:      "LINE",
						Body:             "Guard the cache fallback before returning stale data.\n\nExpanded context explains stale data handling.",
						AuthorLogin:      "bob",
						Path:             "src/cache/store.ts",
						Line:             42,
						URL:              "https://github.com/acme/widgets/pull/1#discussion_r6011",
						CommitID:         widgetsPR1HeadSHA,
						OriginalCommitID: widgetsPR1HeadSHA,
						CreatedAt:        w1Created.Add(3*time.Hour + 15*time.Minute),
						UpdatedAt:        w1Created.Add(3*time.Hour + 15*time.Minute),
					},
					{
						NodeID:           "PRRC_cache_guard_followup",
						DatabaseID:       6012,
						ReviewDatabaseID: 5012,
						SubjectType:      "LINE",
						Body:             "Follow-up compact review context for the same thread.",
						AuthorLogin:      "bob",
						Path:             "src/cache/store.ts",
						Line:             42,
						URL:              "https://github.com/acme/widgets/pull/1#discussion_r6012",
						CommitID:         widgetsPR1HeadSHA,
						OriginalCommitID: widgetsPR1HeadSHA,
						CreatedAt:        w1Created.Add(3*time.Hour + 20*time.Minute),
						UpdatedAt:        w1Created.Add(3*time.Hour + 20*time.Minute),
					},
				},
			},
			{
				NodeID:     "PRRT_reply_regroup",
				IsResolved: false,
				Path:       "src/cache/store.ts",
				Side:       "RIGHT",
				Line:       43,
				Comments: []platformgithub.PullRequestReviewThreadComment{
					{
						NodeID:           "PRRC_reply_regroup_root",
						DatabaseID:       6013,
						ReviewDatabaseID: 5012,
						SubjectType:      "LINE",
						Body:             "Regroup root review thread comment.",
						AuthorLogin:      "bob",
						Path:             "src/cache/store.ts",
						Line:             43,
						URL:              "https://github.com/acme/widgets/pull/1#discussion_r6013",
						CommitID:         widgetsPR1HeadSHA,
						OriginalCommitID: widgetsPR1HeadSHA,
						CreatedAt:        w1Created.Add(3*time.Hour + 25*time.Minute),
						UpdatedAt:        w1Created.Add(3*time.Hour + 25*time.Minute),
					},
				},
			},
		},
	}

	allIssues := map[string][]*gh.Issue{
		"acme/widgets": {
			setIssueComments(setIssueBody(buildGHIssue("acme", "widgets", 3010, 10, "Widget rendering broken on Safari", "eve", "open", wi10Created, now.Add(-4*time.Hour)), wi10Body), 2),
			setIssueBody(buildGHIssue("acme", "widgets", 3011, 11, "Add dark mode support", "alice", "open", wi11Created, wi11Created), wi11Body),
			buildGHIssue("acme", "widgets", 3012, 12, "Crash on empty input", "carol", "closed", now.Add(-7*24*time.Hour), wi12Closed),
			buildGHIssue("acme", "widgets", 3013, 13, "Security advisory: prototype pollution", "dependabot[bot]", "open", wi13Created, wi13Created),
		},
		"acme/tools": {
			buildGHIssue("acme", "tools", 4005, 5, "Support config file loading", "dave", "open", ti5Created, now.Add(-16*time.Hour)),
		},
	}

	// Suppress unused variable warnings for IDs only needed for event insertion.
	_ = wi11ID
	_ = wi13ID

	result := &SeedResult{
		FixtureClient: func() *FixtureClient {
			return &FixtureClient{
				OpenPRs:    openPRs,
				PRs:        allPRs,
				OpenIssues: openIssues,
				Issues:     allIssues,
				Comments: map[string][]*gh.IssueComment{
					issueKey("acme", "widgets", 10): {
						buildGHIssueComment(
							7010,
							"alice",
							"Confirmed on Safari 17. Looks like the widget frame loses its computed height after the resize.",
							"https://github.com/acme/widgets/issues/10#issuecomment-7010",
							now.Add(-3*24*time.Hour),
						),
						buildGHIssueComment(
							7011,
							"bob",
							"I can reproduce on 17.4. I will reduce it to the containment rule before assigning the fix.",
							"https://github.com/acme/widgets/issues/10#issuecomment-7011",
							now.Add(-2*24*time.Hour),
						),
					},
				},
				Reviews:       reviews,
				ReviewThreads: reviewThreads,
				Tags:          make(map[string][]*gh.RepositoryTag),
				Labels:        make(map[string][]*gh.Label),
				Notifications: fixtureNotifications,
				CombinedStatuses: map[string]*gh.CombinedStatus{
					refKey("acme", "widgets", widgetsPR1HeadSHA): {
						State: new("success"),
					},
				},
				CheckRuns: map[string][]*gh.CheckRun{
					refKey("acme", "widgets", widgetsPR1HeadSHA): {
						{
							Name:        new("build"),
							Status:      new("completed"),
							Conclusion:  new("success"),
							HTMLURL:     new("https://github.com/acme/widgets/actions/runs/1/job/1"),
							StartedAt:   &gh.Timestamp{Time: checkStarted},
							CompletedAt: &gh.Timestamp{Time: checkStarted.Add(time.Duration(buildDuration) * time.Second)},
							App:         &gh.App{Name: new("GitHub Actions")},
						},
						{
							Name:        new("test"),
							Status:      new("completed"),
							Conclusion:  new("success"),
							HTMLURL:     new("https://github.com/acme/widgets/actions/runs/1/job/2"),
							StartedAt:   &gh.Timestamp{Time: checkStarted},
							CompletedAt: &gh.Timestamp{Time: checkStarted.Add(time.Duration(testDuration) * time.Second)},
							App:         &gh.App{Name: new("GitHub Actions")},
						},
						{
							Name:        new("lint"),
							Status:      new("completed"),
							Conclusion:  new("success"),
							HTMLURL:     new("https://github.com/acme/widgets/actions/runs/1/job/3"),
							StartedAt:   &gh.Timestamp{Time: checkStarted},
							CompletedAt: &gh.Timestamp{Time: checkStarted.Add(time.Duration(lintDuration) * time.Second)},
							App:         &gh.App{Name: new("GitHub Actions")},
						},
						{
							Name:       new("roborev"),
							Status:     new("in_progress"),
							Conclusion: new(""),
							HTMLURL:    new(""),
							App:        &gh.App{Name: new("roborev")},
						},
					},
				},
				WorkflowRuns: make(map[string][]*gh.WorkflowRun),
				nextID:       10_000,
			}
		},
	}
	return result, nil
}

func fixtureRepoIdentity(owner, name string) db.RepoIdentity {
	return db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "repo-" + owner + "-" + name,
		Owner:          owner,
		Name:           name,
	}
}

func buildGHReview(id int64, login, state string, submittedAt time.Time) *gh.PullRequestReview {
	return &gh.PullRequestReview{
		ID:          new(id),
		User:        &gh.User{Login: new(login)},
		State:       new(state),
		SubmittedAt: &gh.Timestamp{Time: submittedAt},
	}
}

// buildGHPR creates a minimal *gh.PullRequest for the FixtureClient.
func buildGHPR(
	owner, repo string,
	id int64, number int, title, login, state string,
	draft bool, mergeableState string,
	createdAt, updatedAt time.Time,
) *gh.PullRequest {
	url := fmt.Sprintf(
		"https://github.com/%s/%s/pull/%d", owner, repo, number)
	cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	pr := &gh.PullRequest{
		ID:        new(id),
		NodeID:    new(fmt.Sprintf("PR_%d", id)),
		Number:    new(number),
		Title:     new(title),
		HTMLURL:   new(url),
		State:     new(state),
		Draft:     new(draft),
		User:      &gh.User{Login: new(login)},
		CreatedAt: &gh.Timestamp{Time: createdAt},
		UpdatedAt: &gh.Timestamp{Time: updatedAt},
		Head: &gh.PullRequestBranch{
			Ref:  new("feature"),
			Repo: &gh.Repository{CloneURL: &cloneURL},
		},
		Base: &gh.PullRequestBranch{Ref: new("main")},
	}
	if mergeableState != "" {
		pr.MergeableState = new(mergeableState)
	}
	return pr
}

// setPRBody sets a Body on a *gh.PullRequest so the fixture client
// returns prose for sync paths exercising body persistence (e.g.
// task-list checkboxes).
func setPRBody(pr *gh.PullRequest, body string) *gh.PullRequest {
	pr.Body = &body
	return pr
}

func setPRLocked(pr *gh.PullRequest) *gh.PullRequest {
	locked := true
	pr.Locked = &locked
	return pr
}

// setIssueBody sets a Body on a *gh.Issue. Mirrors setPRBody for
// issue-side body edits (task-list checkboxes on issue descriptions).
func setIssueBody(issue *gh.Issue, body string) *gh.Issue {
	issue.Body = &body
	return issue
}

func setIssueComments(issue *gh.Issue, count int) *gh.Issue {
	issue.Comments = &count
	return issue
}

func buildGHIssueComment(id int64, login, body, url string, createdAt time.Time) *gh.IssueComment {
	return &gh.IssueComment{
		ID:        &id,
		Body:      &body,
		HTMLURL:   &url,
		CreatedAt: &gh.Timestamp{Time: createdAt},
		UpdatedAt: &gh.Timestamp{Time: createdAt},
		User:      &gh.User{Login: &login},
	}
}

// setPRStats sets Additions and Deletions on a *gh.PullRequest so the
// fixture client returns non-zero diff stats for sync paths. Returns
// the same pointer for chain-friendly call sites.
func setPRStats(pr *gh.PullRequest, additions, deletions int) *gh.PullRequest {
	pr.Additions = &additions
	pr.Deletions = &deletions
	return pr
}

func setPRHeadSHA(pr *gh.PullRequest, sha string) *gh.PullRequest {
	if pr.Head == nil {
		pr.Head = &gh.PullRequestBranch{}
	}
	pr.Head.SHA = &sha
	return pr
}

func setPRBranches(pr *gh.PullRequest, head, base string) *gh.PullRequest {
	if pr.Head == nil {
		pr.Head = &gh.PullRequestBranch{}
	}
	if pr.Base == nil {
		pr.Base = &gh.PullRequestBranch{}
	}
	pr.Head.Ref = &head
	pr.Base.Ref = &base
	return pr
}

// buildGHIssue creates a minimal *gh.Issue for the FixtureClient.
func buildGHIssue(
	owner, repo string,
	id int64, number int, title, login, state string,
	createdAt, updatedAt time.Time,
) *gh.Issue {
	url := fmt.Sprintf(
		"https://github.com/%s/%s/issues/%d", owner, repo, number)
	return &gh.Issue{
		ID:        new(id),
		Number:    new(number),
		Title:     new(title),
		HTMLURL:   new(url),
		State:     new(state),
		User:      &gh.User{Login: new(login)},
		CreatedAt: &gh.Timestamp{Time: createdAt},
		UpdatedAt: &gh.Timestamp{Time: updatedAt},
	}
}

// OpenFixtureTestDB opens a temporary SQLite database seeded with fixture data.
// It returns the DB and SeedResult. The DB is closed automatically via t.Cleanup.
func OpenFixtureTestDB(t *testing.T) (*db.DB, *SeedResult) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })

	result, err := SeedFixtures(context.Background(), d)
	require.NoError(t, err)
	return d, result
}

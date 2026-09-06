package tokenauth

import (
	"context"
	"errors"
	"fmt"
	"go.kenn.io/forge/platform"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func githubAppDescriptor(installationID int64) Descriptor {
	return Descriptor{
		Key: Key{Platform: "github", Host: "github.com"},
		Candidates: []Candidate{
			{
				Kind:           SourceKindGitHubApp,
				Host:           "github.com",
				FilePath:       "/keys/app.pem",
				AppID:          77,
				InstallationID: installationID,
			},
			{Kind: SourceKindEnv, EnvName: "TEST_GITHUB_APP_FALLBACK"},
		},
	}
}

func TestGitHubAppTokenMintAndCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var mints atomic.Int64
	src := NewManagedSource(githubAppDescriptor(42), Options{
		GitHubApp: func(_ context.Context, c Candidate) (string, time.Time, error) {
			mints.Add(1)
			assert.Equal(int64(77), c.AppID)
			assert.Equal(int64(42), c.InstallationID)
			assert.Equal("/keys/app.pem", c.FilePath)
			return "ghs_minted", time.Now().Add(time.Hour), nil
		},
	})

	token, err := src.Token(context.Background())
	require.NoError(err)
	assert.Equal("ghs_minted", token)

	// A second resolve inside the expiry window reuses the cache.
	token, err = src.Token(context.Background())
	require.NoError(err)
	assert.Equal("ghs_minted", token)
	assert.Equal(int64(1), mints.Load())

	// Invalidate (e.g. a 401 retry in platform.AuthTransport) forces a re-mint.
	src.Invalidate("ghs_minted")
	_, err = src.Token(context.Background())
	require.NoError(err)
	assert.Equal(int64(2), mints.Load())
}

func TestGitHubAppTokenCacheIsSharedAcrossExactRoutes(t *testing.T) {
	var mints atomic.Int64
	set := NewSourceSet(Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			mints.Add(1)
			return "ghs_shared", time.Now().Add(time.Hour), nil
		},
	})
	first := githubAppDescriptor(42)
	first.Key.Scope = "repo:kenn-io/one"
	first.Candidates[0].InstallationAccount = "kenn-io"
	second := githubAppDescriptor(42)
	second.Key.Scope = "repo:kenn-io/two"
	second.Candidates[0].InstallationAccount = "kenn-io"

	ctx := WithGitHubOwner(context.Background(), "kenn-io")
	_, err := set.Upsert(first).Token(ctx)
	require.NoError(t, err)
	_, err = set.Upsert(second).Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), mints.Load(),
		"one installation token must be reused across repository-exact routes")
}

func TestGitHubAppTokenRemintsNearExpiry(t *testing.T) {
	var mints atomic.Int64
	src := NewManagedSource(githubAppDescriptor(42), Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			mints.Add(1)
			// Always within the refresh skew, so every resolve re-mints.
			return "ghs_shortlived", time.Now().Add(time.Minute), nil
		},
	})
	for range 2 {
		_, err := src.Token(context.Background())
		require.NoError(t, err)
	}
	assert.Equal(t, int64(2), mints.Load())
}

func TestGitHubAppNotInstalledFallsThrough(t *testing.T) {
	t.Setenv("TEST_GITHUB_APP_FALLBACK", "pat-token")
	src := NewManagedSource(githubAppDescriptor(0), Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			return "", time.Time{}, errors.New("must not be called for installation 0")
		},
	})
	token, err := src.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "pat-token", token)
}

func TestGitHubAppNilMinterFallsThrough(t *testing.T) {
	t.Setenv("TEST_GITHUB_APP_FALLBACK", "pat-token")
	src := NewManagedSource(githubAppDescriptor(42), Options{})
	token, err := src.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "pat-token", token)
}

func TestGitHubAppRequiresMatchingOwnerScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("TEST_GITHUB_APP_FALLBACK", "pat-token")
	var mints atomic.Int64
	desc := githubAppDescriptor(42)
	desc.Candidates[0].InstallationAccount = "kenn-io"
	src := NewManagedSource(desc, Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			mints.Add(1)
			return "ghs_minted", time.Now().Add(time.Hour), nil
		},
	})

	token, err := src.Token(context.Background())
	require.NoError(err)
	assert.Equal("pat-token", token)

	token, err = src.Token(WithGitHubOwner(context.Background(), "mariusvniekerk"))
	require.NoError(err)
	assert.Equal("pat-token", token)

	token, err = src.Token(WithGitHubOwner(context.Background(), "Kenn-IO"))
	require.NoError(err)
	assert.Equal("ghs_minted", token)
	assert.Equal(int64(1), mints.Load())
}

func TestGitHubAppCacheIsScopedToInstallationAccount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	desc := githubAppDescriptor(42)
	desc.Candidates = []Candidate{
		{
			Kind:                SourceKindGitHubApp,
			Host:                "github.com",
			FilePath:            "/tmp/app.pem",
			AppID:               77,
			InstallationID:      42,
			InstallationAccount: "kenn-io",
		},
		{
			Kind:                SourceKindGitHubApp,
			Host:                "github.com",
			FilePath:            "/tmp/app.pem",
			AppID:               77,
			InstallationID:      43,
			InstallationAccount: "other-org",
		},
	}
	minted := make(map[int64]int)
	src := NewManagedSource(desc, Options{
		GitHubApp: func(_ context.Context, c Candidate) (string, time.Time, error) {
			minted[c.InstallationID]++
			return fmt.Sprintf("ghs_%d", c.InstallationID), time.Now().Add(time.Hour), nil
		},
	})

	token, err := src.Token(WithGitHubOwner(context.Background(), "kenn-io"))
	require.NoError(err)
	assert.Equal("ghs_42", token)

	token, err = src.Token(WithGitHubOwner(context.Background(), "other-org"))
	require.NoError(err)
	assert.Equal("ghs_43", token)

	token, err = src.Token(WithGitHubOwner(context.Background(), "kenn-io"))
	require.NoError(err)
	assert.Equal("ghs_42", token)
	assert.Equal(map[int64]int{42: 1, 43: 1}, minted)
}

func TestGitHubAppMintFailureSurfacesError(t *testing.T) {
	t.Setenv("TEST_GITHUB_APP_FALLBACK", "pat-token")
	src := NewManagedSource(githubAppDescriptor(42), Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			return "", time.Time{}, errors.New("key rejected")
		},
	})
	// Mint failures must not silently degrade to the PAT chain: the
	// app exists because the PAT budget is exhausted.
	_, err := src.Token(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "key rejected")
	require.ErrorContains(t, err, "github_app:77@github.com")
}

func TestGitHubAppFailedMintIsSingleFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mints atomic.Int64
		firstEntered := make(chan struct{})
		secondEntered := make(chan struct{})
		release := make(chan struct{})
		src := NewManagedSource(githubAppDescriptor(42), Options{
			GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
				switch mints.Add(1) {
				case 1:
					close(firstEntered)
				case 2:
					close(secondEntered)
				}
				<-release
				return "", time.Time{}, errors.New("rate limited")
			},
		})

		results := make(chan error, 2)
		for range 2 {
			go func() {
				_, err := src.Token(context.Background())
				results <- err
			}()
		}
		<-firstEntered
		synctest.Wait()
		select {
		case <-secondEntered:
			assert.Fail(t, "parallel callers minted the same App token independently")
		default:
		}
		close(release)
		for range 2 {
			require.ErrorContains(t, <-results, "rate limited")
		}
		assert.Equal(t, int64(1), mints.Load())
	})
}

func TestGitHubAppInvalidateDoesNotEvictInFlightMint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var mints atomic.Int64
		entered := make(chan struct{})
		release := make(chan struct{})
		src := NewManagedSource(githubAppDescriptor(42), Options{
			GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
				if mints.Add(1) == 1 {
					close(entered)
					<-release
				}
				return "ghs_fresh", time.Now().Add(time.Hour), nil
			},
		})

		type result struct {
			token string
			err   error
		}
		winner := make(chan result, 1)
		go func() {
			token, err := src.Token(context.Background())
			winner <- result{token: token, err: err}
		}()
		<-entered
		// A stale 401 for the previous token arrives while the replacement
		// mint is already in flight.
		src.Invalidate("ghs_stale")

		joiner := make(chan result, 1)
		go func() {
			token, err := src.Token(context.Background())
			joiner <- result{token: token, err: err}
		}()
		synctest.Wait()
		assert.Equal(int64(1), mints.Load(),
			"stale invalidation must not evict the in-flight mint into a parallel mint")
		close(release)
		for _, ch := range []chan result{winner, joiner} {
			got := <-ch
			require.NoError(got.err)
			assert.Equal("ghs_fresh", got.token)
		}
		assert.Equal(int64(1), mints.Load())
	})
}

func TestGitHubAppStaleUnauthorizedDoesNotEvictReplacementToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var mints atomic.Int64
	src := NewManagedSource(githubAppDescriptor(42), Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			mint := mints.Add(1)
			return fmt.Sprintf("token-%d", mint), time.Now().Add(time.Hour), nil
		},
	})
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var authMu sync.Mutex
	authByPath := make(map[string][]string)
	transport := platform.AuthTransport{
		Source: src,
		Base: platform.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
			authMu.Lock()
			authByPath[req.URL.Path] = append(
				authByPath[req.URL.Path], req.Header.Get("Authorization"),
			)
			attempt := len(authByPath[req.URL.Path])
			authMu.Unlock()

			status := http.StatusOK
			if attempt == 1 {
				status = http.StatusUnauthorized
				if req.URL.Path == "/first" {
					close(firstEntered)
					<-releaseFirst
				}
			}
			return &http.Response{
				StatusCode: status,
				Body:       http.NoBody,
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
		SetHeader:           platform.BearerAuthHeader,
		RetryOnUnauthorized: true,
	}

	firstReq, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, "https://api.example.test/first", nil,
	)
	require.NoError(err)
	firstResult := make(chan error, 1)
	go func() {
		_, err := transport.RoundTrip(firstReq)
		firstResult <- err
	}()
	<-firstEntered

	secondReq, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, "https://api.example.test/second", nil,
	)
	require.NoError(err)
	_, secondErr := transport.RoundTrip(secondReq)
	close(releaseFirst)
	firstErr := <-firstResult
	require.NoError(secondErr)
	require.NoError(firstErr)

	assert.Equal([]string{"Bearer token-1", "Bearer token-2"}, authByPath["/second"])
	assert.Equal([]string{"Bearer token-1", "Bearer token-2"}, authByPath["/first"])
	assert.Equal(int64(2), mints.Load(),
		"a stale 401 must not evict the replacement token")
}

func TestGitHubAppMintCancellationIsNotPublishedToWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var mints atomic.Int64
		winnerEntered := make(chan struct{})
		src := NewManagedSource(githubAppDescriptor(42), Options{
			GitHubApp: func(ctx context.Context, _ Candidate) (string, time.Time, error) {
				if mints.Add(1) == 1 {
					close(winnerEntered)
					<-ctx.Done()
					return "", time.Time{}, ctx.Err()
				}
				return "ghs_recovered", time.Now().Add(time.Hour), nil
			},
		})

		winnerCtx, cancel := context.WithCancel(context.Background())
		winnerErr := make(chan error, 1)
		go func() {
			_, err := src.Token(winnerCtx)
			winnerErr <- err
		}()
		<-winnerEntered
		type result struct {
			token string
			err   error
		}
		waiterResult := make(chan result, 1)
		go func() {
			token, err := src.Token(context.Background())
			waiterResult <- result{token: token, err: err}
		}()
		synctest.Wait()
		cancel()

		require.ErrorIs(<-winnerErr, context.Canceled)
		waiter := <-waiterResult
		require.NoError(waiter.err)
		assert.Equal("ghs_recovered", waiter.token)
		assert.Equal(int64(2), mints.Load(),
			"waiter must re-mint with its own context after the winner cancels")
	})
}

func TestGitHubAppMintCallerDeadlineFailureIsNotCached(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var mints atomic.Int64
	src := NewManagedSource(githubAppDescriptor(42), Options{
		GitHubApp: func(ctx context.Context, _ Candidate) (string, time.Time, error) {
			if mints.Add(1) == 1 {
				<-ctx.Done()
				return "", time.Time{}, ctx.Err()
			}
			return "ghs_recovered", time.Now().Add(time.Hour), nil
		},
	})

	deadlineCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := src.Token(deadlineCtx)
	require.ErrorIs(err, context.DeadlineExceeded)

	token, err := src.Token(context.Background())
	require.NoError(err)
	assert.Equal("ghs_recovered", token)
	assert.Equal(int64(2), mints.Load(),
		"caller-caused deadline failure must not enter the retry window")
}

func TestGitHubAppMintInternalDeadlineFailureIsCached(t *testing.T) {
	var mints atomic.Int64
	src := NewManagedSource(githubAppDescriptor(42), Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			if mints.Add(1) == 1 {
				return "", time.Time{}, fmt.Errorf("mint client timeout: %w", context.DeadlineExceeded)
			}
			return "ghs_recovered", time.Now().Add(time.Hour), nil
		},
	})

	for range 2 {
		_, err := src.Token(context.Background())
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	assert.Equal(t, int64(1), mints.Load(),
		"an internal client deadline must enter the retry window")
}

type retryDeadlineTestError struct {
	at time.Time
}

func (e retryDeadlineTestError) Error() string { return "rate limited" }

func (e retryDeadlineTestError) RetryDeadline(time.Time) time.Time { return e.at }

func TestGitHubAppFailedMintCachesBoundedRetryDeadline(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	tooLate := now.Add(24 * time.Hour)
	assert.Equal(now.Add(githubAppMintRetryMax),
		githubAppMintRetryDeadline(retryDeadlineTestError{at: tooLate}, nil, now))
	assert.Equal(now.Add(githubAppMintRetryDefault),
		githubAppMintRetryDeadline(retryDeadlineTestError{at: now}, nil, now))
	assert.True(githubAppMintRetryDeadline(context.Canceled, context.Canceled, now).IsZero())

	var mints atomic.Int64
	store := newGitHubAppTokenStore()
	store.now = func() time.Time { return now }
	src := newManagedSource(githubAppDescriptor(42), Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			if mints.Add(1) == 1 {
				return "", time.Time{}, retryDeadlineTestError{at: now.Add(time.Minute)}
			}
			return "ghs_recovered", now.Add(time.Hour), nil
		},
	}, store)

	_, err := src.Token(context.Background())
	require.ErrorContains(err, "rate limited")
	_, err = src.Token(context.Background())
	require.ErrorContains(err, "rate limited")
	assert.Equal(int64(1), mints.Load(), "retry window must suppress another mint")

	now = now.Add(time.Minute)
	token, err := src.Token(context.Background())
	require.NoError(err)
	assert.Equal("ghs_recovered", token)
	assert.Equal(int64(2), mints.Load())
}

func TestGitHubAppHeaderlessMintFailureCooldownIsSharedAcrossRoutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	var mints atomic.Int64
	set := NewSourceSet(Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			if mints.Add(1) == 1 {
				return "", time.Time{}, errors.New("upstream unavailable")
			}
			return "ghs_recovered", now.Add(time.Hour), nil
		},
	})
	set.appTokens.now = func() time.Time { return now }
	first := githubAppDescriptor(42)
	first.Key.Scope = "repo:kenn-io/one"
	second := githubAppDescriptor(42)
	second.Key.Scope = "repo:kenn-io/two"

	_, err := set.Upsert(first).Token(context.Background())
	require.ErrorContains(err, "upstream unavailable")
	_, err = set.Upsert(second).Token(context.Background())
	require.ErrorContains(err, "upstream unavailable")
	assert.Equal(int64(1), mints.Load(), "shared routes must retain the failure cooldown")

	now = now.Add(githubAppMintRetryDefault)
	token, err := set.Upsert(second).Token(context.Background())
	require.NoError(err)
	assert.Equal("ghs_recovered", token)
	assert.Equal(int64(2), mints.Load())
}

func TestGitHubAppInvalidatePreservesFailedMintCooldown(t *testing.T) {
	var mints atomic.Int64
	src := NewManagedSource(githubAppDescriptor(42), Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			mints.Add(1)
			return "", time.Time{}, errors.New("upstream unavailable")
		},
	})

	_, err := src.Token(context.Background())
	require.ErrorContains(t, err, "upstream unavailable")
	src.Invalidate("ghs_stale")
	_, err = src.Token(context.Background())
	require.ErrorContains(t, err, "upstream unavailable")
	assert.Equal(t, int64(1), mints.Load(),
		"stale invalidation must preserve an active failure cooldown")
}

func TestMutationAuthSkipsGitHubAppCandidate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("TEST_GITHUB_APP_FALLBACK", "user-pat")
	var mints atomic.Int64
	src := NewManagedSource(githubAppDescriptor(42), Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			mints.Add(1)
			return "ghs_minted", time.Now().Add(time.Hour), nil
		},
	})

	// Mutation-marked resolution must bypass the app and land on the
	// user's PAT so writes are attributed to the user.
	token, err := src.Token(WithMutationAuth(context.Background()))
	require.NoError(err)
	assert.Equal("user-pat", token)
	assert.Zero(mints.Load())

	// Unmarked resolution still mints the app token.
	token, err = src.Token(context.Background())
	require.NoError(err)
	assert.Equal("ghs_minted", token)
	assert.Equal(int64(1), mints.Load())
}

func TestGitHubAppDescriptorUpdateClearsCache(t *testing.T) {
	var mints atomic.Int64
	src := NewManagedSource(githubAppDescriptor(42), Options{
		GitHubApp: func(context.Context, Candidate) (string, time.Time, error) {
			mints.Add(1)
			return "ghs_minted", time.Now().Add(time.Hour), nil
		},
	})
	_, err := src.Token(context.Background())
	require.NoError(t, err)

	// Pointing the source at a different installation must drop the
	// cached token: it was scoped to the old installation's repos.
	src.Update(githubAppDescriptor(43))
	_, err = src.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), mints.Load())
}

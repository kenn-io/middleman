package github

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

const repositoryFeatureProbeInterval = 24 * time.Hour

type repositoryFeatureCooldownKey struct {
	platform       platform.Kind
	host           string
	providerRepoID string
	repoPath       string
	feature        string
}

type repositoryFeatureCooldowns struct {
	mu              sync.Mutex
	states          map[repositoryFeatureCooldownKey]repositoryFeatureCooldownState
	nextGeneration  uint64
	nextReservation uint64
}

type repositoryFeatureCooldownState struct {
	nextProbe   time.Time
	generation  uint64
	reservation uint64
	// providerRepoID identifies the repository that recorded the state;
	// empty when it was recorded from a route-only (unresolved) ref.
	providerRepoID string
}

type repositoryFeatureProbe struct {
	cooldowns *repositoryFeatureCooldowns
	// key is where the governing state lives; keys is the repo's full key
	// set, so clear can also drop entries recorded alongside it.
	key         repositoryFeatureCooldownKey
	keys        []repositoryFeatureCooldownKey
	generation  uint64
	reservation uint64
	bypass      bool
}

type repositoryFeatureCooldownBypassKey struct{}

type repositoryFeatureCooldownBypass struct {
	throughGeneration uint64
	repos             []RepoRef
}

func withRepositoryFeatureCooldownBypass(
	ctx context.Context,
	throughGeneration uint64,
) context.Context {
	return context.WithValue(ctx, repositoryFeatureCooldownBypassKey{}, repositoryFeatureCooldownBypass{
		throughGeneration: throughGeneration,
	})
}

func withRepositoryFeatureCooldownBypassForRepos(
	ctx context.Context,
	throughGeneration uint64,
	repos []RepoRef,
) context.Context {
	return context.WithValue(ctx, repositoryFeatureCooldownBypassKey{}, repositoryFeatureCooldownBypass{
		throughGeneration: throughGeneration,
		repos:             slices.Clone(repos),
	})
}

func repositoryFeatureCooldownBypassFromContext(
	ctx context.Context,
) (repositoryFeatureCooldownBypass, bool) {
	bypass, ok := ctx.Value(repositoryFeatureCooldownBypassKey{}).(repositoryFeatureCooldownBypass)
	return bypass, ok
}

func (b repositoryFeatureCooldownBypass) appliesTo(repo RepoRef) bool {
	return b.repos == nil || slices.ContainsFunc(b.repos, func(candidate RepoRef) bool {
		return sameRepoIntent(repo, candidate)
	})
}

// repositoryFeatureKeys returns the cooldown keys for a repository. The
// provider-ID key comes first: it survives renames and keeps a replacement
// repository on a reused route from inheriting the displaced repository's
// cooldown. States are recorded under every returned key so refs that
// predate identity resolution (route-only) still observe them.
func repositoryFeatureKeys(repo RepoRef, feature string) []repositoryFeatureCooldownKey {
	if repoPlatform(repo) == platform.KindGitHub {
		repo = canonicalRepoRef(repo)
		if repo.Owner != "" && repo.Name != "" {
			repo.RepoPath = repo.Owner + "/" + repo.Name
		}
	}
	ref := platformRepoRef(repo)
	routeKey := repositoryFeatureCooldownKey{
		platform: ref.Platform,
		host:     ref.Host,
		repoPath: ref.RepoPath,
		feature:  feature,
	}
	if ref.PlatformExternalID == "" {
		return []repositoryFeatureCooldownKey{routeKey}
	}
	idKey := repositoryFeatureCooldownKey{
		platform:       ref.Platform,
		host:           ref.Host,
		providerRepoID: ref.PlatformExternalID,
		feature:        feature,
	}
	return []repositoryFeatureCooldownKey{idKey, routeKey}
}

// lookupState returns the state governing a probe and the key it lives at.
// The provider-ID key wins; a route-key state applies only when its recorder
// is unknown (route-only record) or is this same repository — a different
// recorder means a displaced repository's cooldown that must not transfer.
func (c *repositoryFeatureCooldowns) lookupState(
	keys []repositoryFeatureCooldownKey,
) (repositoryFeatureCooldownKey, repositoryFeatureCooldownState, bool) {
	primary := keys[0]
	if state, ok := c.states[primary]; ok {
		return primary, state, true
	}
	for _, key := range keys[1:] {
		state, ok := c.states[key]
		if !ok {
			continue
		}
		if state.providerRepoID == "" || state.providerRepoID == primary.providerRepoID {
			return key, state, true
		}
	}
	return primary, repositoryFeatureCooldownState{}, false
}

func (c *repositoryFeatureCooldowns) beginProbeWithRetry(
	repo RepoRef,
	feature string,
	now time.Time,
	bypass repositoryFeatureCooldownBypass,
	bypassEnabled bool,
) (repositoryFeatureProbe, bool, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := repositoryFeatureKeys(repo, feature)
	key, state, ok := c.lookupState(keys)
	if bypassEnabled && (!ok || state.generation <= bypass.throughGeneration) {
		return repositoryFeatureProbe{
			cooldowns:  c,
			key:        key,
			keys:       keys,
			generation: state.generation,
			bypass:     true,
		}, true, time.Time{}
	}
	if !ok {
		return repositoryFeatureProbe{cooldowns: c, key: key, keys: keys}, true, time.Time{}
	}
	if state.reservation != 0 {
		return repositoryFeatureProbe{}, false, now.Add(time.Second)
	}
	if state.nextProbe.After(now) {
		return repositoryFeatureProbe{}, false, state.nextProbe
	}
	c.nextReservation++
	state.reservation = c.nextReservation
	c.states[key] = state
	return repositoryFeatureProbe{
		cooldowns:   c,
		key:         key,
		keys:        keys,
		generation:  state.generation,
		reservation: state.reservation,
	}, true, time.Time{}
}

func (c *repositoryFeatureCooldowns) beginProbe(
	repo RepoRef,
	feature string,
	now time.Time,
	bypass repositoryFeatureCooldownBypass,
	bypassEnabled bool,
) (repositoryFeatureProbe, bool) {
	probe, due, _ := c.beginProbeWithRetry(repo, feature, now, bypass, bypassEnabled)
	return probe, due
}

func (c *repositoryFeatureCooldowns) currentGeneration() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nextGeneration
}

func (c *repositoryFeatureCooldowns) deferUntil(repo RepoRef, feature string, nextProbe time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.states == nil {
		c.states = make(map[repositoryFeatureCooldownKey]repositoryFeatureCooldownState)
	}
	c.nextGeneration++
	keys := repositoryFeatureKeys(repo, feature)
	state := repositoryFeatureCooldownState{
		nextProbe:      nextProbe,
		generation:     c.nextGeneration,
		providerRepoID: keys[0].providerRepoID,
	}
	for _, key := range keys {
		c.states[key] = state
	}
}

func (probe repositoryFeatureProbe) release() {
	if probe.bypass {
		probe.clear()
		return
	}
	if probe.cooldowns == nil || probe.reservation == 0 {
		return
	}
	probe.cooldowns.mu.Lock()
	defer probe.cooldowns.mu.Unlock()
	state, ok := probe.cooldowns.states[probe.key]
	if ok && state.generation == probe.generation && state.reservation == probe.reservation {
		delete(probe.cooldowns.states, probe.key)
	}
}

func (probe repositoryFeatureProbe) abandon() {
	if probe.cooldowns == nil || probe.reservation == 0 {
		return
	}
	probe.cooldowns.mu.Lock()
	defer probe.cooldowns.mu.Unlock()
	state, ok := probe.cooldowns.states[probe.key]
	if !ok || state.generation != probe.generation || state.reservation != probe.reservation {
		return
	}
	state.reservation = 0
	probe.cooldowns.states[probe.key] = state
}

func (probe repositoryFeatureProbe) clear() {
	if probe.cooldowns == nil {
		return
	}
	probe.cooldowns.mu.Lock()
	defer probe.cooldowns.mu.Unlock()
	state, ok := probe.cooldowns.states[probe.key]
	if !ok || state.generation != probe.generation {
		return
	}
	if !probe.bypass && probe.reservation != 0 && state.reservation != probe.reservation {
		return
	}
	delete(probe.cooldowns.states, probe.key)
	// Entries recorded alongside this state (deferUntil writes every key
	// with one generation) must clear together, or the repo stays cooled
	// under its other key after a successful probe.
	for _, key := range probe.keys {
		if key == probe.key {
			continue
		}
		if sibling, ok := probe.cooldowns.states[key]; ok && sibling.generation == probe.generation {
			delete(probe.cooldowns.states, key)
		}
	}
}

func (s *Syncer) beginRepositoryFeatureProbe(
	ctx context.Context,
	repo RepoRef,
	feature string,
) (repositoryFeatureProbe, bool) {
	bypass, bypassEnabled := repositoryFeatureCooldownBypassFromContext(ctx)
	bypassEnabled = bypassEnabled && bypass.appliesTo(repo)
	return s.featureCooldowns.beginProbe(
		repo, feature, s.now().UTC(), bypass, bypassEnabled,
	)
}

func (s *Syncer) beginRepositoryFeatureProbeWithRetry(
	ctx context.Context,
	repo RepoRef,
	feature string,
) (repositoryFeatureProbe, bool, time.Time) {
	bypass, bypassEnabled := repositoryFeatureCooldownBypassFromContext(ctx)
	bypassEnabled = bypassEnabled && bypass.appliesTo(repo)
	return s.featureCooldowns.beginProbeWithRetry(
		repo, feature, s.now().UTC(), bypass, bypassEnabled,
	)
}

func (s *Syncer) recordRepositoryFeatureDisabled(repo RepoRef, feature string, err error) bool {
	_, recorded := s.recordRepositoryFeatureDisabledUntil(repo, feature, err)
	return recorded
}

func (s *Syncer) recordRepositoryFeatureDisabledUntil(
	repo RepoRef,
	feature string,
	err error,
) (time.Time, bool) {
	var platformErr *platform.Error
	if !errors.As(err, &platformErr) ||
		platformErr.Code != platform.ErrCodeRepositoryFeatureDisabled ||
		platformErr.Capability != feature {
		return time.Time{}, false
	}
	nextProbe := s.now().UTC().Add(repositoryFeatureProbeInterval)
	s.featureCooldowns.deferUntil(repo, feature, nextProbe)
	slog.Info("repository feature disabled; deferring background sync",
		"platform", repoPlatform(repo),
		"host", repoHost(repo),
		"repo", platformRepoRef(repo).RepoPath,
		"feature", feature,
		"next_probe_at", nextProbe,
	)
	return nextProbe, true
}

func (s *Syncer) recordGitHubRepositoryFeatureDisabled(
	repo RepoRef,
	feature string,
	err error,
) bool {
	classified := repositoryFeatureDisabledError(repo, feature, err)
	if classified == nil {
		return false
	}
	return s.recordRepositoryFeatureDisabled(repo, feature, classified)
}

func repositoryFeatureDisabledError(repo RepoRef, feature string, err error) error {
	if errors.Is(err, platform.ErrRepositoryFeatureDisabled) {
		return err
	}
	if repoPlatform(repo) != platform.KindGitHub {
		return nil
	}
	return platformgithub.RepositoryFeatureDisabled(repoHost(repo), feature, err)
}

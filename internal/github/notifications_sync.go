package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/internal/archive"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

const (
	defaultNotificationPropagationMaxAttempts = 10
	notificationSyncSinceOverlap              = 5 * time.Minute
	notificationFullSyncInterval              = time.Hour
)

// notificationAckWorstCaseRequests is the number of user-credential requests
// one queued acknowledgement can spend: the pre-ack refetch, the mark-read,
// and the post-ack reconciliation refetch.
const notificationAckWorstCaseRequests = 3

type NotificationSyncStatus struct {
	Running        bool
	LastStartedAt  time.Time
	LastFinishedAt time.Time
	LastError      string
}

type notificationThreadGetter interface {
	GetNotificationThread(context.Context, string) (NotificationThread, error)
}

type routedNotificationThreadGetter interface {
	GetNotificationThreadForRepo(
		context.Context, string, string, string,
	) (NotificationThread, error)
}

type routedNotificationReadMarker interface {
	MarkNotificationThreadReadForRepo(
		context.Context, string, string, string,
	) error
}

type notificationReadRateReserveBypasser interface {
	InstallationAuthenticationActive() bool
}

func notificationBypassesReadRateReserve(client notificationClient) bool {
	if bypasser, ok := client.(notificationReadRateReserveBypasser); ok {
		return bypasser.InstallationAuthenticationActive()
	}
	if legacy, ok := client.(interface{ GitHubClient() platformgithub.API }); ok {
		if inner := legacy.GitHubClient(); inner != nil {
			if bypasser, ok := inner.(notificationReadRateReserveBypasser); ok {
				return bypasser.InstallationAuthenticationActive()
			}
		}
	}
	return false
}

// notificationThreadGetterFor resolves the optional thread-refetch
// capability used by the reopen-on-remote-activity check. The GitHub
// provider exposes it on its inner REST client; other providers may
// implement it directly once their notification support lands.
func notificationThreadGetterFor(client notificationClient) (notificationThreadGetter, bool) {
	if getter, ok := client.(notificationThreadGetter); ok {
		return getter, true
	}
	if legacy, ok := client.(interface{ GitHubClient() platformgithub.API }); ok {
		if inner := legacy.GitHubClient(); inner != nil {
			if getter, ok := inner.(notificationThreadGetter); ok {
				return getter, true
			}
		}
	}
	return nil, false
}

func (s *Syncer) RunNotificationSync(ctx context.Context) error {
	if !s.SyncEnabled() {
		return platform.ErrSyncDisabled
	}
	if !s.BeginNotificationSync() {
		return nil
	}
	err := s.SyncNotifications(ctx)
	s.FinishNotificationSync(err)
	// Nudge listeners (the SSE hub) even on a partial error: a run that
	// errored on one host can still have inserted rows for another, and the
	// reload it triggers is idempotent.
	if s.onNotificationSyncComplete != nil {
		s.onNotificationSyncComplete()
	}
	return err
}

func (s *Syncer) BeginNotificationSync() bool {
	s.notificationSyncMu.Lock()
	defer s.notificationSyncMu.Unlock()
	if s.notificationSync.Running {
		return false
	}
	s.notificationSync.Running = true
	s.notificationSync.LastStartedAt = time.Now().UTC()
	s.notificationSync.LastError = ""
	return true
}

func (s *Syncer) FinishNotificationSync(err error) {
	s.notificationSyncMu.Lock()
	defer s.notificationSyncMu.Unlock()
	s.notificationSync.Running = false
	s.notificationSync.LastFinishedAt = time.Now().UTC()
	if err != nil {
		s.notificationSync.LastError = err.Error()
	}
}

func (s *Syncer) NotificationSyncStatus() NotificationSyncStatus {
	s.notificationSyncMu.RLock()
	defer s.notificationSyncMu.RUnlock()
	return s.notificationSync
}

func (s *Syncer) SyncNotifications(ctx context.Context) error {
	ctx = WithSyncBudget(ctx)
	repos := excludeArchivedRepos(s.TrackedRepos())
	tracked := make(map[string]RepoRef, len(repos))
	for _, repo := range repos {
		platformName := string(repoPlatform(repo))
		host := normalizedPlatformHost(repo.PlatformHost)
		trackedRepo := RepoRef{
			Platform:     repoPlatform(repo),
			Owner:        strings.ToLower(repo.Owner),
			Name:         strings.ToLower(repo.Name),
			PlatformHost: host,
		}
		dbRepo, err := s.db.GetRepoByIdentity(ctx, db.RepoIdentity{
			Platform:     platformName,
			PlatformHost: host,
			Owner:        repo.Owner,
			Name:         repo.Name,
			RepoPath:     repo.RepoPath,
		})
		if err != nil {
			return fmt.Errorf("load notification repo identity for %s/%s on %s/%s: %w", repo.Owner, repo.Name, platformName, host, err)
		}
		if dbRepo != nil {
			trackedRepo.RepoID = dbRepo.ID
		}
		tracked[notificationRepoKey(platformName, host, repo.Owner, repo.Name)] = trackedRepo
	}
	clients := s.notificationClients()
	var errs []error
	for _, entry := range clients {
		providerWork := s.beginNotificationProviderWork(
			entry.platform, entry.host, tracked,
		)
		err := s.syncNotificationsForHost(
			ctx, entry.platform, entry.host, entry.client, tracked, providerWork,
		)
		providerWork.release()
		if err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.db.MarkClosedLinkedNotificationsDone(ctx, time.Now().UTC()); err != nil {
		errs = append(errs, fmt.Errorf("mark closed linked notifications done: %w", err))
	}
	return errors.Join(errs...)
}

func (s *Syncer) beginNotificationProviderWork(
	kind platform.Kind,
	host string,
	tracked map[string]RepoRef,
) *notificationProviderWork {
	work := &notificationProviderWork{
		syncer: s,
		seen:   make(map[string]struct{}),
	}
	// A repository whose credential route cannot resolve registers nothing:
	// its own wire calls fail closed at routing without upstream I/O and its
	// per-repository sync reports the error. Aborting the whole host here
	// would block healthy siblings from syncing and advancing their
	// watermarks — the exact coupling per-repository watermarks remove.
	for _, repo := range notificationTrackedRepos(string(kind), host, tracked) {
		work.addRepo(repo)
	}
	return work
}

type notificationProviderWork struct {
	syncer   *Syncer
	seen     map[string]struct{}
	releases []func()
}

func (w *notificationProviderWork) addRepo(repo RepoRef) {
	identityRoutes := []bool{false}
	if repoPlatform(repo) == platform.KindGitHub {
		identityRoutes = append(identityRoutes, true)
	}
	for _, useWriteIdentity := range identityRoutes {
		bucket, err := w.syncer.bucketKeyForRepo(repo, useWriteIdentity)
		if err != nil {
			continue
		}
		if _, ok := w.seen[bucket]; ok {
			continue
		}
		w.seen[bucket] = struct{}{}
		w.releases = append(w.releases, w.syncer.beginProviderWork(
			bucket, archive.PriorityNotificationRefresh,
		))
	}
}

func (w *notificationProviderWork) release() {
	for _, v := range slices.Backward(w.releases) {
		v()
	}
}

// notificationClient is the provider surface the notification sync
// engine needs: list threads and propagate read acks. Providers gate
// support through Capabilities().ReadNotifications and
// NotificationMutation; non-supporting providers ship stubs that
// return unsupported_capability errors until filled in.
type notificationClient interface {
	platform.NotificationReader
	platform.NotificationMutator
}

type notificationHostClient struct {
	platform platform.Kind
	host     string
	client   notificationClient
}

func (s *Syncer) notificationClients() []notificationHostClient {
	providers := s.clients.Providers()
	clients := make([]notificationHostClient, 0, len(providers))
	for _, provider := range providers {
		caps := provider.Capabilities()
		if !caps.ReadNotifications || !caps.NotificationMutation {
			continue
		}
		client, ok := provider.(notificationClient)
		if !ok {
			continue
		}
		clients = append(clients, notificationHostClient{platform: provider.Platform(), host: normalizedPlatformHost(provider.Host()), client: client})
	}
	sort.Slice(clients, func(i, j int) bool {
		if clients[i].platform != clients[j].platform {
			return clients[i].platform < clients[j].platform
		}
		return clients[i].host < clients[j].host
	})
	return clients
}

func (s *Syncer) notificationClientForHost(kind platform.Kind, host string) (notificationClient, bool) {
	provider, err := s.clients.Provider(kind, normalizedPlatformHost(host))
	if err != nil {
		return nil, false
	}
	caps := provider.Capabilities()
	if !caps.ReadNotifications || !caps.NotificationMutation {
		return nil, false
	}
	client, ok := provider.(notificationClient)
	if !ok {
		return nil, false
	}
	return client, true
}

func (s *Syncer) syncNotificationsForHost(
	ctx context.Context,
	kind platform.Kind,
	host string,
	client notificationClient,
	tracked map[string]RepoRef,
	providerWork *notificationProviderWork,
) error {
	startedAt := time.Now().UTC()
	trackedRepos := notificationTrackedRepos(string(kind), host, tracked)
	if len(trackedRepos) == 0 {
		return nil
	}
	// Watermarks are per repository identity: a repository whose credential
	// route is unavailable or exhausted reports its error without holding
	// back watermark advancement for the healthy repositories on the host.
	var repoErrs []error
	for _, repo := range trackedRepos {
		if err := s.syncNotificationsForRepo(
			ctx, kind, host, client, tracked, repo, startedAt, providerWork,
		); err != nil {
			repoErrs = append(repoErrs, err)
		}
	}
	return errors.Join(repoErrs...)
}

func (s *Syncer) syncNotificationsForRepo(
	ctx context.Context,
	kind platform.Kind,
	host string,
	client notificationClient,
	tracked map[string]RepoRef,
	repo RepoRef,
	startedAt time.Time,
	providerWork *notificationProviderWork,
) error {
	for range 2 {
		retry, err := s.syncNotificationsForRepoAttempt(
			ctx, kind, host, client, tracked, repo, startedAt, providerWork,
		)
		if err != nil || !retry {
			return err
		}
	}
	return fmt.Errorf(
		"repository route changed repeatedly during notification sync of %s/%s on %s",
		repo.Owner, repo.Name, host,
	)
}

func (s *Syncer) syncNotificationsForRepoAttempt(
	ctx context.Context,
	kind platform.Kind,
	host string,
	client notificationClient,
	tracked map[string]RepoRef,
	repo RepoRef,
	startedAt time.Time,
	providerWork *notificationProviderWork,
) (bool, error) {
	platformName := string(kind)
	if err := s.ensureNotificationIdentityBudget(repo, client); err != nil {
		return false, err
	}
	resolved, observedRepoID, providerRepo, observedAt, accepted, err :=
		s.reconcileRepoIdentityObservation(ctx, repo)
	if err != nil {
		return false, fmt.Errorf(
			"verify repository identity before notification sync of %s/%s on %s: %w",
			repo.Owner, repo.Name, host, err,
		)
	}
	if !accepted {
		return true, nil
	}
	repo = resolved
	observedIdentity := platformdb.DBRepoIdentity(platformRepoRef(repo))
	routeFence, found, err := s.db.CurrentRepositoryRouteFence(
		ctx, observedIdentity, observedRepoID,
	)
	if err != nil {
		return false, fmt.Errorf(
			"capture repository route before notification sync of %s/%s on %s: %w",
			repo.Owner, repo.Name, host, err,
		)
	}
	if !found {
		return true, nil
	}
	providerWork.addRepo(repo)
	if s.afterNotificationRepoIdentityReconciled != nil {
		s.afterNotificationRepoIdentityReconciled()
	}
	if providerRepo != nil {
		applied, err := s.updateRepoSettingsFromProviderObservation(
			s.db.WithRepositoryRouteFence(ctx, observedIdentity, routeFence),
			observedRepoID, observedAt, *providerRepo,
		)
		if err != nil {
			if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
				return true, nil
			}
			return false, fmt.Errorf(
				"persist repository settings before notification sync of %s/%s on %s: %w",
				repo.Owner, repo.Name, host, err,
			)
		}
		if !applied {
			return true, nil
		}
	}
	if err := s.ensureNotificationBudget(repo, client, 1); err != nil {
		return false, err
	}
	trackedRepo := repo
	trackedRepo.RepoID = observedRepoID
	tracked[notificationRepoKey(platformName, host, repo.Owner, repo.Name)] = trackedRepo
	watermark, err := s.db.GetNotificationSyncWatermark(
		ctx, platformName, host, repo.Owner, repo.Name,
	)
	if err != nil {
		return false, fmt.Errorf(
			"load notification sync watermark for %s/%s on %s: %w",
			repo.Owner, repo.Name, host, err,
		)
	}
	var since *time.Time
	fullSync := shouldFullSyncNotifications(startedAt, watermark)
	if watermark != nil && !fullSync {
		value := watermark.LastSuccessfulSyncAt.Add(-notificationSyncSinceOverlap).UTC()
		since = &value
	}
	participatingIDs, err := s.listParticipatingNotificationIDs(
		ctx, host, client, []RepoRef{repo}, since,
	)
	if err != nil {
		return false, err
	}
	for page := 1; ; page++ {
		if err := s.ensureNotificationBudget(repo, client, 1); err != nil {
			return false, err
		}
		threads, hasNext, err := client.ListNotifications(ctx, NotificationListOptions{
			All:       true,
			Since:     since,
			Page:      page,
			RepoOwner: repo.Owner,
			RepoName:  repo.Name,
		})
		if err != nil {
			return false, fmt.Errorf(
				"list notifications for %s/%s on %s page %d: %w",
				repo.Owner, repo.Name, host, page, err,
			)
		}
		notifications := make([]db.Notification, 0, len(threads))
		now := time.Now().UTC()
		for _, thread := range threads {
			if thread.RepoOwner == "" {
				thread.RepoOwner = repo.Owner
			}
			if thread.RepoName == "" {
				thread.RepoName = repo.Name
			}
			if participatingIDs[thread.ID] {
				thread.Participating = true
			}
			key := notificationRepoKey(platformName, host, thread.RepoOwner, thread.RepoName)
			trackedRepo, ok := tracked[key]
			if !ok {
				continue
			}
			trackedRepo.RepoID = observedRepoID
			// Only notifications anchored to a PR or issue have an in-app
			// destination and meaningful triage. CI/check-suite, discussion,
			// release, and other subjects are worthless in kenn-forge, so do
			// not persist them.
			if (thread.ItemType != "pr" && thread.ItemType != "issue") || thread.ItemNumber == nil {
				continue
			}
			// "author" notifications fire for any activity on a thread the
			// user opened ("Your thread"); the triggering comment/review/state
			// change is already its own row in the feed, so they are pure
			// duplication. Drop them while keeping comment, subscribed, and
			// the attention-requesting reasons (mention, review_requested, ...).
			if thread.Reason == "author" {
				continue
			}
			notification, err := s.notificationToDB(ctx, host, trackedRepo, thread, now)
			if err != nil {
				return false, fmt.Errorf(
					"normalize notification %s for %s/%s on %s page %d: %w",
					thread.ID, repo.Owner, repo.Name, host, page, err,
				)
			}
			notifications = append(notifications, notification)
		}
		committed, err := s.db.UpsertNotificationsIfRouteFence(
			ctx, notifications, observedIdentity, routeFence,
		)
		if err != nil {
			return false, fmt.Errorf("upsert notifications for %s/%s on %s page %d: %w", repo.Owner, repo.Name, host, page, err)
		}
		if !committed {
			return true, nil
		}
		if !hasNext {
			break
		}
	}
	lastFullSyncAt := watermarkLastFullSyncAt(watermark, startedAt, fullSync)
	committed, err := s.db.UpdateNotificationSyncWatermarkIfRouteFence(
		ctx, platformName, host, repo.Owner, repo.Name,
		routeFence, startedAt, lastFullSyncAt,
	)
	if err != nil {
		return false, fmt.Errorf(
			"store notification sync watermark for %s/%s on %s: %w",
			repo.Owner, repo.Name, host, err,
		)
	}
	return !committed, nil
}

func (s *Syncer) listParticipatingNotificationIDs(
	ctx context.Context,
	host string,
	client notificationClient,
	trackedRepos []RepoRef,
	since *time.Time,
) (map[string]bool, error) {
	participating := map[string]bool{}
	for _, repo := range trackedRepos {
		for page := 1; ; page++ {
			if err := s.ensureNotificationBudget(repo, client, 1); err != nil {
				return nil, err
			}
			threads, hasNext, err := client.ListNotifications(ctx, NotificationListOptions{
				All:           true,
				Participating: true,
				Since:         since,
				Page:          page,
				RepoOwner:     repo.Owner,
				RepoName:      repo.Name,
			})
			if err != nil {
				return nil, fmt.Errorf("list participating notifications for %s/%s on %s page %d: %w", repo.Owner, repo.Name, host, page, err)
			}
			for _, thread := range threads {
				if thread.ID != "" {
					participating[thread.ID] = true
				}
			}
			if !hasNext {
				break
			}
		}
	}
	return participating, nil
}

// ensureNotificationBudget checks both ceilings for an operation that can spend
// up to cost requests against the local hourly ceiling, which is the hard
// guard and keeps a real per-operation cost. The provider reserve beside it is
// the shared cadence-cached check and does not vary by caller. Callers pass
// the operation's worst case, not one request, so a multi-request
// acknowledgement cannot start with only enough headroom for its first call and
// cross a ceiling partway through.
func (s *Syncer) ensureNotificationBudget(
	repo RepoRef, client notificationClient, cost int,
) error {
	return s.ensureNotificationBudgetForIdentity(
		repo, client, cost, repoPlatform(repo) == platform.KindGitHub,
	)
}

// ensureNotificationIdentityBudget admits the repository read and, when split
// authentication adds a permissions overlay, the distinct write credential.
// Matching identities are checked once because GetRepository then spends one
// request from one REST pool.
func (s *Syncer) ensureNotificationIdentityBudget(
	repo RepoRef, client notificationClient,
) error {
	readIdentity, err := s.identityForRepo(repo, false)
	if err != nil {
		return err
	}
	if err := s.ensureNotificationBudgetForIdentity(
		repo, client, 1, false,
	); err != nil {
		return err
	}
	if repoPlatform(repo) != platform.KindGitHub {
		return nil
	}
	writeIdentity, err := s.identityForRepo(repo, true)
	if err != nil {
		return fmt.Errorf(
			"notification sync paused for %s: no startup-resolved write identity",
			repoHost(repo),
		)
	}
	if writeIdentity == readIdentity {
		return nil
	}
	return s.ensureNotificationBudgetForIdentity(repo, client, 1, true)
}

func (s *Syncer) ensureNotificationBudgetForIdentity(
	repo RepoRef,
	client notificationClient,
	cost int,
	writeIdentity bool,
) error {
	host := repoHost(repo)
	if writeIdentity && s.routers[host] != nil {
		if _, ok := s.WriteIdentityForRepo(repo); !ok {
			return fmt.Errorf(
				"notification sync paused for %s: no startup-resolved write identity",
				host,
			)
		}
	}
	bucket, err := s.bucketKeyForRepo(repo, writeIdentity)
	if err != nil {
		return err
	}
	if budget := s.budgets[bucket]; budget != nil && !budget.CanSpend(cost) {
		return fmt.Errorf("notification sync paused for %s: sync budget exhausted", host)
	}
	// Notification reads and acknowledgement propagation resolve to the write
	// identity, so they gate on that credential's REST pool even when
	// repository reads run on an App installation token and therefore bypass
	// the shared read tracker. Without this, split-auth hosts would let
	// background notification work spend the user credential below the reserve
	// held for foreground mutations.
	// Notifications resolve to the write identity, so they gate on that
	// credential -- through the same cadence-cached reserve check every other
	// background path uses.
	if s.backgroundReserveExhausted(repo, QuotaResourceREST, writeIdentity) {
		identityKind := "user"
		if !writeIdentity {
			identity, identityErr := s.identityForRepo(repo, false)
			if identityErr == nil && strings.HasPrefix(
				identity.Principal, "installation:",
			) {
				identityKind = "read"
			}
		}
		return fmt.Errorf(
			"notification sync paused for %s: %s rate reserve exhausted",
			host, identityKind,
		)
	}
	bypassReserve := notificationBypassesReadRateReserve(client)
	if s.routers[host] != nil {
		bypassReserve = false
	}
	if bypassReserve {
		return nil
	}
	trackers := s.rateTrackers
	if writeIdentity && s.writeRateTrackers[bucket] != nil {
		trackers = s.writeRateTrackers
	}
	if rateTracker := trackers[bucket]; rateTracker != nil && rateTracker.IsPaused() {
		return fmt.Errorf("notification sync paused for %s: rate reserve exhausted", host)
	}
	return nil
}

func shouldFullSyncNotifications(now time.Time, watermark *db.NotificationSyncWatermark) bool {
	if watermark == nil || watermark.LastFullSyncAt == nil {
		return true
	}
	return !watermark.LastFullSyncAt.Add(notificationFullSyncInterval).After(now)
}

func watermarkLastFullSyncAt(watermark *db.NotificationSyncWatermark, startedAt time.Time, fullSync bool) *time.Time {
	if fullSync {
		value := startedAt.UTC()
		return &value
	}
	if watermark == nil || watermark.LastFullSyncAt == nil {
		return nil
	}
	value := watermark.LastFullSyncAt.UTC()
	return &value
}

func notificationTrackedRepos(platformName, host string, tracked map[string]RepoRef) []RepoRef {
	prefix := platformName + "/" + normalizedPlatformHost(host) + "/"
	keys := make([]string, 0, len(tracked))
	for key := range tracked {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	repos := make([]RepoRef, 0, len(keys))
	for _, key := range keys {
		repos = append(repos, tracked[key])
	}
	return repos
}

func notificationRepoKey(platformName, host, owner, name string) string {
	return strings.ToLower(platformName) + "/" + normalizedPlatformHost(host) + "/" + strings.ToLower(owner) + "/" + strings.ToLower(name)
}

func (s *Syncer) notificationToDB(ctx context.Context, host string, repo RepoRef, thread NotificationThread, syncedAt time.Time) (db.Notification, error) {
	notification := notificationToDB(host, repo, thread, syncedAt)
	if notification.ItemAuthor != "" || notification.ItemNumber == nil {
		return notification, nil
	}
	if repo.RepoID == 0 {
		return notification, nil
	}
	switch notification.ItemType {
	case "pr":
		mr, err := s.db.GetMergeRequestByRepoIDAndNumber(ctx, repo.RepoID, *notification.ItemNumber)
		if err != nil || mr == nil {
			return notification, err
		}
		notification.ItemAuthor = mr.Author
	case "issue":
		issue, err := s.db.GetIssueByRepoIDAndNumber(ctx, repo.RepoID, *notification.ItemNumber)
		if err != nil || issue == nil {
			return notification, err
		}
		notification.ItemAuthor = issue.Author
	}
	return notification, nil
}

func notificationToDB(host string, repo RepoRef, thread NotificationThread, syncedAt time.Time) db.Notification {
	var repoID *int64
	if repo.RepoID != 0 {
		repoID = &repo.RepoID
	}
	return db.Notification{
		Platform:                 string(repoPlatform(repo)),
		PlatformHost:             normalizedPlatformHost(host),
		PlatformNotificationID:   thread.ID,
		RepoID:                   repoID,
		RepoOwner:                strings.ToLower(repo.Owner),
		RepoName:                 strings.ToLower(repo.Name),
		SubjectType:              thread.SubjectType,
		SubjectTitle:             thread.SubjectTitle,
		SubjectURL:               thread.SubjectURL,
		SubjectLatestCommentURL:  thread.SubjectLatestCommentURL,
		WebURL:                   thread.WebURL,
		ItemNumber:               thread.ItemNumber,
		ItemType:                 thread.ItemType,
		ItemAuthor:               thread.ItemAuthor,
		Reason:                   thread.Reason,
		Unread:                   thread.Unread,
		Participating:            thread.Participating,
		SourceUpdatedAt:          thread.UpdatedAt,
		SourceLastAcknowledgedAt: thread.LastReadAt,
		SyncedAt:                 syncedAt,
	}
}

func (s *Syncer) ProcessQueuedNotificationReads(ctx context.Context, kind platform.Kind, host string, batchSize int) error {
	ctx = WithSyncBudget(ctx)
	if batchSize <= 0 {
		batchSize = 25
	}
	host = normalizedPlatformHost(host)
	client, ok := s.notificationClientForHost(kind, host)
	if !ok {
		return fmt.Errorf("%s notification client for host %s not configured", kind, host)
	}
	queued, err := s.db.ListQueuedNotificationAcks(ctx, string(kind), host, batchSize, time.Now().UTC())
	if err != nil {
		return err
	}
	// Read propagation makes refetch and mark-read wire requests, so it holds
	// notification-priority provider work for each queued repository's
	// effective identity; otherwise an admitted archive on the same principal
	// could overlap these requests. Rows whose repository no longer resolves
	// to a route register nothing: their wire calls fail closed at routing
	// without upstream I/O.
	bucketRepos, ackBuckets := s.ackRepoBuckets(kind, host, queued)
	seenBuckets := make(map[string]struct{}, len(bucketRepos))
	for _, notification := range queued {
		bucket, ok := ackBuckets[notification.ID]
		if !ok {
			continue
		}
		if _, seen := seenBuckets[bucket]; seen {
			continue
		}
		seenBuckets[bucket] = struct{}{}
		defer s.beginProviderWork(bucket, archive.PriorityNotificationRefresh)()
	}
	// A rate limit stops only the credential that hit it. Its remaining rows
	// are already deferred in the database, so skipping them here avoids
	// re-spending an exhausted budget while repositories served by other
	// credentials on this host keep propagating.
	exhausted := make(map[string]struct{}, len(seenBuckets))
	var rateLimitErr error
	for _, notification := range queued {
		bucket := ackBuckets[notification.ID]
		if _, stop := exhausted[bucket]; stop {
			continue
		}
		// Each queued ack spends a refetch, the mark-read, and a
		// reconciliation refetch on this row's credential. Stop before
		// crossing that credential's reserve instead of discovering it as a
		// per-row rate-limit error partway through the acknowledgement.
		//
		// Exhausting one credential's headroom stops only that bucket. Other
		// credentials on this host keep propagating, matching how an actual
		// rate-limit response is handled below.
		if err := s.ensureNotificationBudget(RepoRef{
			Platform: kind, PlatformHost: host,
			Owner: notification.RepoOwner, Name: notification.RepoName,
		}, client, notificationAckWorstCaseRequests); err != nil {
			exhausted[bucket] = struct{}{}
			if rateLimitErr == nil {
				rateLimitErr = err
			}
			continue
		}
		current, err := s.db.NotificationAckPropagationCurrent(ctx, notification.ID, notification.SourceAckQueuedAt, notification.SourceUpdatedAt)
		if err != nil {
			return err
		}
		if !current {
			continue
		}
		identity := db.RepoIdentity{
			Platform: notification.Platform, PlatformHost: host,
			Owner: notification.RepoOwner, Name: notification.RepoName,
		}
		var routeFence db.RepositoryRouteFence
		var routeFound bool
		if notification.RepoID == nil {
			routeFence, routeFound, err = s.db.ResolveCurrentRepositoryRouteFence(
				ctx, identity,
			)
		} else {
			routeFence, routeFound, err = s.db.CurrentRepositoryRouteFence(
				ctx, identity, *notification.RepoID,
			)
		}
		if err != nil {
			return fmt.Errorf(
				"capture repository route before refreshing notification %s: %w",
				notification.PlatformNotificationID, err,
			)
		}
		if !routeFound {
			if err := s.reopenLegacyNotificationAckAfterRouteChange(
				ctx, notification,
			); err != nil {
				return err
			}
			continue
		}
		remote, advanced, err := s.fetchAdvancedNotificationThread(ctx, host, client, notification)
		if err != nil {
			// The pre-ack refetch spends the same upstream budget as the
			// mark-read call, so a rate limit here must defer the queued ack
			// rather than retry the same due row every tick. Only this refetch
			// API error routes through backoff; the persistence error below
			// surfaces normally so a failed local refresh is not hidden.
			limited, deferErr := s.deferQueuedNotificationAckOnError(
				ctx, kind, host, bucket, bucketRepos[bucket], notification, err,
			)
			if deferErr != nil {
				return deferErr
			}
			if limited {
				exhausted[bucket] = struct{}{}
				rateLimitErr = fmt.Errorf(
					"notification read propagation rate limited for %s/%s on %s: %w",
					notification.RepoOwner, notification.RepoName, host, err,
				)
			}
			continue
		}
		if advanced {
			// New activity arrived since the read was queued. Refresh local
			// state from the upstream thread, preserving its read/unread flag
			// so a thread the user already read upstream is not resurrected,
			// and skip the mark-read so we never ack unseen activity.
			if err := s.persistOrReopenNotificationAck(
				ctx, host, notification, remote, routeFence, false,
			); err != nil {
				return err
			}
			continue
		}
		markRead := client.MarkNotificationThreadRead
		if routed, ok := client.(routedNotificationReadMarker); ok {
			markRead = func(ctx context.Context, threadID string) error {
				return routed.MarkNotificationThreadReadForRepo(
					ctx, notification.RepoOwner, notification.RepoName, threadID,
				)
			}
		}
		releaseRoute, err := s.db.LockRepositoryReconciliationRead(ctx)
		if err != nil {
			return err
		}
		routeMatches, err := s.db.RepositoryRouteFenceMatchesUnderRepositoryReconciliationRead(
			ctx, identity, routeFence,
		)
		if err != nil {
			releaseRoute()
			return err
		}
		if !routeMatches {
			releaseRoute()
			if err := s.reopenLegacyNotificationAckAfterRouteChange(
				ctx, notification,
			); err != nil {
				return err
			}
			continue
		}
		markErr := markRead(ctx, notification.PlatformNotificationID)
		releaseRoute()
		if markErr != nil {
			limited, deferErr := s.deferQueuedNotificationAckOnError(
				ctx, kind, host, bucket, bucketRepos[bucket], notification, markErr,
			)
			if deferErr != nil {
				return deferErr
			}
			if limited {
				exhausted[bucket] = struct{}{}
				rateLimitErr = fmt.Errorf(
					"notification read propagation rate limited for %s/%s on %s: %w",
					notification.RepoOwner, notification.RepoName, host, markErr,
				)
			}
			continue
		}
		// Reconciliation refetch: if the thread advanced between the pre-ack
		// refetch and the mark-read, our PATCH may have read newer activity
		// the user never saw, so force it back to unread. Do not clear the
		// queued ack until this refetch proves there was no newer activity.
		remote, advanced, err = s.fetchAdvancedNotificationThread(ctx, host, client, notification)
		if err != nil {
			limited, deferErr := s.reopenNotificationAfterPostAckRefetchError(
				ctx, kind, host, bucket, bucketRepos[bucket], notification, err,
			)
			if deferErr != nil {
				return deferErr
			}
			if limited {
				exhausted[bucket] = struct{}{}
				rateLimitErr = fmt.Errorf(
					"notification read propagation rate limited for %s/%s on %s: %w",
					notification.RepoOwner, notification.RepoName, host, err,
				)
			}
			continue
		}
		if advanced {
			if err := s.persistOrReopenNotificationAck(
				ctx, host, notification, remote, routeFence, true,
			); err != nil {
				return err
			}
			continue
		}
		syncedAt := time.Now().UTC()
		if err := s.db.MarkNotificationAckPropagationResult(ctx, notification.ID, notification.SourceAckQueuedAt, notification.SourceUpdatedAt, &syncedAt, "", nil); err != nil {
			return err
		}
	}
	// Healthy credentials finished their rows; still report the rate limit so
	// the caller records the host's sync error rather than claiming success.
	return rateLimitErr
}

// ackRepoBuckets groups the repositories whose queued acknowledgements share a
// credential. A rate limit belongs to the credential that hit it, so only its
// repositories may be deferred; repositories on the same host served by another
// credential still have quota.
//
// Both the due batch and the tracked repository list contribute. The batch is
// always present, so deferral works even for a syncer with no tracked repos,
// while the tracked list also reaches queued rows this batch did not include.
func (s *Syncer) ackRepoBuckets(
	kind platform.Kind, host string, queued []db.Notification,
) (map[string][]db.NotificationRepoRef, map[int64]string) {
	byBucket := make(map[string][]db.NotificationRepoRef)
	byNotification := make(map[int64]string, len(queued))
	seen := make(map[string]struct{})
	write := kind == platform.KindGitHub
	add := func(bucket string, repoID int64, owner, name string) {
		key := bucket + "\x00" + strconv.FormatInt(repoID, 10) + "\x00" +
			strings.ToLower(owner) + "/" + strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		byBucket[bucket] = append(byBucket[bucket], db.NotificationRepoRef{
			RepoID: repoID, Owner: owner, Name: name,
		})
	}
	for _, notification := range queued {
		bucket, err := s.bucketKeyForRepo(RepoRef{
			Platform: kind, PlatformHost: host,
			Owner: notification.RepoOwner, Name: notification.RepoName,
		}, write)
		if err != nil {
			continue
		}
		byNotification[notification.ID] = bucket
		var repoID int64
		if notification.RepoID != nil {
			repoID = *notification.RepoID
		}
		add(bucket, repoID, notification.RepoOwner, notification.RepoName)
	}
	// Archived repos stay in the grouping: their queued acknowledgements
	// (from before archiving) must still be covered by credential-wide
	// rate-limit deferral. Only polling skips archived repos.
	for _, repo := range s.TrackedRepos() {
		if repoPlatform(repo) != kind || repoHost(repo) != host {
			continue
		}
		bucket, err := s.bucketKeyForRepo(repo, write)
		if err != nil {
			continue
		}
		if _, relevant := byBucket[bucket]; !relevant {
			continue
		}
		add(bucket, repo.RepoID, repo.Owner, repo.Name)
	}
	return byBucket, byNotification
}

// reopenNotificationAfterPostAckRefetchError puts the notification back in
// front of the user when the reconciliation refetch could not prove the thread
// was unchanged. The refetch spends the same credential's budget as the
// mark-read, so a rate limit here belongs to that credential alone: only its
// repositories' remaining queued acks defer, and rateLimited tells the caller to
// stop spending this bucket while other credentials on the host keep going.
func (s *Syncer) reopenNotificationAfterPostAckRefetchError(
	ctx context.Context,
	kind platform.Kind,
	host string,
	bucket string,
	bucketRepos []db.NotificationRepoRef,
	notification db.Notification,
	cause error,
) (rateLimited bool, err error) {
	if err := s.db.ReopenNotificationAckPropagation(ctx, notification.ID, notification.SourceAckQueuedAt, notification.SourceUpdatedAt); err != nil {
		return false, err
	}
	nextAttemptAt, ok := notificationReadRateLimitNextAttempt(cause, time.Now().UTC())
	if !ok {
		return false, nil
	}
	if recordErr := s.db.DeferQueuedNotificationAcksForRepos(
		ctx, string(kind), host, bucketRepos, nextAttemptAt, "rate_limited",
	); recordErr != nil {
		return true, recordErr
	}
	slog.Warn("notification read propagation rate limited after ack",
		"host", host, "bucket", bucket,
		"owner", notification.RepoOwner, "name", notification.RepoName,
		"err", cause,
	)
	return true, nil
}

// deferQueuedNotificationAckOnError records backoff after a propagation step
// (thread refetch or mark-read) fails for a queued ack. Rate-limit errors defer
// the queued acks of the repositories sharing the exhausted credential, so the
// caller can skip that bucket instead of burning a budget that cannot make
// progress; any other error records a per-row next-attempt time so only this
// row backs off. A nil error means the ack was deferred and the caller should
// advance to the next queued row.
func (s *Syncer) deferQueuedNotificationAckOnError(
	ctx context.Context,
	kind platform.Kind,
	host string,
	bucket string,
	bucketRepos []db.NotificationRepoRef,
	notification db.Notification,
	cause error,
) (rateLimited bool, err error) {
	now := time.Now().UTC()
	if nextAttemptAt, ok := notificationReadRateLimitNextAttempt(cause, now); ok {
		if recordErr := s.db.DeferQueuedNotificationAcksForRepos(
			ctx, string(kind), host, bucketRepos, nextAttemptAt, "rate_limited",
		); recordErr != nil {
			return true, recordErr
		}
		slog.Warn("notification read propagation rate limited",
			"host", host, "bucket", bucket,
			"owner", notification.RepoOwner, "name", notification.RepoName,
			"err", cause,
		)
		return true, nil
	}
	errText := cause.Error()
	var nextAttemptAt *time.Time
	if notification.SourceAckAttempts+1 >= defaultNotificationPropagationMaxAttempts {
		errText = "max_attempts_exceeded"
	} else {
		next := now.Add(notificationReadBackoff(notification.SourceAckAttempts + 1))
		nextAttemptAt = &next
	}
	if recordErr := s.db.MarkNotificationAckPropagationResult(ctx, notification.ID, notification.SourceAckQueuedAt, notification.SourceUpdatedAt, nil, errText, nextAttemptAt); recordErr != nil {
		return false, recordErr
	}
	return false, nil
}

// fetchAdvancedNotificationThread refetches the upstream thread and reports
// whether it advanced past the locally recorded source_updated_at. A provider
// without the refetch capability or an unchanged thread reports advanced=false
// with no error. The returned error is always the refetch API error, so
// callers can route it through ack backoff; local persistence is handled
// separately by persistReopenedNotification so its failures are not mistaken
// for an upstream/ack failure.
func (s *Syncer) fetchAdvancedNotificationThread(
	ctx context.Context,
	host string,
	client notificationClient,
	notification db.Notification,
) (NotificationThread, bool, error) {
	var remote NotificationThread
	var err error
	if routed, ok := client.(routedNotificationThreadGetter); ok {
		remote, err = routed.GetNotificationThreadForRepo(
			ctx, notification.RepoOwner, notification.RepoName,
			notification.PlatformNotificationID,
		)
	} else {
		getter, ok := notificationThreadGetterFor(client)
		if !ok {
			return NotificationThread{}, false, nil
		}
		remote, err = getter.GetNotificationThread(
			ctx, notification.PlatformNotificationID,
		)
	}
	if err != nil {
		return NotificationThread{}, false, fmt.Errorf("get notification thread %s for %s: %w", notification.PlatformNotificationID, host, err)
	}
	if !remote.UpdatedAt.After(notification.SourceUpdatedAt) {
		return NotificationThread{}, false, nil
	}
	return remote, true, nil
}

// persistReopenedNotification refreshes local state from an advanced upstream
// thread. forceUnread marks the row unread regardless of the refreshed flag:
// the post-ack reconciliation path sets it because our own mark-read has
// already flipped the upstream thread to read, so the refetch can no longer
// report the unseen activity as unread. The pre-ack path passes false so a
// thread the user already read upstream is not resurrected as unread.
func (s *Syncer) persistReopenedNotification(
	ctx context.Context,
	host string,
	notification db.Notification,
	remote NotificationThread,
	routeFence db.RepositoryRouteFence,
	forceUnread bool,
) (bool, error) {
	if routeFence.RepoID == 0 {
		return false, nil
	}
	observedIdentity := db.RepoIdentity{
		Platform:     notification.Platform,
		PlatformHost: host,
		Owner:        notification.RepoOwner,
		Name:         notification.RepoName,
	}
	observedRepoID := routeFence.RepoID
	if remote.ID == "" {
		remote.ID = notification.PlatformNotificationID
	}
	if remote.RepoOwner == "" {
		remote.RepoOwner = notification.RepoOwner
	}
	if remote.RepoName == "" {
		remote.RepoName = notification.RepoName
	}
	if forceUnread {
		remote.Unread = true
	}
	// Preserve the original provider identity: notificationToDB keys off
	// repoPlatform(repo), so dropping Platform here would re-upsert the
	// refreshed notification under GitHub for any non-GitHub provider.
	repo := RepoRef{
		Platform:     platform.Kind(notification.Platform),
		Owner:        remote.RepoOwner,
		Name:         remote.RepoName,
		PlatformHost: host,
	}
	repo.RepoID = observedRepoID
	refreshed, err := s.notificationToDB(ctx, host, repo, remote, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("normalize refreshed notification %s for %s: %w", notification.PlatformNotificationID, host, err)
	}
	committed, err := s.db.UpsertNotificationsIfRouteFence(
		ctx, []db.Notification{refreshed}, observedIdentity, routeFence,
	)
	if err != nil {
		return false, fmt.Errorf("upsert refreshed notification %s for %s: %w", notification.PlatformNotificationID, host, err)
	}
	return committed, nil
}

func (s *Syncer) persistOrReopenNotificationAck(
	ctx context.Context,
	host string,
	notification db.Notification,
	remote NotificationThread,
	routeFence db.RepositoryRouteFence,
	forceUnread bool,
) error {
	committed, err := s.persistReopenedNotification(
		ctx, host, notification, remote, routeFence, forceUnread,
	)
	if err != nil || committed {
		return err
	}
	if notification.RepoID != nil {
		// A linked repository can be renamed while this provider request is
		// in flight. Keep its stable-ID acknowledgement queued so the next
		// pass resolves the repository's current route and retries there.
		return nil
	}
	if forceUnread || remote.Unread {
		return s.db.ReactivateNotificationAckPropagation(
			ctx, notification.ID, notification.SourceAckQueuedAt,
			notification.SourceUpdatedAt,
		)
	}
	return s.db.ReopenNotificationAckPropagation(
		ctx, notification.ID, notification.SourceAckQueuedAt,
		notification.SourceUpdatedAt,
	)
}

func (s *Syncer) reopenLegacyNotificationAckAfterRouteChange(
	ctx context.Context,
	notification db.Notification,
) error {
	if notification.RepoID != nil {
		// ListQueuedNotificationAcks resolves linked rows by repo_id on every
		// pass, so retaining the queue is sufficient to retry a rename safely.
		return nil
	}
	return s.db.ReopenNotificationAckPropagation(
		ctx, notification.ID, notification.SourceAckQueuedAt,
		notification.SourceUpdatedAt,
	)
}

func notificationReadRateLimitNextAttempt(err error, now time.Time) (time.Time, bool) {
	if rateLimitErr, ok := errors.AsType[*gh.RateLimitError](err); ok {
		resetAt := rateLimitErr.Rate.Reset.UTC()
		if resetAt.After(now) {
			return resetAt, true
		}
		return now.Add(notificationReadBackoff(1)), true
	}
	if abuseRateLimitErr, ok := errors.AsType[*gh.AbuseRateLimitError](err); ok {
		if abuseRateLimitErr.RetryAfter != nil && *abuseRateLimitErr.RetryAfter > 0 {
			return now.Add(*abuseRateLimitErr.RetryAfter), true
		}
		return now.Add(notificationReadBackoff(1)), true
	}
	return time.Time{}, false
}

func (s *Syncer) ProcessQueuedNotificationReadsForAllHosts(ctx context.Context, batchSize int) error {
	var errs []error
	for _, entry := range s.notificationClients() {
		if err := s.ProcessQueuedNotificationReads(ctx, entry.platform, entry.host, batchSize); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func notificationReadBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 6 {
		attempts = 6
	}
	return time.Duration(1<<uint(attempts-1)) * time.Minute
}

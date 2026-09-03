<script lang="ts">
  import { onMount } from "svelte";
  import { Effect } from "effect";
  import { pollWhileVisible } from "../../effect/poll-while-visible.js";
  import { FilterDropdown } from "@kenn-io/kit-ui";
  import { getStores } from "../../context.js";
  import type { ProviderRouteRef } from "../../api/provider-routes.js";
  import { buildIssueRoute, buildRepoBrowserRoute } from "../../routes.js";
  import { showFlash } from "../../stores/flash.svelte.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import type { AppExecution } from "../../app/runtime.js";
  import { executeGeneratedApiRequest } from "../../api/generated-api.js";

  import {
    RefreshIcon,
    SearchIcon,
  } from "../../icons.js";
  import { apiErrorMessage } from "../../api/runtime.js";
  import { setGlobalRepo } from "../../stores/filter.svelte.js";
  import { navigate } from "../../stores/router.svelte.js";
  import RepoMetricGrid from "./RepoMetricGrid.svelte";
  import RepoPageState from "./RepoPageState.svelte";
  import RepoSummaryCard from "./RepoSummaryCard.svelte";
  import RepoIssueModal from "./RepoIssueModal.svelte";
  import {
    normalizeSummaries,
    providerRepoStateKey,
    repoKey,
    repoStateKey,
    isStaleRelease,
    type RepoFilter,
    type RepoMetric,
    type RepoSort,
    type RepoSummaryCard as RepoSummaryCardData,
  } from "./repoSummary.js";
  import {
    loadRepoSummaryFilters,
    saveRepoSummaryFilters,
  } from "./repoSummaryFilters.js";
  import {
    makeRepoSummaryPresenterID,
    repoSummaryIssueKey,
    RepoSummaryWorkflow,
    type RepoSummaryIssueState,
    type RepoSummaryReadError,
  } from "./repo-summary-workflow.js";

  const stores = getStores();
  const appRuntime = getAppRuntime();
  const initialFilters = loadRepoSummaryFilters();
  let loadExecution: AppExecution<void, never> | null = null;
  let refreshExecution: AppExecution<void, never> | null = null;
  let roborevExecution: AppExecution<void, never> | null = null;
  const issuePresenterID = makeRepoSummaryPresenterID();
  const retainedIssueStates: Record<string, RepoSummaryIssueState> = {};
  let issuePresenterNeedsSummaryReplay = true;

  let summaries = $state<RepoSummaryCardData[]>([]);
  let roborevConfiguredRepos = $state.raw<Set<string>>(new Set());
  let loading = $state(true);
  let loadError = $state<string | null>(null);
  let composerSummary = $state<RepoSummaryCardData | null>(null);
  let issueTitleByRepo = $state<Record<string, string>>({});
  let issueBodyByRepo = $state<Record<string, string>>({});
  let issueErrorByRepo = $state<
    Record<string, string | null>
  >({});
  let issueSubmittingByRepo = $state<Record<string, boolean>>(
    {},
  );
  let issueOutcomeUnknownByRepo = $state<Record<string, boolean>>({});
  let searchQuery = $state(initialFilters.searchQuery);
  let activeFilter = $state<RepoFilter>(initialFilters.activeFilter);
  let sortMode = $state<RepoSort>(initialFilters.sortMode);

  const totals = $derived.by(() =>
    summaries.reduce(
      (acc, summary) => ({
        openPRs: acc.openPRs + summary.open_pr_count,
        openIssues:
          acc.openIssues + summary.open_issue_count,
        draftPRs: acc.draftPRs + summary.draft_pr_count,
        staleReleases: acc.staleReleases + (isStaleRelease(summary) ? 1 : 0),
      }),
      { openPRs: 0, openIssues: 0, draftPRs: 0, staleReleases: 0 },
    )
  );

  const overviewMetrics = $derived<RepoMetric[]>([
    {
      label: "Total repos",
      value: summaries.length,
    },
    {
      label: "Open PRs",
      value: totals.openPRs,
      tone: "blue",
    },
    {
      label: "Open issues",
      value: totals.openIssues,
      tone: "green",
    },
    {
      label: "Draft PRs",
      value: totals.draftPRs,
      tone: "amber",
    },
    {
      label: "Stale",
      value: totals.staleReleases,
      tone: "red",
    },
  ]);

  const sortOptions: { value: RepoSort; label: string }[] = [
    { value: "name", label: "Name" },
    { value: "open-prs", label: "Open PRs" },
    { value: "open-issues", label: "Open issues" },
    { value: "activity", label: "Recent activity" },
    { value: "stale", label: "Stale" },
  ];

  const sortDetail = $derived(
    sortOptions.find((option) => option.value === sortMode)?.label ?? "Name",
  );

  const sortSections = $derived.by(() => [
    {
      items: sortOptions.map((option) => ({
        id: option.value,
        label: option.label,
        active: sortMode === option.value,
        closeOnSelect: true,
        onSelect: () => setSort(option.value),
      })),
    },
  ]);

  const searchedSummaries = $derived.by(() => {
    const q = searchQuery.trim().toLowerCase();
    if (q === "") return summaries;
    return summaries.filter((summary) =>
      repoKey(summary).toLowerCase().includes(q)
        || summary.platform_host.toLowerCase().includes(q));
  });

  const filteredSummaries = $derived.by(() => {
    const matches = searchedSummaries.filter((summary) => {
      if (activeFilter === "prs" && summary.open_pr_count === 0) return false;
      if (activeFilter === "issues" && summary.open_issue_count === 0) return false;
      if (activeFilter === "stale" && !isStaleRelease(summary)) return false;
      return true;
    });

    return [...matches].sort((a, b) => {
      switch (sortMode) {
        case "open-prs":
          return b.open_pr_count - a.open_pr_count || repoKey(a).localeCompare(repoKey(b));
        case "open-issues":
          return b.open_issue_count - a.open_issue_count || repoKey(a).localeCompare(repoKey(b));
        case "activity":
          return dateValue(b.most_recent_activity_at) - dateValue(a.most_recent_activity_at)
            || repoKey(a).localeCompare(repoKey(b));
        case "stale":
          return (b.commits_since_release ?? -1) - (a.commits_since_release ?? -1)
            || repoKey(a).localeCompare(repoKey(b));
        case "name":
        default:
          return repoKey(a).localeCompare(repoKey(b));
      }
    });
  });

  const showProviderIcons = $derived.by(() => {
    const providers = new Set(
      summaries.map((summary) => summary.repo.provider.toLowerCase()),
    );
    return providers.size > 1;
  });

  function resultsLabel(count: number): string {
    return `${count} ${count === 1 ? "result" : "results"}`;
  }

  function dateValue(value: string | undefined): number {
    if (!value) return 0;
    return new Date(value).getTime();
  }

  function repoSummaryFailureMessage(failure: RepoSummaryReadError, fallback: string): string {
    return failure._tag === "ApiProblemError"
      ? apiErrorMessage(failure.problem, fallback)
      : failure.cause instanceof Error
        ? failure.cause.message
        : fallback;
  }

  function readSummariesProgram() {
    return Effect.gen(function* () {
      const workflow = yield* RepoSummaryWorkflow;
      return normalizeSummaries(yield* workflow.read);
    });
  }

  function repoIssueRef(summary: RepoSummaryCardData): ProviderRouteRef {
    return {
      provider: summary.repo.provider,
      platformHost: summary.repo.platform_host,
      owner: summary.repo.owner,
      name: summary.repo.name,
      repoPath: summary.repo.repo_path,
    };
  }

  function issueStateKey(summary: RepoSummaryCardData): string {
    return repoSummaryIssueKey(repoIssueRef(summary));
  }

  function issueSummaryFor(key: string): RepoSummaryCardData | undefined {
    return summaries.find((summary) => issueStateKey(summary) === key);
  }

  function presentIssueState(state: RepoSummaryIssueState): boolean {
    const { key } = state.request;
    retainedIssueStates[key] = state;
    issueTitleByRepo[key] = state.request.title;
    issueBodyByRepo[key] = state.request.body ?? "";

    if (state.kind === "succeeded") {
      delete retainedIssueStates[key];
      issueTitleByRepo[key] = "";
      issueBodyByRepo[key] = "";
      issueSubmittingByRepo[key] = false;
      issueOutcomeUnknownByRepo[key] = false;
      issueErrorByRepo[key] = null;
      composerSummary = null;
      setGlobalRepo(
        providerRepoStateKey({
          provider: state.request.ref.provider,
          platform_host: state.request.ref.platformHost ?? "",
          repo_path: state.request.ref.repoPath,
        }),
      );
      navigate(buildIssueRoute({ ...state.request.ref, number: state.issue.Number }));
      return true;
    }

    const summary = issueSummaryFor(key);
    if (summary === undefined) return false;
    composerSummary = summary;

    if (state.kind === "pending") {
      issueSubmittingByRepo[key] = true;
      issueOutcomeUnknownByRepo[key] = false;
      issueErrorByRepo[key] = null;
      return false;
    }

    issueSubmittingByRepo[key] = false;
    const message = repoSummaryFailureMessage(state.error, "failed to create issue");
    if (state.kind === "uncertain") {
      const warning = `${message} The request outcome is unknown; check the issue list before retrying.`;
      issueOutcomeUnknownByRepo[key] = true;
      issueErrorByRepo[key] = warning;
      showFlash(warning, { tone: "danger" });
      return false;
    }

    delete retainedIssueStates[key];
    issueOutcomeUnknownByRepo[key] = false;
    issueErrorByRepo[key] = null;
    showFlash(message, { tone: "danger" });
    return true;
  }

  function claimIssuePresenterProgram() {
    return Effect.gen(function* () {
      const workflow = yield* RepoSummaryWorkflow;
      yield* workflow.claimIssuePresenter(
        issuePresenterID,
        (state) => Effect.sync(() => presentIssueState(state)),
      );
    });
  }

  function replayIssuePresenterAfterFirstSummaryLoadProgram() {
    return Effect.gen(function* () {
      const shouldReplay = yield* Effect.sync(() => {
        if (!issuePresenterNeedsSummaryReplay) return false;
        issuePresenterNeedsSummaryReplay = false;
        return true;
      });
      if (!shouldReplay) return;
      yield* claimIssuePresenterProgram();
    });
  }

  function loadSummaries(): void {
    const showSpinner = summaries.length === 0;
    if (showSpinner) loading = true;
    loadError = null;
    loadExecution = appRuntime.runCommand(
      readSummariesProgram().pipe(
        Effect.tap((loaded) =>
          Effect.sync(() => {
            summaries = loaded;
            loading = false;
          }),
        ),
        Effect.andThen(replayIssuePresenterAfterFirstSummaryLoadProgram()),
        Effect.catch((failure) =>
          Effect.sync(() => {
            loadError = repoSummaryFailureMessage(failure, "failed to load repositories");
            loading = false;
          }),
        ),
        Effect.asVoid,
      ),
      {
        operation: "repositories.summary.read",
        safeContext: { surface: "repositories" },
        onFailure: () => undefined,
      },
    );
  }

  function loadRoborevConfiguredRepositories(): void {
    roborevExecution?.interrupt();
    roborevExecution = appRuntime.runCommand(
      executeGeneratedApiRequest("load Roborev configured repositories", (generatedClient, signal) =>
        generatedClient.RoborevService.listRoborevConfiguredRepositories({ signal }),
      ).pipe(
        Effect.tap((data) =>
          Effect.sync(() => {
            roborevConfiguredRepos = new Set(
              (data.repositories ?? []).map(providerRepoStateKey),
            );
          }),
        ),
        // Roborev is optional; repository summaries remain authoritative.
        Effect.catch(() => Effect.void),
        Effect.asVoid,
      ),
      {
        operation: "repositories.roborev-config.read",
        safeContext: { surface: "repositories" },
        onFailure: () => undefined,
      },
    );
  }

  function refreshSummaries(): void {
    refreshExecution = appRuntime.runCommand(
      executeGeneratedApiRequest("refresh repository summaries", (generatedClient, signal) =>
        generatedClient.SyncService.triggerSync(undefined, { signal }),
      ).pipe(
        Effect.andThen(readSummariesProgram()),
        Effect.tap((loaded) =>
          Effect.sync(() => {
            summaries = loaded;
            loadError = null;
            loading = false;
          }),
        ),
        Effect.catch((failure) =>
          Effect.sync(() => {
            showFlash(repoSummaryFailureMessage(failure, "failed to refresh repositories"), { tone: "danger" });
          }),
        ),
        Effect.asVoid,
      ),
      {
        operation: "repositories.summary.refresh",
        safeContext: { surface: "repositories" },
        onFailure: () => undefined,
      },
    );
  }

  function setFilter(filter: RepoFilter): void {
    activeFilter = filter;
    persistFilters();
  }

  function updateSearch(event: Event): void {
    searchQuery = event.currentTarget instanceof HTMLInputElement
      ? event.currentTarget.value
      : "";
    persistFilters();
  }

  function setSort(sort: RepoSort): void {
    sortMode = sort;
    persistFilters();
  }

  function persistFilters(): void {
    saveRepoSummaryFilters({
      searchQuery,
      activeFilter,
      sortMode,
    });
  }

  function filterAndNavigate(
    summary: RepoSummaryCardData,
    path: string,
  ): void {
    setGlobalRepo(repoStateKey(summary));
    navigate(path);
  }

  function viewRepo(summary: RepoSummaryCardData): void {
    filterAndNavigate(
      summary,
      buildRepoBrowserRoute({
        provider: summary.repo.provider,
        platformHost: summary.repo.platform_host,
        owner: summary.repo.owner,
        name: summary.repo.name,
        repoPath: summary.repo.repo_path,
      }),
    );
  }

  function openComposer(summary: RepoSummaryCardData): void {
    if (!summary.repo.capabilities.issue_mutation) return;
    const key = issueStateKey(summary);
    composerSummary = summary;
    if (!issueOutcomeUnknownByRepo[key]) issueErrorByRepo[key] = null;
    if (issueTitleByRepo[key] === undefined) {
      issueTitleByRepo[key] = "";
    }
    if (issueBodyByRepo[key] === undefined) {
      issueBodyByRepo[key] = "";
    }
  }

  function closeComposer(key: string): void {
    if (composerSummary && issueStateKey(composerSummary) === key) {
      composerSummary = null;
    }
    if (!issueOutcomeUnknownByRepo[key]) issueErrorByRepo[key] = null;
  }

  function updateIssueTitle(
    key: string,
    title: string,
  ): void {
    issueTitleByRepo[key] = title;
  }

  function updateIssueBody(key: string, body: string): void {
    issueBodyByRepo[key] = body;
  }

  function submitIssue(
    summary: RepoSummaryCardData,
  ): void {
    if (!summary.repo.capabilities.issue_mutation) return;
    const key = issueStateKey(summary);
    if (issueSubmittingByRepo[key]) return;
    if (issueOutcomeUnknownByRepo[key]) return;

    const title = (issueTitleByRepo[key] ?? "").trim();
    if (title === "") {
      issueErrorByRepo[key] = "Title is required.";
      return;
    }

    issueSubmittingByRepo[key] = true;
    issueErrorByRepo[key] = null;

    appRuntime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* RepoSummaryWorkflow;
        yield* workflow.createIssue({
          ref: repoIssueRef(summary),
          title,
          body: issueBodyByRepo[key] ?? "",
        });
      }),
      {
        operation: "repositories.issue.create",
        safeContext: { surface: "repositories" },
        onFailure: () => {
          issueSubmittingByRepo[key] = false;
          showFlash("The issue request stopped before it was accepted.", { tone: "danger" });
        },
      },
    );
  }

  function submitActiveIssue(): void {
    if (!composerSummary) return;
    submitIssue(composerSummary);
  }

  function acknowledgeUnknownIssueOutcome(key: string): void {
    const state = retainedIssueStates[key];
    if (state?.kind !== "uncertain") return;
    appRuntime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* RepoSummaryWorkflow;
        yield* workflow.acknowledgeUncertainIssue(key, state.request.submissionID, issuePresenterID);
        yield* Effect.sync(() => {
          delete retainedIssueStates[key];
          issueOutcomeUnknownByRepo[key] = false;
          issueErrorByRepo[key] = null;
        });
      }),
      {
        operation: "repositories.issue.acknowledge-uncertain",
        safeContext: { surface: "repositories" },
        onFailure: () => undefined,
      },
    );
  }

  onMount(() => {
    loadSummaries();
    loadRoborevConfiguredRepositories();
    const lifecycle = appRuntime.runCommand(
      Effect.scoped(
        Effect.gen(function* () {
          const workflow = yield* RepoSummaryWorkflow;
          yield* Effect.acquireRelease(
            workflow.claimIssuePresenter(
              issuePresenterID,
              (state) => Effect.sync(() => presentIssueState(state)),
            ),
            () => workflow.releaseIssuePresenter(issuePresenterID),
          );
          yield* Effect.acquireRelease(
            Effect.sync(() => stores.sync.subscribeSyncComplete(loadSummaries)),
            (unsubscribe) => Effect.sync(unsubscribe),
          );
          yield* pollWhileVisible(Effect.sync(loadSummaries), "30 seconds");
        }),
      ),
      {
        operation: "repositories.summary.lifecycle",
        safeContext: { surface: "repositories" },
        onFailure: () => undefined,
      },
    );
    return () => {
      lifecycle.interrupt();
      loadExecution?.interrupt();
      refreshExecution?.interrupt();
      roborevExecution?.interrupt();
    };
  });
</script>

<section class="repo-page">
  <header class="repo-page__header">
    <div>
      <h1 class="repo-page__title">Repositories</h1>
      <p class="repo-page__subtitle">
        Summary of your tracked GitHub repositories
      </p>
    </div>
    <RepoMetricGrid metrics={overviewMetrics} strip />
  </header>

  {#if stores.settings.isSettingsLoaded() && !stores.settings.hasConfiguredRepos()}
    <RepoPageState
      title="No repositories configured"
      message="Add a repository in Settings before using the repository overview."
      actionLabel="Open Settings"
      onaction={() => navigate("/settings")}
    />
  {:else if loading}
    <RepoPageState
      title="Loading repositories"
      message="Fetching the latest cached repo summaries."
    />
  {:else if loadError}
    <RepoPageState
      title="Couldn’t load repositories"
      message={loadError}
      tone="error"
      actionLabel="Retry"
      onaction={loadSummaries}
    />
  {:else if summaries.length === 0}
    <RepoPageState
      title="No cached repositories yet"
      message="Run a sync to populate repository summaries."
    />
  {:else}
    <div class="repo-page__toolbar">
      <label class="repo-page__search">
        <SearchIcon size={16} aria-hidden="true" />
        <input
          value={searchQuery}
          placeholder="Filter repositories"
          oninput={updateSearch}
        />
      </label>

      <div class="repo-page__filters" aria-label="Repository filters">
        <button
          type="button"
          class={[
            "repo-page__filter",
            { "repo-page__filter--active": activeFilter === "all" },
          ]}
          onclick={() => setFilter("all")}
        >
          All
        </button>
        <button
          type="button"
          class={[
            "repo-page__filter",
            { "repo-page__filter--active": activeFilter === "prs" },
          ]}
          onclick={() => setFilter("prs")}
        >
          Has PRs
        </button>
        <button
          type="button"
          class={[
            "repo-page__filter",
            { "repo-page__filter--active": activeFilter === "issues" },
          ]}
          onclick={() => setFilter("issues")}
        >
          Has issues
        </button>
        <button
          type="button"
          class={[
            "repo-page__filter",
            { "repo-page__filter--active": activeFilter === "stale" },
          ]}
          onclick={() => setFilter("stale")}
        >
          Stale
        </button>
      </div>

      <div class="repo-page__sort">
        <div class="repo-page__sort-dropdown">
          <FilterDropdown
            label={sortDetail}
            showBadge={false}
            sections={sortSections}
            title="Sort repositories"
            minWidth="180px"
            icon="sort"
          />
        </div>
        <span class="repo-page__results">
          <span class="repo-page__results-sizer" aria-hidden="true">
            {searchedSummaries.length} results
          </span>
          <span class="repo-page__results-value">
            {resultsLabel(filteredSummaries.length)}
          </span>
        </span>
        <button
          type="button"
          class="repo-page__refresh"
          title="Refresh repositories"
          aria-label="Refresh repositories"
          onclick={refreshSummaries}
        >
          <RefreshIcon size={16} aria-hidden="true" />
        </button>
      </div>
    </div>

    {#if filteredSummaries.length === 0}
      <RepoPageState
        title="No repositories match"
        message="Adjust the filters or search query to see more repositories."
      />
    {:else}
    <div class="repo-grid">
      {#each filteredSummaries as summary (repoStateKey(summary))}
        <RepoSummaryCard
          {summary}
          roborevConfigured={roborevConfiguredRepos.has(repoStateKey(summary))}
          showProviderIcon={showProviderIcons}
          onviewprs={() =>
            filterAndNavigate(summary, "/pulls")}
          onviewissues={() =>
            filterAndNavigate(summary, "/issues")}
          onviewrepo={() => viewRepo(summary)}
          onopencomposer={() => openComposer(summary)}
          onopenissue={(number) =>
            filterAndNavigate(
              summary,
              buildIssueRoute({
                provider: summary.repo.provider,
                platformHost: summary.repo.platform_host,
                owner: summary.repo.owner,
                name: summary.repo.name,
                repoPath: summary.repo.repo_path,
                number,
              }),
            )}
        />
      {/each}
    </div>
    {/if}
  {/if}

  {#if composerSummary}
    {@const key = issueStateKey(composerSummary)}
    <RepoIssueModal
      summary={composerSummary}
      title={issueTitleByRepo[key] ?? ""}
      body={issueBodyByRepo[key] ?? ""}
      error={issueErrorByRepo[key] ?? null}
      submitting={issueSubmittingByRepo[key] ?? false}
      outcomeUnknown={issueOutcomeUnknownByRepo[key] ?? false}
      ontitlechange={(title) => updateIssueTitle(key, title)}
      onbodychange={(body) => updateIssueBody(key, body)}
      oncancel={() => closeComposer(key)}
      onsubmitissue={submitActiveIssue}
      onacknowledgeoutcome={() => acknowledgeUnknownIssueOutcome(key)}
    />
  {/if}
</section>

<style>
  .repo-page {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: var(--space-6);
    overflow-y: auto;
    padding: 26px 28px;
    background: var(--bg-primary);
  }

  .repo-page__header {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(560px, 720px);
    gap: var(--space-6);
    align-items: start;
    padding-bottom: 20px;
    border-bottom: 1px solid var(--border-muted);
  }

  .repo-page__title {
    margin-bottom: 6px;
    color: var(--text-primary);
    font-size: var(--font-size-xl);
    font-weight: 700;
    line-height: 1.2;
  }

  .repo-page__subtitle {
    max-width: 560px;
    color: var(--text-secondary);
    font-size: var(--font-size-md);
  }

  .repo-page__toolbar {
    display: grid;
    grid-template-columns: minmax(220px, 1fr) max-content max-content;
    gap: 12px;
    align-items: center;
  }

  .repo-page__search {
    min-width: 0;
    display: flex;
    align-items: center;
    gap: 8px;
    height: 36px;
    padding: 0 12px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-muted);
    box-shadow: var(--shadow-sm);
  }

  .repo-page__search input {
    width: 100%;
    min-width: 0;
    padding: 0;
    border: 0;
    background: transparent;
    color: var(--text-primary);
  }

  .repo-page__search input:focus {
    border: 0;
  }

  .repo-page__filters,
  .repo-page__sort {
    display: flex;
    align-items: center;
  }

  .repo-page__filters {
    overflow: hidden;
    width: max-content;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    box-shadow: var(--shadow-sm);
  }

  .repo-page__filter {
    display: inline-flex;
    flex: 0 0 auto;
    align-items: center;
    height: 34px;
    padding: 0 14px;
    border: 0;
    border-right: 1px solid var(--border-muted);
    background: transparent;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: 500;
    min-width: max-content;
    white-space: nowrap;
  }

  .repo-page__filter:last-child {
    border-right: 0;
  }

  .repo-page__filter:hover {
    background: var(--bg-surface-hover);
  }

  .repo-page__filter--active {
    background: color-mix(in srgb, var(--accent-blue) 10%, var(--bg-surface));
    color: var(--accent-blue);
  }

  .repo-page__sort {
    justify-content: flex-end;
    gap: 12px;
    justify-self: end;
  }

  .repo-page__sort-dropdown :global(.kit-filter-dropdown__btn) {
    width: 148px;
    min-height: 34px;
    padding: 0 12px;
    border-color: var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    box-shadow: var(--shadow-sm);
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }

  .repo-page__sort-dropdown :global(.kit-filter-dropdown__trigger-label) {
    flex: 1;
    overflow: hidden;
    color: var(--text-primary);
    font-weight: 600;
    text-align: left;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .repo-page__results {
    display: inline-grid;
    justify-items: end;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-variant-numeric: tabular-nums;
  }

  /* Sizer and value stack in the same grid cell so the label is always as
     wide as the unfiltered ("All") count, preventing layout shift when
     switching filters changes the digit count. */
  .repo-page__results-sizer,
  .repo-page__results-value {
    grid-area: 1 / 1;
    white-space: nowrap;
  }

  .repo-page__results-sizer {
    visibility: hidden;
  }

  .repo-page__refresh {
    display: inline-grid;
    width: 34px;
    height: 34px;
    place-items: center;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-secondary);
    box-shadow: var(--shadow-sm);
  }

  .repo-page__refresh:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .repo-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(390px, 1fr));
    gap: 12px;
  }

  /* kit-ui-check-ignore: header/toolbar must stack before their max-content columns overflow; 960 sits above the shared ladder's widest step */
  @media (max-width: 960px) {
    .repo-page {
      padding: 18px;
    }

    .repo-page__header {
      grid-template-columns: 1fr;
    }

    .repo-page__toolbar {
      grid-template-columns: 1fr;
    }

    .repo-page__sort {
      justify-content: flex-start;
    }
  }

  @media (max-width: 640px) {
    .repo-grid {
      grid-template-columns: 1fr;
    }
  }
</style>

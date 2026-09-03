<script lang="ts">
  import { Effect } from "effect";
  import { getStores } from "../../context.js";
  import WorkspaceCreateSplitButton from "../workspace/WorkspaceCreateSplitButton.svelte";
  import {
    Button,
    SearchInput,
    SelectDropdown,
    type SelectDropdownOption,
    TextInput,
    Typeahead,
    type TypeaheadOption,
  } from "@kenn-io/kit-ui";
  import GitBranchIcon from "@lucide/svelte/icons/git-branch";
  import { canonicalProvider, providerHostRouteParams, providerRouteParams, providerUsesHostRoute } from "../../api/provider-routes.js";
  import type { Repo } from "../../api/types.js";
  import {
    ApiProblemError,
    InvalidExternalPayload,
    type TransientTransportError,
  } from "../../api/effect-errors.js";
  import { executeGeneratedApiRequest } from "../../api/generated-api.js";
  import { executeOpaqueGeneratedApiRequest } from "../../api/generated-api.js";
  import { loadFleetSnapshot, type HostSummary } from "../../api/fleet-snapshot.js";
  import type { ProblemBody } from "../../api/problems.js";
  import type { AppExecution } from "../../app/runtime.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { queueWorkspaceLaunch } from "../../stores/workspace-create-pending.svelte.js";
  import Modal from "../shared/Modal.svelte";
  import { apiErrorMessage } from "../../api/runtime.js";
  import {
    createOrOpenKataWorkspace,
    searchKataReferences,
    type KataIssueReference,
  } from "../../api/kata/integration.js";
  import { createKataDaemonsStore } from "../../stores/kata-daemons.svelte.js";
  import {
    beginKataWorkspaceCreate,
    createdKataWorkspaceRef,
    endKataWorkspaceCreate,
    isKataWorkspaceCreatePending,
    recordKataWorkspaceCreated,
    type KataWorkspaceIdentity,
  } from "../../stores/kata-workspace-create.svelte.js";
  import { navigate } from "../../stores/router.svelte.js";
  import {
    getLastUsedNewWorkspaceRepoKey,
    rememberNewWorkspaceRepoKey,
    type NewWorkspaceRepoSeed,
    type NewWorkspaceSource,
  } from "../../stores/new-workspace.svelte.js";

  // Starts new work in a tracked repository without a pull request, issue, or
  // Kata task: pick the repo, optionally name the branch, and kenn-forge
  // materializes a worktree from the repository's default branch.

  interface Props {
    open: boolean;
    onClose: () => void;
    seedRepo?: NewWorkspaceRepoSeed | null;
    initialSource?: NewWorkspaceSource;
    // Overridable so callers embedding this dialog outside the app shell (and
    // tests) can observe the created workspace instead of navigating.
    onCreated?: ((workspaceId: string, hostKey?: string) => void) | undefined;
  }

  const {
    open,
    onClose,
    seedRepo = null,
    initialSource = "repository",
    onCreated = undefined,
  }: Props = $props();
  const { settings } = getStores();
  const runtime = getAppRuntime();

  type RepoOption = {
    key: string;
    provider: string;
    platformHost: string;
    owner: string;
    name: string;
    label: string;
  };

  type CreatedWorkspacePayload = {
    id?: string;
    status?: string;
  };

  let repos = $state<RepoOption[]>([]);
  let reposLoading = $state(false);
  let reposError = $state<string | null>(null);
  let selectedKey = $state("");
  let branch = $state("");
  let submitting = $state(false);
  let error = $state<string | null>(null);
  let suggestedBranch = $state<string | null>(null);
  let pendingLaunchTargetKey = $state<string | null>(null);
  let workspaceHosts = $state.raw<HostSummary[]>([]);
  let selectedWorkspaceHostKey = $state("");
  let source = $state<NewWorkspaceSource>("repository");
  const kataDaemons = createKataDaemonsStore();
  let selectedDaemonID = $state("");
  let kataQuery = $state("");
  let kataReferences = $state.raw<KataIssueReference[]>([]);
  let selectedKataReference = $state.raw<KataIssueReference | null>(null);
  let kataSearching = $state(false);
  let kataRosterController: AbortController | null = null;
  let kataSearchController: AbortController | null = null;
  let kataSearchTimer: ReturnType<typeof setTimeout> | null = null;
  let kataSearchGeneration = 0;
  let activeSession: object | null = null;
  let repoLoadExecution: AppExecution<void, ApiProblemError | TransientTransportError> | null = null;
  let fleetLoadExecution: AppExecution<
    void,
    ApiProblemError | InvalidExternalPayload | TransientTransportError
  > | null = null;

  function clearPendingLaunch(): void {
    pendingLaunchTargetKey = null;
  }

  function loadWorkspaceHosts(session: object): void {
    fleetLoadExecution?.interrupt();
    const execution = runtime.runCommand(
      loadFleetSnapshot().pipe(
        Effect.tap((snapshot) =>
          Effect.sync(() => {
            if (activeSession !== session) return;
            const hosts = snapshot.hosts ?? [];
            const self = hosts.find((host) => host.kind === "self");
            workspaceHosts = self?.federationRole === "hub"
              ? hosts
              : self
                ? [self]
                : hosts.filter((host) => host.operationAvailability.workspaceWrite?.available === true);
            selectedWorkspaceHostKey =
              workspaceHosts.find((host) => host.kind === "self")?.configKey ??
              workspaceHosts[0]?.configKey ??
              "";
          }),
        ),
        Effect.asVoid,
      ),
      {
        operation: "load workspace execution hosts",
        safeContext: {},
        onFailure: () => {
          if (activeSession !== session) return;
          workspaceHosts = [];
          selectedWorkspaceHostKey = "";
        },
      },
    );
    fleetLoadExecution = execution;
  }

  function cancelKataSearch(): void {
    if (kataSearchTimer !== null) {
      clearTimeout(kataSearchTimer);
      kataSearchTimer = null;
    }
    kataSearchController?.abort();
    kataSearchController = null;
  }

  function resetKataSelection(): void {
    cancelKataSearch();
    kataSearchGeneration += 1;
    kataRosterController?.abort();
    kataRosterController = null;
    selectedDaemonID = "";
    kataQuery = "";
    kataReferences = [];
    selectedKataReference = null;
    kataSearching = false;
  }

  function repoOption(repo: Repo): RepoOption {
    const provider = canonicalProvider(repo.Platform);
    return {
      key: `${provider}/${repo.PlatformHost}/${repo.Owner}/${repo.Name}`,
      provider,
      platformHost: repo.PlatformHost,
      owner: repo.Owner,
      name: repo.Name,
      label: `${repo.Owner}/${repo.Name}`,
    };
  }

  function seedKey(seed: NewWorkspaceRepoSeed | null): string {
    if (!seed) return "";
    return `${canonicalProvider(seed.provider)}/${seed.platformHost}/${seed.owner}/${seed.name}`;
  }

  function normalizeCreatedWorkspace(value: unknown): CreatedWorkspacePayload {
    if (typeof value !== "object" || value === null) return {};
    const record = value as Record<string, unknown>;
    return {
      ...(typeof record.id === "string" ? { id: record.id } : {}),
      ...(typeof record.status === "string" ? { status: record.status } : {}),
    };
  }

  // An explicit seed is a promise about which repository the workspace
  // targets. When it cannot be resolved (for example a repository hidden
  // from the UI), require a choice instead of silently diverting to another
  // repository. Without a seed, prefer the last repo work was started in,
  // then the first.
  function defaultRepoSelection(): string {
    const seededRepoKey = seedKey(seedRepo);
    if (seededRepoKey) {
      return repos.some((repo) => repo.key === seededRepoKey) ? seededRepoKey : "";
    }
    const lastUsed = getLastUsedNewWorkspaceRepoKey();
    return (lastUsed && repos.some((repo) => repo.key === lastUsed) ? lastUsed : repos[0]?.key) ?? "";
  }

  // Each open starts a fresh request and fresh form state; a stale response
  // from a previous open must not repopulate the list. The previous list and
  // selection are dropped up front so a reopen cannot submit against the repo
  // picked last time while the new list is still in flight, or if it fails.
  $effect(() => {
    if (!open) {
      activeSession = null;
      repoLoadExecution?.interrupt();
      repoLoadExecution = null;
      fleetLoadExecution?.interrupt();
      fleetLoadExecution = null;
      kataRosterController?.abort();
      resetKataSelection();
      return;
    }
    const session = {};
    const requestedSource = initialSource;
    activeSession = session;
    source = requestedSource;
    branch = "";
    error = null;
    suggestedBranch = null;
    pendingLaunchTargetKey = null;
    workspaceHosts = [];
    selectedWorkspaceHostKey = "";
    submitting = false;
    repos = [];
    selectedKey = "";
    reposError = null;
    reposLoading = requestedSource === "repository";
    resetKataSelection();
    if (requestedSource === "kata_issue") {
      const controller = new AbortController();
      kataRosterController = controller;
      void kataDaemons.load(controller.signal).then(() => {
        if (activeSession !== session || controller.signal.aborted) return;
        selectedDaemonID = kataDaemons.defaultDaemonID();
        if (kataRosterController === controller) kataRosterController = null;
      });
      return () => {
        controller.abort();
        cancelKataSearch();
        if (activeSession === session) activeSession = null;
      };
    }
    loadWorkspaceHosts(session);
    const execution = runtime.runCommand(
      executeGeneratedApiRequest("load repositories", (client, signal) => client.RepositoriesService.listRepos({ signal })).pipe(
        Effect.flatMap((loaded) =>
          Effect.sync(() => {
            if (activeSession !== session) return;
            reposLoading = false;
            repos = (loaded ?? []).map(repoOption);
            selectedKey = defaultRepoSelection();
          }),
        ),
      ),
      {
        operation: "load repositories for a new workspace",
        safeContext: {},
        onFailure: (failure) => {
          if (activeSession !== session) return;
          reposLoading = false;
          reposError = failure instanceof ApiProblemError
            ? apiErrorMessage(failure.problem, "Could not load repositories")
            : "Could not load repositories";
        },
      },
    );
    repoLoadExecution = execution;
    return () => {
      execution.interrupt();
      if (repoLoadExecution === execution) repoLoadExecution = null;
      if (activeSession === session) activeSession = null;
    };
  });

  function chooseSource(nextSource: NewWorkspaceSource): void {
    if (source === nextSource || submitting) return;
    clearPendingLaunch();
    repoLoadExecution?.interrupt();
    repoLoadExecution = null;
    fleetLoadExecution?.interrupt();
    fleetLoadExecution = null;
    resetKataSelection();
    source = nextSource;
    branch = "";
    error = null;
    suggestedBranch = null;
    repos = [];
    selectedKey = "";
    reposError = null;
    const session = {};
    activeSession = session;
    if (source === "kata_issue") {
      reposLoading = false;
      const controller = new AbortController();
      kataRosterController = controller;
      void kataDaemons.load(controller.signal).then(() => {
        if (activeSession !== session || controller.signal.aborted) return;
        selectedDaemonID = kataDaemons.defaultDaemonID();
        if (kataRosterController === controller) kataRosterController = null;
      });
      return;
    }
    loadWorkspaceHosts(session);
    reposLoading = true;
    const execution = runtime.runCommand(
      executeGeneratedApiRequest("load repositories", (client, signal) => client.RepositoriesService.listRepos({ signal })).pipe(
        Effect.tap((loaded) => Effect.sync(() => {
          if (activeSession !== session) return;
          reposLoading = false;
          repos = (loaded ?? []).map(repoOption);
          selectedKey = defaultRepoSelection();
        })),
        Effect.asVoid,
      ),
      {
        operation: "load repositories for a new workspace",
        safeContext: {},
        onFailure: (failure) => {
          if (activeSession !== session) return;
          reposLoading = false;
          reposError = failure instanceof ApiProblemError
            ? apiErrorMessage(failure.problem, "Could not load repositories")
            : "Could not load repositories";
        },
      },
    );
    repoLoadExecution = execution;
  }

  const repoRows = $derived<TypeaheadOption[]>(
    repos.map((repo) => ({ name: repo.key, label: repo.label, meta: repo.platformHost })),
  );

  const selected = $derived(repos.find((repo) => repo.key === selectedKey) ?? null);
  const selectedWorkspaceHost = $derived(
    workspaceHosts.find((host) => host.configKey === selectedWorkspaceHostKey) ?? null,
  );
  const remoteWorkspaceHostKey = $derived(
    source !== "repository" || selectedWorkspaceHost?.kind === "self"
      ? undefined
      : selectedWorkspaceHost?.configKey,
  );
  const workspaceHostOptions = $derived<SelectDropdownOption[]>(
    workspaceHosts.map((host) => {
      const writeAvailability = host.operationAvailability.workspaceWrite;
      const unavailableReason = writeAvailability?.unavailableReason || "Workspace creation is unavailable.";
      return {
        value: host.configKey,
        label: `${host.name.trim() || host.hostname?.trim() || host.configKey}${host.kind === "self" ? " (this machine)" : ""}`,
        disabled: writeAvailability?.available !== true,
        ...(writeAvailability?.available === true
          ? {}
          : { indicator: { tone: "danger" as const, title: unavailableReason } }),
      };
    }),
  );

  const selectedDaemon = $derived(
    kataDaemons.daemons().find((daemon) => daemon.id === selectedDaemonID) ?? null,
  );
  const selectedDaemonUsable = $derived(selectedDaemon?.health === "connected");
  const selectedDaemonProblem = $derived.by(() => {
    if (!selectedDaemon || selectedDaemonUsable) return null;
    if (selectedDaemon.hint) return selectedDaemon.hint;
    return `Kata daemon health: ${selectedDaemon.health}.`;
  });
  const selectedKataWorkspaceIdentity = $derived<KataWorkspaceIdentity | null>(
    source === "kata_issue" && selectedKataReference && selectedDaemonID
      ? { daemonID: selectedDaemonID, issueUID: selectedKataReference.uid }
      : null,
  );
  const kataWorkspaceCreatePending = $derived(
    selectedKataWorkspaceIdentity
      ? isKataWorkspaceCreatePending(selectedKataWorkspaceIdentity)
      : false,
  );
  const daemonOptions = $derived(
    kataDaemons.daemons().map((daemon) => {
      const reason = daemon.health === "connected" ? "" : daemon.hint || `Health: ${daemon.health}`;
      return {
        value: daemon.id,
        label: daemon.id,
        disabled: reason !== "",
        ...(reason === ""
          ? { indicator: { tone: "success" as const, title: "Connected" } }
          : { indicator: { tone: "danger" as const, title: reason } }),
      };
    }),
  );

  const repoFallbackLabel = $derived(
    reposLoading ? "Loading repositories…" : "No tracked repositories yet",
  );

  const canSubmit = $derived(
    source === "repository"
      ? selected !== null
      : selectedKataReference !== null && selectedDaemonUsable,
  );

  function chooseDaemon(daemonID: string): void {
    clearPendingLaunch();
    selectedDaemonID = daemonID;
    scheduleKataSearch();
  }

  function scheduleKataSearch(): void {
    clearPendingLaunch();
    cancelKataSearch();
    const generation = ++kataSearchGeneration;
    kataReferences = [];
    selectedKataReference = null;
    error = null;
    if (!selectedDaemonUsable || kataQuery.trim() === "") {
      kataSearching = false;
      return;
    }
    kataSearchTimer = setTimeout(() => {
      kataSearchTimer = null;
      void runKataSearch(generation);
    }, 250);
  }

  async function runKataSearch(generation: number): Promise<void> {
    const daemonID = selectedDaemonID;
    const query = kataQuery.trim();
    if (daemonID === "" || query === "" || generation !== kataSearchGeneration) return;
    const controller = new AbortController();
    kataSearchController = controller;
    kataSearching = true;
    try {
      const references = await searchKataReferences(daemonID, query, controller.signal);
      if (generation !== kataSearchGeneration || controller.signal.aborted) return;
      kataReferences = references;
    } catch (cause) {
      if (generation !== kataSearchGeneration || controller.signal.aborted) return;
      error = cause instanceof Error ? cause.message : "Unable to search Kata issues.";
    } finally {
      if (generation === kataSearchGeneration) kataSearching = false;
      if (kataSearchController === controller) kataSearchController = null;
    }
  }

  // Branch conflicts are recognized by the stable problem code and read from
  // typed `details`, never from prose or the per-field huma error array.
  function suggestedBranchFrom(requestError: ProblemBody | undefined): string | null {
    if (requestError?.code !== "branchConflict") return null;
    const value = requestError.details?.["suggestedBranch"];
    return typeof value === "string" && value ? value : null;
  }

  function submit(selectedTargetKey?: string): void {
    if (selectedTargetKey !== undefined) {
      pendingLaunchTargetKey = selectedTargetKey;
    }
    const launchTargetKey = pendingLaunchTargetKey;
    if (submitting) return;
    const repo = selected;
    const kataReference = selectedKataReference;
    const requestedSource = source;
    const daemonID = selectedDaemonID;
    if (requestedSource === "repository" && !repo) {
      error = "Pick a repository.";
      return;
    }
    if (requestedSource === "kata_issue" && (!kataReference || !selectedDaemonUsable)) {
      error = "Pick a Kata issue.";
      return;
    }
    const requested = branch.trim();
    // Escape and backdrop clicks dismiss the dialog even mid-request, and a
    // reopen starts a new form; either way this create must stop influencing
    // the UI once its own dialog session is gone.
    const session = activeSession;
    if (session === null || !open) return;
    const kataWorkspaceIdentity = requestedSource === "kata_issue" && kataReference
      ? { daemonID, issueUID: kataReference.uid }
      : null;
    if (kataWorkspaceIdentity) {
      const created = createdKataWorkspaceRef(kataWorkspaceIdentity);
      if (created) {
        if (launchTargetKey) queueWorkspaceLaunch(created.id, launchTargetKey, remoteWorkspaceHostKey);
        pendingLaunchTargetKey = null;
        onClose();
        if (onCreated) onCreated(created.id, remoteWorkspaceHostKey);
        else navigate(`/terminal/${created.id}`);
        return;
      }
      if (!beginKataWorkspaceCreate(kataWorkspaceIdentity)) return;
    }
    error = null;
    suggestedBranch = null;
    submitting = true;
    const createProgram = requestedSource === "repository" && repo
      ? (() => {
          const ref = {
            provider: repo.provider,
            platformHost: repo.platformHost,
            owner: repo.owner,
            name: repo.name,
            repoPath: `${repo.owner}/${repo.name}`,
          };
          const routeParams = providerRouteParams(ref);
          const body = requested ? { branch: requested } : {};
          if (remoteWorkspaceHostKey) {
            return executeOpaqueGeneratedApiRequest("create fleet workspace", (client, signal) =>
              providerUsesHostRoute(ref)
                ? client.FleetService.createFleetRepoWorkspaceOnPlatformHost(
                    { hostKey: remoteWorkspaceHostKey, ...providerHostRouteParams(ref) },
                    body,
                    { signal },
                  )
                : client.FleetService.createFleetRepoWorkspace(
                    { hostKey: remoteWorkspaceHostKey, ...routeParams },
                    body,
                    { signal },
                  ),
            ).pipe(Effect.map(normalizeCreatedWorkspace));
          }
          return executeGeneratedApiRequest("create workspace", (client, signal) =>
            providerUsesHostRoute(ref)
              ? client.WorkspacesService.createRepoWorkspaceOnHost(providerHostRouteParams(ref), body, { signal })
              : client.WorkspacesService.createRepoWorkspace(routeParams, body, { signal }),
          ).pipe(Effect.map(normalizeCreatedWorkspace));
        })()
      : Effect.tryPromise({
          try: () => createOrOpenKataWorkspace({
            daemon_id: daemonID,
            issue_uid: kataReference!.uid,
            project_uid: kataReference!.project_uid,
            ...(kataReference!.project_name ? { project_name: kataReference!.project_name } : {}),
            ...(kataReference!.short_id ? { short_id: kataReference!.short_id } : {}),
            ...(kataReference!.qualified_id ? { qualified_id: kataReference!.qualified_id } : {}),
            ...(kataReference!.title ? { title: kataReference!.title } : {}),
          }),
          catch: (cause) => cause,
        }).pipe(Effect.map(normalizeCreatedWorkspace));
    const program = createProgram.pipe(
      Effect.tap((created) =>
        Effect.sync(() => {
          if (!kataWorkspaceIdentity || !created?.id) return;
          recordKataWorkspaceCreated(kataWorkspaceIdentity, {
            id: created.id,
            status: created.status ?? "provisioning",
          });
        }),
      ),
      Effect.flatMap((created) =>
        created?.id
          ? Effect.succeed(created.id)
          : Effect.fail(
              InvalidExternalPayload.make({
                operation: "decode create workspace response",
                cause: created,
              }),
            ),
      ),
      Effect.tap((workspaceId) =>
        Effect.sync(() => {
          if (launchTargetKey) queueWorkspaceLaunch(workspaceId, launchTargetKey, remoteWorkspaceHostKey);
          // The workspace exists either way, so it stays the last-used repo; only
          // the navigation is abandoned when the user moved on.
          if (requestedSource === "repository" && repo) rememberNewWorkspaceRepoKey(repo.key);
        }),
      ),
      Effect.tap((workspaceId) =>
        Effect.sync(() => {
          if (!open || activeSession !== session) return;
          pendingLaunchTargetKey = null;
          onClose();
          if (onCreated) onCreated(workspaceId, remoteWorkspaceHostKey);
          else if (remoteWorkspaceHostKey) {
            navigate(`/terminal/fleet/${encodeURIComponent(remoteWorkspaceHostKey)}/${encodeURIComponent(workspaceId)}`);
          } else {
            navigate(`/terminal/${encodeURIComponent(workspaceId)}`);
          }
        }),
      ),
      Effect.ensuring(
        Effect.sync(() => {
          if (kataWorkspaceIdentity) endKataWorkspaceCreate(kataWorkspaceIdentity);
          // A stale create must not re-enable the form under a newer one that is
          // still in flight; each open resets this flag for its own session.
          if (open && activeSession === session) submitting = false;
        }),
      ),
    );
    runtime.runCommand(program, {
      operation: "create workspace",
      safeContext: {
        source: requestedSource,
        ...(repo ? {
          provider: repo.provider,
          platformHost: repo.platformHost,
          owner: repo.owner,
          name: repo.name,
          remote: remoteWorkspaceHostKey !== undefined,
        } : { daemonId: daemonID }),
      },
      onFailure: (failure) => {
        if (!open || activeSession !== session) return;
        if (failure instanceof ApiProblemError) {
          suggestedBranch = suggestedBranchFrom(failure.problem);
          error = apiErrorMessage(failure.problem, "Could not create workspace");
          return;
        }
        error = requestedSource === "repository"
          ? "Could not create workspace"
          : failure instanceof Error
            ? failure.message
            : "Could not create or open Kata workspace";
      },
    });
  }

  function useSuggestedBranch(): void {
    if (!suggestedBranch) return;
    branch = suggestedBranch;
    suggestedBranch = null;
    error = null;
  }
</script>

<Modal {open} title="New workspace" width={440} frameId="new-workspace" {onClose}>
  <form
    class="new-workspace-form"
    onsubmit={(event) => {
      event.preventDefault();
      submit();
    }}
  >
    <div class="source-picker" role="group" aria-label="Workspace source">
      <button
        type="button"
        class={["source-button", { "source-button--active": source === "repository" }]}
        aria-pressed={source === "repository"}
        disabled={submitting}
        onclick={() => chooseSource("repository")}
      >
        Repository
      </button>
      <button
        type="button"
        class={["source-button", { "source-button--active": source === "kata_issue" }]}
        aria-pressed={source === "kata_issue"}
        disabled={submitting}
        onclick={() => chooseSource("kata_issue")}
      >
        Kata issue
      </button>
    </div>

    {#if source === "repository"}
      <div class="field repo-field">
        <span class="field-label">Repository</span>
        <Typeahead
          options={repoRows}
          value={selectedKey}
          fallbackLabel={repoFallbackLabel}
          placeholder="Filter repositories"
          title="Repository"
          emptyLabel="No repositories match"
          loading={reposLoading}
          error={reposError ?? ""}
          disabled={submitting}
          onselect={(value) => {
            clearPendingLaunch();
            selectedKey = value;
            reposError = null;
          }}
        />
      </div>

      <label class="field">
        <span class="field-label">Branch</span>
        <TextInput
          bind:value={branch}
          block
          placeholder="(generated when empty)"
          ariaLabel="Branch name"
          disabled={submitting}
          oninput={clearPendingLaunch}
        />
        <small class="field-hint">
          <GitBranchIcon size="11" strokeWidth="2" aria-hidden="true" />
          Branches from the repository's default branch in a new worktree.
        </small>
      </label>

      {#if workspaceHostOptions.length > 1}
        <label class="field">
          <span class="field-label">Run on</span>
          <SelectDropdown
            value={selectedWorkspaceHostKey}
            options={workspaceHostOptions}
            onchange={(hostKey) => {
              clearPendingLaunch();
              selectedWorkspaceHostKey = hostKey;
            }}
            title="Workspace machine"
            disabled={submitting}
          />
          <small class="field-hint">The selected Forge owns the worktree and runtime sessions.</small>
        </label>
      {/if}
    {:else}
      <label class="field">
        <span class="field-label">Kata daemon</span>
        {#if kataDaemons.loading()}
          <span class="field-message">Loading daemons…</span>
        {:else if daemonOptions.length > 0}
          <SelectDropdown
            value={selectedDaemonID}
            options={daemonOptions}
            onchange={chooseDaemon}
            title="Kata daemon"
            disabled={submitting}
          />
        {:else}
          <span class="field-message">{kataDaemons.error() ?? "No Kata daemons configured."}</span>
        {/if}
      </label>

      <div class="field">
        <span class="field-label">Issue</span>
        <SearchInput
          bind:value={kataQuery}
          block
          ariaLabel="Search Kata issues"
          placeholder="Search by reference or title"
          disabled={!selectedDaemonUsable || submitting}
          oninput={scheduleKataSearch}
        />
      </div>

      {#if selectedDaemonProblem}
        <p class="form-error" role="alert">{selectedDaemonProblem}</p>
      {/if}

      {#if kataSearching}
        <p class="field-message" role="status">Searching…</p>
      {:else if kataQuery.trim() !== "" && kataReferences.length === 0 && !error}
        <p class="field-message">No matching Kata issues.</p>
      {:else if kataReferences.length > 0}
        <div class="kata-results" aria-label="Kata issue results">
          {#each kataReferences as reference (`${reference.project_uid}:${reference.uid}`)}
            <button
              type="button"
              class={["kata-result", {
                "kata-result--selected": selectedKataReference?.project_uid === reference.project_uid &&
                  selectedKataReference?.uid === reference.uid,
              }]}
              aria-pressed={selectedKataReference?.project_uid === reference.project_uid &&
                selectedKataReference?.uid === reference.uid}
              disabled={submitting}
              onclick={() => {
                clearPendingLaunch();
                selectedKataReference = reference;
              }}
            >
              <strong>{reference.qualified_id || reference.short_id}</strong>
              <span>{reference.title}</span>
              <small>{reference.project_name} · {reference.status}</small>
            </button>
          {/each}
        </div>
      {/if}
    {/if}

    {#if error}
      <p class="form-error" role="alert">
        {error}
        {#if suggestedBranch}
          <Button size="sm" onclick={useSuggestedBranch}>
            Use {suggestedBranch}
          </Button>
        {/if}
      </p>
    {/if}

    <div class="form-actions">
      <Button onclick={onClose} disabled={submitting}>Cancel</Button>
      <WorkspaceCreateSplitButton
        label={source === "repository" ? "Create workspace" : "Create or open workspace"}
        busyLabel={source === "repository" ? "Creating…" : "Opening workspace…"}
        launchTargets={settings.getLaunchTargets()}
        busy={submitting || kataWorkspaceCreatePending}
        disabled={!canSubmit || kataWorkspaceCreatePending}
        disabledReason={source === "repository" ? "Pick a repository." : "Pick a Kata issue."}
        surface="solid"
        primaryType="submit"
        onCreate={submit}
      />
    </div>
  </form>
</Modal>

<style>
  .new-workspace-form {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .source-picker {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 4px;
    padding: 3px;
    border-radius: var(--radius-md);
    background: var(--bg-subtle);
  }

  .source-button {
    min-height: 30px;
    border: 1px solid transparent;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-muted);
    font: inherit;
    font-size: var(--font-size-sm);
    cursor: pointer;
  }

  .source-button:hover:not(:disabled) { color: var(--text-primary); }

  .source-button--active {
    border-color: var(--border-default);
    background: var(--bg-surface);
    color: var(--text-primary);
    font-weight: 600;
  }

  .source-button:disabled { cursor: not-allowed; opacity: var(--opacity-disabled); }

  /* The picker defaults to a 300px cap, which reads as a misaligned control
     next to the full-width branch input. */
  .repo-field {
    --typeahead-min-width: 0;
    --typeahead-max-width: 100%;
  }

  .field-label {
    font-size: var(--font-size-xs);
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }

  .field-hint {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .field-message {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .kata-results {
    display: grid;
    max-height: 220px;
    overflow-y: auto;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
  }

  .kata-result {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr);
    gap: 2px 8px;
    padding: 8px 10px;
    border: 0;
    border-bottom: 1px solid var(--border-muted);
    background: transparent;
    color: var(--text-primary);
    text-align: left;
    cursor: pointer;
  }

  .kata-result:last-child { border-bottom: 0; }
  .kata-result:hover:not(:disabled),
  .kata-result--selected { background: var(--bg-hover); }
  .kata-result strong,
  .kata-result small { white-space: nowrap; }
  .kata-result span {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .kata-result small {
    grid-column: 1 / -1;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .form-error {
    display: flex;
    align-items: center;
    gap: 8px;
    margin: 0;
    padding: 6px 8px;
    background: var(--bg-error-subtle, #ffebe9);
    color: var(--text-error, #cf222e);
    border-radius: var(--radius-sm);
    font-size: var(--font-size-xs);
  }

  .form-actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    margin-top: 4px;
  }
</style>

<script lang="ts">
  import { Effect } from "effect";
  import { onDestroy, tick, untrack } from "svelte";
  import type { ApiProblemError, TransientTransportError } from "../../api/effect-errors.js";
  import { executeGeneratedApiRequest } from "../../api/generated-api.js";
  import { retryIdempotentRead } from "../../api/retry-policy.js";
  import type { ProblemBody } from "../../api/problems.js";
  import type { AppExecution } from "../../app/runtime.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { transientClipboardFeedback } from "../../browser/clipboard-feedback.js";
  import { canonicalProvider, providerItemPath, providerRepoPath, providerRouteParams, resolvedPlatformHost, providerHostRouteParams, providerUsesHostRoute } from "../../api/provider-routes.js";
  import type { IssueDetail, Label, ProviderCapabilities } from "../../api/types.js";
  import {
    getStores, getActions,
    getUIConfig, getNavigate,
  } from "../../context.js";
  import { pushModalFrame } from "../../stores/keyboard/modal-stack.svelte.js";
  import { showFlash } from "../../stores/flash.svelte.js";
  import type { IssueDetailSyncMode } from "../../stores/issues.svelte.js";
  import type { MutationCallbacks } from "../../stores/ordered-mutations.js";
  import MarkdownHtml from "../shared/MarkdownHtml.svelte";
  import { moveTaskListItem, toggleTaskListItem } from "../../utils/task-list.js";
  import { firstUnavailableGate, operationGate } from "./operation-gates.js";
  import { copyToClipboard, formatRelativeTime, StatusDot } from "@kenn-io/kit-ui";
  import EventTimeline from "./EventTimeline.svelte";
  import CollapsibleDescription from "./CollapsibleDescription.svelte";
  import DetailActivityViewMenu from "./DetailActivityViewMenu.svelte";
  import DetailRefreshButton from "./DetailRefreshButton.svelte";
  import IssueCommentBox from "./IssueCommentBox.svelte";
  import WorkspaceCreateSplitButton from "../workspace/WorkspaceCreateSplitButton.svelte";
    import { AdaptiveActionGrid, Button, Chip, Modal } from "@kenn-io/kit-ui";
  import { Spinner } from "@kenn-io/kit-ui";
  import LabelRow from "../shared/LabelRow.svelte";
  import { ScrollBox } from "@kenn-io/kit-ui";
  import LabelPicker from "./LabelPicker.svelte";
  import UserListEditor from "./UserListEditor.svelte";
  import { loadLabelCatalogWithRefresh } from "./labelCatalogRefresh.js";
  import {
    labelPickerCommandMatches,
    OPEN_LABEL_PICKER_EVENT,
    type OpenLabelPickerDetail,
  } from "./labelPickerCommand.js";
  import { nextCatalogLabels } from "./labelSelection.js";
  import { floatingPopoverStyle } from "@kenn-io/kit-ui";
  import CopyItemNumber from "./CopyItemNumber.svelte";
  import ExternalLinkIcon from "@lucide/svelte/icons/external-link";
  import MonitorUpIcon from "@lucide/svelte/icons/monitor-up";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import TagsIcon from "@lucide/svelte/icons/tags";
  import UsersIcon from "@lucide/svelte/icons/users";
  import XIcon from "@lucide/svelte/icons/x";
  import { identityEquals, type InlineWorkspaceController, type WorkspaceItemIdentity } from "../../workspace-inline.js";
  import {
    beginWorkspaceCreate,
    endWorkspaceCreate,
    isWorkspaceCreatePending,
    promoteWorkspaceCreateLaunch,
    reconcileWorkspaceCreated,
    recordWorkspaceCreated,
    resolveControllerlessWorkspaceRef,
  } from "../../stores/workspace-create-pending.svelte.js";

  const CLEAR_LABELS_PENDING = "__clear-label-selection__";

  const { issues, activity, detailActivityView, settings } = getStores();
  const runtime = getAppRuntime();
  const actions = getActions();
  const uiConfig = getUIConfig();
  const navigate = getNavigate();
  let manualRefreshPending = $state(false);
  let manualRefreshGeneration = 0;

  function isCurrentManualRefresh(
    requestGeneration: number,
    requestIdentity: WorkspaceItemIdentity,
  ): boolean {
    return !componentDestroyed
      && requestGeneration === manualRefreshGeneration
      && identityEquals(requestIdentity, $state.snapshot(itemIdentity));
  }

  function refreshDetail(): void {
    if (
      manualRefreshPending
      || issues.isIssueDetailLoading()
      || issues.isIssueDetailSyncing()
      || staleIssue
    ) return;
    const requestIdentity = $state.snapshot(itemIdentity);
    const requestGeneration = ++manualRefreshGeneration;
    manualRefreshPending = true;
    issues.syncIssueDetailNow(
      owner,
      name,
      number,
      { provider, platformHost, repoPath },
      {
        onFailure: (message) => {
          if (isCurrentManualRefresh(requestGeneration, requestIdentity)) {
            showFlash(message, { tone: "danger" });
          }
        },
        onSettled: () => {
          if (isCurrentManualRefresh(requestGeneration, requestIdentity)) {
            manualRefreshPending = false;
          }
        },
      },
    );
  }

  const defaultProviderCapabilities: ProviderCapabilities = {
    read_repositories: true,
    read_merge_requests: true,
    read_issues: true,
    read_issue_pr_references: false,
    read_comments: true,
    read_releases: true,
    read_ci: true,
    read_workflows: false,
    read_workflow_runs: false,
    workflow_dispatch: false,
    read_labels: false,
    read_markdown_images: false,
    read_authenticated_user: false,
    comment_mutation: true,
    state_mutation: true,
    merge_mutation: true,
    review_mutation: true,
    workflow_approval: true,
    ready_for_review: true,
    draft_mutation: true,
    issue_mutation: true,
    label_mutation: false,
    assignee_mutation: false,
    reviewer_mutation: false,
    thread_reply: false,
    thread_resolve: false,
    review_draft_mutation: false,
    review_thread_resolution: false,
    review_suggestion_application: false,
    read_review_threads: false,
    native_multiline_ranges: false,
    mutation_head_binding: false,
    supported_review_actions: [],
  };

  function currentCapabilities(): ProviderCapabilities {
    return issues.getIssueDetail()?.repo?.capabilities
      ?? defaultProviderCapabilities;
  }

  interface Props {
    owner: string;
    name: string;
    number: number;
    provider: string;
    platformHost?: string | undefined;
    repoPath: string;
    hideStaleWhileLoading?: boolean;
    autoSync?: IssueDetailSyncMode;
    onOpenWorkspace?: ((workspaceId: string) => void) | undefined;
    onViewWorkspaces?: (() => void) | undefined;
    inlineWorkspace?: InlineWorkspaceController | null;
  }

  const {
    owner,
    name,
    number,
    provider,
    platformHost,
    repoPath,
    hideStaleWhileLoading = false,
    autoSync = "background",
    onOpenWorkspace,
    onViewWorkspaces,
    inlineWorkspace = null,
  }: Props = $props();

  const routeRef = $derived({
    provider,
    platformHost,
    owner,
    name,
    repoPath,
  });
  const labelPickerCommandRef = $derived({
    itemType: "issue" as const,
    provider,
    platformHost,
    owner,
    name,
    repoPath,
    number,
  });
  const itemIdentity = $derived<WorkspaceItemIdentity>({
    provider,
    platformHost,
    owner,
    name,
    repoPath,
    number,
    itemType: "issue",
  });
  const descriptionItemKey = $derived(
    `${canonicalProvider(provider)}:${resolvedPlatformHost(provider, platformHost)}:${owner}/${name}:issue:${number}`,
  );

  // See PullDetail.svelte: while a route change is in flight, the
  // displayed issue may briefly belong to the previous route. Mutating
  // actions (state change, workspace create, etc.) read the props,
  // which point at the new route — so they must be gated until the
  // displayed issue catches up.
  const staleIssue = $derived.by(() => {
    const d = issues.getIssueDetail();
    if (d == null) return false;
    if (
      d.repo_owner !== owner ||
      d.repo_name !== name ||
      (d.issue?.Number ?? -1) !== number
    ) {
      return true;
    }
    // Props may carry provider aliases (gh) or omit the default host
    // (Activity URLs) while the payload is canonical and concrete;
    // treating those as stale would disable every mutation action on a
    // detail that is in fact current.
    return canonicalProvider(d.repo?.provider ?? "") !== canonicalProvider(provider)
      || d.repo?.repo_path !== repoPath
      || resolvedPlatformHost(provider, d.repo?.platform_host)
        !== resolvedPlatformHost(provider, platformHost);
  });

  // Same comparison shape as PRListView's detailMatchesSelected, but
  // against the inline workspace identity rather than a route ref: the
  // reconcile effect below must not fire the override reconciliation for
  // a detail payload that belongs to a different issue (e.g. one still
  // in-flight from a prior route).
  function detailMatchesIdentity(
    detail: IssueDetail,
    identity: WorkspaceItemIdentity,
  ): boolean {
    return (
      detail.repo_owner === identity.owner &&
      detail.repo_name === identity.name &&
      detail.issue.Number === identity.number &&
      canonicalProvider(detail.repo?.provider ?? "") === canonicalProvider(identity.provider) &&
      resolvedPlatformHost(identity.provider, detail.repo?.platform_host) ===
        resolvedPlatformHost(identity.provider, identity.platformHost) &&
      detail.repo?.repo_path === identity.repoPath
    );
  }

  function editTimelineComment(
    event: { PlatformID: number | null },
    body: string,
    callbacks: MutationCallbacks,
  ): void {
    if (staleIssue || event.PlatformID === null) {
      callbacks.onFailure?.(
        staleIssue ? "Refresh issue details before editing a comment" : "Comment identifier is unavailable",
      );
      callbacks.onSettled?.();
      return;
    }
    issues.editIssueComment(owner, name, number, event.PlatformID, body, callbacks);
  }

  function deleteTimelineComment(event: { PlatformID: number | null }, callbacks: MutationCallbacks): void {
    if (staleIssue || event.PlatformID === null) {
      callbacks.onFailure?.(
        staleIssue ? "Refresh issue details before deleting a comment" : "Comment identifier is unavailable",
      );
      callbacks.onSettled?.();
      return;
    }
    issues.deleteIssueComment(owner, name, number, event.PlatformID, callbacks);
  }

  let lastDetailLoadIdentity: WorkspaceItemIdentity | null = null;
  let lastDetailLoadAutoSync: IssueDetailSyncMode | undefined;

  $effect(() => {
    const requestOwner = owner;
    const requestName = name;
    const requestNumber = number;
    const requestProvider = provider;
    const requestPlatformHost = platformHost;
    const requestRepoPath = repoPath;
    const requestAutoSync = autoSync;
    const requestIdentity = $state.snapshot(itemIdentity);
    const shouldLoad =
      lastDetailLoadIdentity === null
      || !identityEquals(lastDetailLoadIdentity, requestIdentity)
      || lastDetailLoadAutoSync !== requestAutoSync;
    if (shouldLoad) {
      lastDetailLoadIdentity = requestIdentity;
      lastDetailLoadAutoSync = requestAutoSync;
    }
    untrack(() => {
      if (shouldLoad) {
        issues.loadIssueDetail(
          requestOwner,
          requestName,
          requestNumber,
          {
            sync: requestAutoSync,
            provider: requestProvider,
            platformHost: requestPlatformHost,
            repoPath: requestRepoPath,
          },
        );
      }
      issues.startIssueDetailPolling(
        requestOwner,
        requestName,
        requestNumber,
        {
          provider: requestProvider,
          platformHost: requestPlatformHost,
          repoPath: requestRepoPath,
        },
      );
    });
    return () => issues.stopIssueDetailPolling();
  });

  $effect(() => {
    const handler = (event: Event) => onOpenLabelPickerCommand(event);
    window.addEventListener(OPEN_LABEL_PICKER_EVENT, handler);
    return () => window.removeEventListener(OPEN_LABEL_PICKER_EVENT, handler);
  });

  // Clear conflict/error state on route change so issue A's
  // dialogs can't bleed into issue B's view. Keyed on the full
  // provider-aware identity: the same owner/name/number can exist
  // on another provider or host.
  //
  // Tracks the last identity this effect reset for: a route transition can
  // re-express the same item (gh vs github, omitted vs concrete default
  // host), and an alias-only change must not bump the generation — that
  // would discard an in-flight create's success and re-enable the button
  // for a duplicate request.
  let lastResetIdentity: WorkspaceItemIdentity | null = null;
  $effect(() => {
    const current = $state.snapshot(itemIdentity);
    if (lastResetIdentity !== null && identityEquals(lastResetIdentity, current)) return;
    lastResetIdentity = current;
    manualRefreshGeneration += 1;
    manualRefreshPending = false;
    workspaceRequestGen += 1;
    branchConflict = null;
    pendingWorkspaceLaunchTarget = null;
    workspaceCreating = false;
    closeLabelPicker();
  });

  let labelPickerOpen = $state(false);
  let labelCatalog = $state<Label[]>([]);
  let labelCatalogSyncing = $state(false);
  let labelPickerError = $state<string | null>(null);
  let pendingLabel = $state<string | null>(null);
  let labelPickerAnchor = $state<HTMLDivElement>();
  let labelPickerPopover = $state<HTMLDivElement>();
  let labelPickerAutofocusFilter = $state(false);
  let labelPickerStyle = $state("");
  let labelCatalogExecution: AppExecution<void, ApiProblemError | TransientTransportError> | null = null;
  let labelCatalogGeneration = 0;

  function closeLabelPicker(): void {
    labelCatalogGeneration += 1;
    labelCatalogExecution?.interrupt();
    labelCatalogExecution = null;
    labelPickerOpen = false;
    labelPickerError = null;
    pendingLabel = null;
    labelPickerAutofocusFilter = false;
  }

  function positionLabelPicker(): void {
    if (!labelPickerAnchor) return;
    const popoverHeight = labelPickerPopover?.getBoundingClientRect().height;
    labelPickerStyle = floatingPopoverStyle({
      trigger: labelPickerAnchor.getBoundingClientRect(),
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      ...(popoverHeight !== undefined ? { popoverHeight } : {}),
      align: "end",
      edgeGap: 12,
      maxWidth: 360,
      constrainWidth: true,
    });
  }

  function onOpenLabelPickerCommand(event: Event): void {
    const detail = (event as CustomEvent<OpenLabelPickerDetail>).detail;
    if (!labelPickerCommandMatches(labelPickerCommandRef, detail)) return;
    // While the inline dock is expanded this detail stays mounted but
    // hidden/inert, so opening the picker here would build an invisible
    // overlay that pops up on the next collapse. The command is an explicit
    // detail operation: restore split first so the picker lands visibly.
    if (inlineWorkspace?.isClaimedFor(itemIdentity) && inlineWorkspace.getDockMode() === "expanded") {
      inlineWorkspace.setDockMode("split");
    }
    openLabelPicker();
  }

  function openLabelPicker(event?: MouseEvent): void {
    if (labelGate.unavailable) return;
    labelPickerAnchor = (event?.currentTarget as HTMLElement | null)?.closest<HTMLDivElement>(".label-editor-anchor")
      ?? labelPickerAnchor;
    if (event !== undefined && labelPickerOpen) {
      closeLabelPicker();
      return;
    }
    labelPickerAutofocusFilter = event !== undefined && !(window.matchMedia?.("(pointer: coarse)").matches ?? false);
    labelPickerOpen = true;
    labelPickerError = null;
    labelCatalogSyncing = true;
    const generation = ++labelCatalogGeneration;
    labelCatalogExecution?.interrupt();
    const selectedRef = {
      provider: routeRef.provider,
      platformHost: routeRef.platformHost,
      owner: routeRef.owner,
      name: routeRef.name,
      repoPath: routeRef.repoPath,
    };
    const selectedNumber = number;
    const isCurrent = () =>
      labelPickerOpen &&
      labelCatalogGeneration === generation &&
      canonicalProvider(selectedRef.provider) === canonicalProvider(routeRef.provider) &&
      resolvedPlatformHost(selectedRef.provider, selectedRef.platformHost) ===
        resolvedPlatformHost(routeRef.provider, routeRef.platformHost) &&
      selectedRef.owner === routeRef.owner &&
      selectedRef.name === routeRef.name &&
      selectedRef.repoPath === routeRef.repoPath &&
      selectedNumber === number;
    const program = Effect.gen(function* () {
      yield* Effect.promise(() => tick());
      yield* Effect.sync(positionLabelPicker);
      yield* loadLabelCatalogWithRefresh({
        isActive: isCurrent,
        loadOnce: executeGeneratedApiRequest("GET issue label catalog", (client, signal) =>
          providerUsesHostRoute(selectedRef) ? client.RepositoriesService.listRepoLabelsOnHost({ ...providerHostRouteParams(selectedRef) }, { signal }) : client.RepositoriesService.listRepoLabels({ ...providerRouteParams(selectedRef) }, { signal }),
        ).pipe(
          Effect.map((data) => ({
            labels: data.labels ?? [],
            stale: data.stale ?? false,
            syncing: data.syncing ?? false,
          })),
        ),
        onUpdate: (catalog) => Effect.gen(function* () {
          yield* Effect.sync(() => {
            if (!isCurrent()) return;
            labelCatalog = catalog.labels;
            labelCatalogSyncing = Boolean(catalog.stale || catalog.syncing);
          });
          yield* Effect.promise(() => tick());
          yield* Effect.sync(() => {
            if (isCurrent()) positionLabelPicker();
          });
        }),
      });
    }).pipe(
      Effect.ensuring(Effect.sync(() => {
        if (isCurrent()) labelCatalogSyncing = false;
      })),
    );
    labelCatalogExecution = runtime.runCommand(program, {
      operation: "load issue label catalog",
      safeContext: { owner: selectedRef.owner, name: selectedRef.name },
      onFailure: (failure) => {
        if (!isCurrent()) return;
        labelPickerError = failure._tag === "ApiProblemError"
          ? failure.problem.detail ?? failure.problem.title ?? "failed to load labels"
          : "Could not reach Kenn Forge";
      },
    });
  }

  $effect(() => {
    if (!labelPickerOpen) return;

    function updatePosition(): void {
      positionLabelPicker();
    }

    window.addEventListener("resize", updatePosition);
    window.addEventListener("scroll", updatePosition, true);
    return () => {
      window.removeEventListener("resize", updatePosition);
      window.removeEventListener("scroll", updatePosition, true);
    };
  });

  function toggleLabel(labelName: string): void {
    if (pendingLabel !== null || labelGate.unavailable) return;
    const currentLabels = issues.getIssueDetail()?.issue.labels ?? [];
    pendingLabel = labelName;
    labelPickerError = null;
    const nextLabels = nextCatalogLabels(currentLabels, labelCatalog, labelName);
    issues.setIssueLabels(owner, name, number, nextLabels, {
      onFailure: (message) => {
        labelPickerError = message;
      },
      onSettled: () => {
        pendingLabel = null;
      },
    });
  }

  function clearLabels(): void {
    if (labelGate.unavailable) return;
    if (pendingLabel !== null) return;
    const currentLabels = issues.getIssueDetail()?.issue.labels ?? [];
    if (currentLabels.length === 0) return;
    pendingLabel = CLEAR_LABELS_PENDING;
    labelPickerError = null;
    issues.setIssueLabels(owner, name, number, [], {
      onFailure: (message) => {
        labelPickerError = message;
      },
      onSettled: () => {
        pendingLabel = null;
      },
    });
  }

  function loadUserCandidates(query: string) {
    return executeGeneratedApiRequest("GET issue user candidates", (client, signal) =>
      providerUsesHostRoute(routeRef) ? client.RepositoriesService.getCommentAutocompleteOnHost({ ...providerHostRouteParams(routeRef) }, { trigger: "@", q: query, limit: 25 }, { signal }) : client.RepositoriesService.getCommentAutocomplete({ ...providerRouteParams(routeRef) }, { trigger: "@", q: query, limit: 25 }, { signal }),
    ).pipe(
      retryIdempotentRead,
      Effect.map((data) => data.users ?? []),
      Effect.mapError((failure) =>
        new Error(
          failure._tag === "ApiProblemError"
            ? failure.problem.detail ?? failure.problem.title ?? "failed to load users"
            : "Could not reach Kenn Forge",
        ),
      ),
    );
  }

  function userAvatarURL(username: string): string {
    if (canonicalProvider(provider) !== "github") return "";
    const login = encodeURIComponent(username.trim());
    const host = issues.getIssueDetail()?.repo?.platform_host
      ?? issues.getIssueDetail()?.platform_host
      ?? platformHost
      ?? "";
    if (login === "" || host === "") return "";
    return `https://${host}/${login}.png?size=40`;
  }

  function onDocumentMousedown(e: MouseEvent): void {
    if (!labelPickerOpen) return;
    const target = e.target as Node;
    if (!labelPickerPopover?.contains(target) && !labelPickerAnchor?.contains(target)) {
      closeLabelPicker();
    }
  }

  function handleStarClick(): void {
    if (staleIssue) return;
    const detail = issues.getIssueDetail();
    if (!detail) return;
    void issues.toggleIssueStar(
      {
        provider,
        platformHost,
        owner,
        name,
        repoPath,
      },
      number,
      detail.issue.Starred,
    );
  }

  let stateSubmitting = $state(false);

  // Per-operation mutation availability from the issue detail payload.
  const repoOperations = $derived(issues.getIssueDetail()?.repo?.operations);
  const addCommentGate = $derived(operationGate(repoOperations?.add_comment));
  const editCommentGate = $derived(operationGate(repoOperations?.edit_comment));
  const deleteCommentGate = $derived(operationGate(repoOperations?.delete_comment));
  const labelGate = $derived(firstUnavailableGate(
    repoOperations?.add_label, repoOperations?.remove_label,
  ));
  const assigneeGate = $derived(operationGate(repoOperations?.set_assignees));
  // Body task-list writes are content edits with their own operation
  // key, so rate limits gate them just like credential failures.
  const contentGate = $derived(operationGate(repoOperations?.update_content));

  function handleStateChange(newState: "open" | "closed"): void {
    if (staleIssue) return;
    if (!currentCapabilities().state_mutation) return;
    stateSubmitting = true;
    issues.setIssueState(routeRef, number, newState, {
      onSuccess: () => activity.loadActivity(),
      onFailure: (message) => showFlash(message, { tone: "danger" }),
      onSettled: () => {
        stateSubmitting = false;
      },
    });
  }

  let workspaceCreating = $state(false);
  let pendingWorkspaceLaunchTarget = $state<string | null>(null);
  // The shared pending store outlives this component and its local flag:
  // route resets and remounts clear workspaceCreating while the POST is
  // still in flight, and a round-trip back to this issue must keep the
  // button disabled or a second click sends a duplicate create.
  const workspaceCreateBlocked = $derived(workspaceCreating || isWorkspaceCreatePending(itemIdentity));
  // Bumped per create request and on identity change (route-reset
  // effect): a workspace-create response whose generation no longer
  // matches arrived for an item this component stopped showing and must
  // not touch any state. Destroy alone is weaker — the same issue can
  // still be selected (drawer tab/layout change), so a success still
  // records its store-level override; only local state, the conflict
  // dialog, flash, and navigation are skipped.
  let workspaceRequestGen = 0;
  let componentDestroyed = false;
  onDestroy(() => {
    labelCatalogGeneration += 1;
    labelCatalogExecution?.interrupt();
    bodyCopyExecution?.interrupt();
    flushBodySave();
    componentDestroyed = true;
  });
  const createWorkspaceTitle =
    "Create an issue worktree, then open Workspaces to launch agents or shells on that branch.";
  const createWorkspaceDescriptionId =
    "issue-create-workspace-description";
  const ISSUE_WORKSPACE_BRANCH_CONFLICT_TYPE =
    "urn:kenn-forge:error:issue-workspace-branch-conflict";

  type BranchConflictState = {
    existingBranch: string;
    suggestedBranch: string;
    branchInput: string;
    existingDirectory: boolean;
    error: string | null;
  };

  let branchConflict = $state<BranchConflictState | null>(
    null,
  );

  $effect(() => {
    if (branchConflict == null) return;
    return untrack(() => pushModalFrame("issue-detail-confirm", []));
  });
  const workspace = $derived(
    staleIssue
      ? null
      : inlineWorkspace
      ? inlineWorkspace.effectiveWorkspaceRef(itemIdentity, issues.getIssueDetail()?.workspace ?? null)
      : // Without a controller there is no override store: the shared
        // resolver stands in — the confirmed created record wins over a
        // stale cached envelope, and an envelope still carrying a
        // session-deleted workspace ID is masked instead of re-offering
        // "Open Workspace" for a workspace that no longer exists.
        resolveControllerlessWorkspaceRef(itemIdentity, issues.getIssueDetail()?.workspace ?? null),
  );
  const workspaceDeletionLifecycle = $derived(
    workspace?.status === "deleting" || workspace?.status === "deletion_failed",
  );

  // Once a detail load lands for the identity this component is currently
  // showing, let the inline controller drop its override if the envelope
  // now agrees (or update its bookkeeping if the envelope disagrees). A
  // load for a stale/mismatched identity must not reconcile — that would
  // let a slow response for issue A clear an override this component
  // just recorded for issue B.
  $effect(() => {
    const detail = issues.getIssueDetail();
    if (!detail) return;
    if (!detailMatchesIdentity(detail, itemIdentity)) return;
    // The shared created-record reconciles the same way, controller or
    // not: an identity-matched envelope that carries the workspace is
    // authoritative, and a null envelope whose request started after the
    // creation was recorded (the tick comparison) is authoritative for
    // absence — another client deleted the workspace. Reading the tick
    // here also reruns this effect when a refreshed envelope's content
    // is identical and the detail object itself is not reassigned.
    const envelopeTick = issues.getIssueDetailEnvelopeTick();
    reconcileWorkspaceCreated(itemIdentity, detail.workspace ?? null, envelopeTick);
    if (inlineWorkspace) inlineWorkspace.reconcile(itemIdentity, detail.workspace ?? null, envelopeTick);
  });

  function issueWorkspaceBranch(): string {
    return `kenn-forge/issue-${number}`;
  }

  function branchConflictValue(
    error: ProblemBody,
    location: string,
  ): string | null {
    const value = error.errors?.find(
      (entry) => entry.location === location,
    )?.value;
    return typeof value === "string" && value
      ? value
      : null;
  }

  function parseBranchConflict(
    error: ProblemBody | undefined,
  ): BranchConflictState | null {
    if (!error) {
      return null;
    }

    const existingBranch =
      branchConflictValue(error, "body.git_head_ref")
      ?? "";
    const suggestedBranch =
      branchConflictValue(
        error,
        "body.suggested_git_head_ref",
      )
      ?? "";
    const isTypedConflict =
      error.type === ISSUE_WORKSPACE_BRANCH_CONFLICT_TYPE;
    if (
      !isTypedConflict
      && (!existingBranch || !suggestedBranch)
    ) {
      return null;
    }

    return {
      existingBranch:
        existingBranch || issueWorkspaceBranch(),
      suggestedBranch:
        suggestedBranch
        || `${existingBranch || issueWorkspaceBranch()}-2`,
      branchInput:
        suggestedBranch
        || `${existingBranch || issueWorkspaceBranch()}-2`,
      existingDirectory:
        error.details?.existingDirectory === true,
      error: null,
    };
  }

  type CreateWorkspaceOptions = {
    gitHeadRef?: string;
    reuseExistingBranch?: boolean;
    reuseExistingDirectory?: boolean;
    fromConflictDialog?: boolean;
    launchTargetKey?: string;
  };

  // Re-checks the identity at call time (not just at the caller's check)
  // before refetching because the selection can move on before this
  // synchronous launcher projects the response.
  function refetchDetailForIdentity(identity: WorkspaceItemIdentity): void {
    if (!identityEquals(identity, $state.snapshot(itemIdentity))) return;
    issues.loadIssueDetail(owner, name, number, {
      provider,
      platformHost,
      repoPath,
    });
  }

  function createWorkspace(
    options: CreateWorkspaceOptions = {},
  ): void {
    if (staleIssue) return;
    const detail = issues.getIssueDetail();
    if (!detail) return;
    const requestIdentity = $state.snapshot(itemIdentity);
    if (!options.fromConflictDialog) {
      pendingWorkspaceLaunchTarget = options.launchTargetKey ?? null;
    }
    const launchTargetKey =
      options.launchTargetKey ?? pendingWorkspaceLaunchTarget ?? undefined;

    if (!options.fromConflictDialog) {
      branchConflict = null;
    } else if (
      branchConflict
      && options.gitHeadRef?.trim() === ""
    ) {
      branchConflict.error =
        "Branch name cannot be empty.";
      return;
    }

    // A create for this item is already in flight somewhere (this
    // instance before a round-trip, or a predecessor before a remount).
    // Checked after the conflict-dialog handling above: a conflict
    // response settles the first request before the dialog can resubmit.
    if (isWorkspaceCreatePending(requestIdentity)) return;
    const requestGen = ++workspaceRequestGen;
    // The identity comparison also covers the microtask gap where props
    // already moved to a new item but the route-reset effect (which bumps
    // the generation) hasn't flushed yet.
    const identityLeft = () =>
      requestGen !== workspaceRequestGen ||
      !identityEquals(requestIdentity, $state.snapshot(itemIdentity));
    const responseIsStale = () => componentDestroyed || identityLeft();
    const selectedRef = {
      provider: requestIdentity.provider,
      platformHost: requestIdentity.platformHost,
      owner: requestIdentity.owner,
      name: requestIdentity.name,
      repoPath: requestIdentity.repoPath,
    };
    const requestBody = {
      ...(options.gitHeadRef ? { git_head_ref: options.gitHeadRef.trim() } : {}),
      ...(options.reuseExistingBranch ? { reuse_existing_branch: true } : {}),
      ...(options.reuseExistingDirectory ? { reuse_existing_directory: true } : {}),
    };

    workspaceCreating = true;
    beginWorkspaceCreate(requestIdentity, launchTargetKey);
    if (branchConflict) {
      branchConflict.error = null;
    }
    const program = executeGeneratedApiRequest("POST issue workspace", (client, signal) =>
      providerUsesHostRoute(selectedRef) ? client.IssuesService.createIssueWorkspaceOnHost({ ...providerHostRouteParams(selectedRef), number: requestIdentity.number }, requestBody, { signal }) : client.IssuesService.createIssueWorkspace({ ...providerRouteParams(selectedRef), number: requestIdentity.number }, requestBody, { signal }),
    ).pipe(
      Effect.flatMap((data) =>
        Effect.sync(() => {
          if (data?.id) {
            // Publish the confirmed creation before any liveness guard: the
            // workspace exists server-side even after navigation or unmount.
            const createdRef = {
              id: data.id,
              status: data.status ?? "provisioning",
            };
            promoteWorkspaceCreateLaunch(requestIdentity, createdRef.id, undefined);
            recordWorkspaceCreated(requestIdentity, createdRef);
            inlineWorkspace?.recordCreated(requestIdentity, createdRef);
          }
          if (responseIsStale()) return;
          pendingWorkspaceLaunchTarget = null;
          if (!data?.id) return;
          if (inlineWorkspace) {
            refetchDetailForIdentity(requestIdentity);
          } else if (onOpenWorkspace) {
            // Hosts without an inline controller (phone-like issue routes)
            // decide where a fresh workspace opens; the desktop terminal
            // route is unusable from a phone shell.
            onOpenWorkspace(data.id);
          } else {
            navigate(`/terminal/${data.id}`);
          }
        }),
      ),
      Effect.ensuring(
        Effect.sync(() => {
          endWorkspaceCreate(requestIdentity);
          if (!responseIsStale()) workspaceCreating = false;
        }),
      ),
    );
    runtime.runCommand(program, {
      operation: "create issue workspace",
      safeContext: {
        provider: requestIdentity.provider,
        owner: requestIdentity.owner,
        name: requestIdentity.name,
        number: requestIdentity.number,
      },
      onFailure: (failure) => {
        if (responseIsStale()) return;
        const problem = failure._tag === "ApiProblemError" ? failure.problem : undefined;
        const conflict = parseBranchConflict(problem);
        if (conflict) {
          if (options.fromConflictDialog && options.reuseExistingBranch && branchConflict) {
            branchConflict.error =
              "This branch is already checked out in another worktree. Create a new branch instead.";
            return;
          }
          branchConflict = conflict;
          return;
        }
        const message = problem?.detail ?? problem?.title ??
          (failure._tag === "ApiProblemError" ? "failed to create workspace" : "Could not reach Kenn Forge");
        if (options.fromConflictDialog && branchConflict) {
          branchConflict.error = message;
          return;
        }
        showFlash(message, { tone: "danger" });
      },
    });
  }

  function closeBranchConflictDialog(): void {
    if (workspaceCreating) return;
    branchConflict = null;
    pendingWorkspaceLaunchTarget = null;
  }

  // Task-list checkbox clicks update the body locally for instant
  // feedback, then debounce a PATCH so a flurry of clicks collapses
  // into a single save. Target and body are captured at schedule
  // time so a route change before the debounce settles can't redirect
  // the save to a different issue or lose the edit.
  type PendingBodySave = {
    owner: string;
    name: string;
    number: number;
    body: string;
    provider: string;
    platformHost?: string | undefined;
    repoPath: string;
  };
  let bodySaveExecution: AppExecution<void, never> | undefined;
  let pendingBodySave: PendingBodySave | null = null;
  const BODY_SAVE_DEBOUNCE_MS = 400;

  function scheduleBodySave(body: string): void {
    pendingBodySave = {
      owner, name, number, body,
      provider, platformHost, repoPath,
    };
    bodySaveExecution?.interrupt();
    bodySaveExecution = runtime.runCommand(
      Effect.sleep(`${BODY_SAVE_DEBOUNCE_MS} millis`).pipe(
        Effect.andThen(
          Effect.sync(() => {
            bodySaveExecution = undefined;
            savePendingBody();
          }),
        ),
      ),
      {
        operation: "debounce issue task body save",
        safeContext: { owner, name, number },
        onFailure: () => {},
      },
    );
  }

  function flushBodySave(): void {
    bodySaveExecution?.interrupt();
    bodySaveExecution = undefined;
    savePendingBody();
  }

  function savePendingBody(): void {
    const target = pendingBodySave;
    pendingBodySave = null;
    if (target === null) return;
    void issues.saveIssueBodyInBackground(
      target.owner, target.name, target.number, target.body,
      {
        provider: target.provider,
        platformHost: target.platformHost,
        repoPath: target.repoPath,
      },
    );
  }

  function onBodyClick(event: MouseEvent): void {
    const target = event.target as HTMLElement | null;
    if (!target) return;
    if (target.tagName !== "INPUT") return;
    if ((target as HTMLInputElement).type !== "checkbox") return;
    const raw = target.getAttribute("data-task-index");
    if (raw === null) return;
    if (staleIssue || !currentCapabilities().state_mutation || contentGate.unavailable) {
      event.preventDefault();
      return;
    }
    const index = parseInt(raw, 10);
    if (Number.isNaN(index)) return;
    const detail = issues.getIssueDetail();
    if (!detail) return;
    const newBody = toggleTaskListItem(detail.issue.Body, index);
    if (newBody === detail.issue.Body) return;
    event.preventDefault();
    issues.setLocalIssueBody(
      provider, platformHost, owner, name, number, newBody,
    );
    scheduleBodySave(newBody);
  }

  // Drag-to-reorder for task-list items. See PullDetail.svelte for the
  // mirror implementation — the only difference is the store getter.
  let dragSourceIndex = $state<number | null>(null);
  let dropTargetIndex = $state<number | null>(null);
  let dropTargetSide = $state<"before" | "after">("before");

  function findTaskItemIndex(el: HTMLElement | null): number | null {
    let cur: HTMLElement | null = el;
    while (cur) {
      if (cur.classList && cur.classList.contains("task-list-item")) {
        const raw = cur.getAttribute("data-task-index");
        if (raw === null) return null;
        const idx = parseInt(raw, 10);
        return Number.isNaN(idx) ? null : idx;
      }
      cur = cur.parentElement;
    }
    return null;
  }

  function onBodyDragStart(event: DragEvent): void {
    if (staleIssue || !currentCapabilities().state_mutation || contentGate.unavailable) return;
    const target = event.target as HTMLElement | null;
    if (!target?.classList?.contains("task-drag-handle")) return;
    const raw = target.getAttribute("data-task-index");
    if (raw === null) return;
    const idx = parseInt(raw, 10);
    if (Number.isNaN(idx)) return;
    dragSourceIndex = idx;
    if (event.dataTransfer) {
      event.dataTransfer.effectAllowed = "move";
      event.dataTransfer.setData("text/plain", String(idx));
    }
  }

  function onBodyDragOver(event: DragEvent): void {
    if (dragSourceIndex === null) return;
    const target = event.target as HTMLElement | null;
    const idx = findTaskItemIndex(target);
    if (idx === null) return;
    event.preventDefault();
    if (event.dataTransfer) event.dataTransfer.dropEffect = "move";
    let li: HTMLElement | null = target;
    while (li && !(li.classList && li.classList.contains("task-list-item"))) {
      li = li.parentElement;
    }
    let side: "before" | "after" = "before";
    if (li) {
      const rect = li.getBoundingClientRect();
      side = event.clientY < rect.top + rect.height / 2
        ? "before"
        : "after";
    }
    dropTargetSide = side;
    dropTargetIndex = idx;
    updateDropIndicatorClasses(
      event.currentTarget as HTMLElement,
      idx,
      side,
    );
  }

  function onBodyDragLeave(event: DragEvent): void {
    const related = event.relatedTarget as HTMLElement | null;
    const body = event.currentTarget as HTMLElement;
    if (!related || !body.contains(related)) {
      dropTargetIndex = null;
      clearDropIndicatorClasses(body);
    }
  }

  function onBodyDrop(event: DragEvent): void {
    const body = event.currentTarget as HTMLElement;
    if (dragSourceIndex === null) {
      clearDragState(body);
      return;
    }
    event.preventDefault();
    const from = dragSourceIndex;
    const to = dropTargetIndex;
    const side = dropTargetSide;
    clearDragState(body);
    if (to === null || to === from) return;
    if (staleIssue || !currentCapabilities().state_mutation || contentGate.unavailable) return;
    const detail = issues.getIssueDetail();
    if (!detail) return;
    let target = to;
    if (from < to && side === "before") target = to - 1;
    else if (from > to && side === "after") target = to + 1;
    if (target === from) return;
    const newBody = moveTaskListItem(detail.issue.Body, from, target);
    if (newBody === detail.issue.Body) return;
    issues.setLocalIssueBody(
      provider, platformHost, owner, name, number, newBody,
    );
    scheduleBodySave(newBody);
  }

  function onBodyDragEnd(event: DragEvent): void {
    clearDragState(event.currentTarget as HTMLElement);
  }

  function updateDropIndicatorClasses(
    root: HTMLElement,
    idx: number,
    side: "before" | "after",
  ): void {
    clearDropIndicatorClasses(root);
    const li = root.querySelector(
      `.task-list-item--interactive[data-task-index="${idx}"]`,
    );
    if (!li) return;
    li.classList.add(
      side === "before" ? "task-drop-before" : "task-drop-after",
    );
  }

  function clearDropIndicatorClasses(root: HTMLElement): void {
    root.querySelectorAll(".task-drop-before").forEach((el) =>
      el.classList.remove("task-drop-before"),
    );
    root.querySelectorAll(".task-drop-after").forEach((el) =>
      el.classList.remove("task-drop-after"),
    );
  }

  function clearDragState(root?: HTMLElement | null): void {
    dragSourceIndex = null;
    dropTargetIndex = null;
    dropTargetSide = "before";
    if (root) clearDropIndicatorClasses(root);
  }

  // Drop any pending checkbox save when navigating to a different
  // issue so a stale toggle doesn't land on the new target. The
  // pending save still fires against the originally-captured target
  // so a fast click + navigate sequence persists.
  $effect(() => {
    void owner;
    void name;
    void number;
    flushBodySave();
    clearDragState();
  });
  // Body-copy feedback is parent-controlled: the kit CopyButton's internal
  // copied state is not observable from CSS, and the reveal-on-hover wrap
  // must keep the button visible for the whole copied window even after
  // the pointer leaves.
  let bodyCopied = $state(false);
  let bodyCopyExecution: AppExecution<void, never> | undefined;
  let bodyCopySeq = 0;

  function copyBody(text: string): void {
    const seq = bodyCopySeq;
    bodyCopyExecution?.interrupt();
    bodyCopyExecution = runtime.runCommand(
      transientClipboardFeedback({
        text,
        write: copyToClipboard,
        isActive: () => !componentDestroyed && seq === bodyCopySeq,
        onCopied: () => {
          bodyCopied = true;
        },
        onExpired: () => {
          bodyCopied = false;
        },
      }),
      {
        operation: "copy issue body",
        safeContext: { owner, name, number },
        onFailure: () => {},
      },
    );
  }

  $effect(() => {
    // The component is reused across item navigation; the copied feedback
    // (and its pending reset timer) belongs to the item it was copied from.
    void [provider, platformHost, owner, name, number];
    bodyCopySeq++;
    bodyCopyExecution?.interrupt();
    bodyCopyExecution = undefined;
    bodyCopied = false;
  });
</script>

<svelte:document onmousedown={onDocumentMousedown} />

{#if issues.isIssueDetailLoading() && (issues.getIssueDetail() === null || (staleIssue && hideStaleWhileLoading))}
  <div class="state-center"><p class="state-msg">Loading...</p></div>
{:else if issues.getIssueDetailError() !== null && (issues.getIssueDetail() === null || (staleIssue && hideStaleWhileLoading))}
  <div class="state-center"><p class="state-msg state-msg--error">Error: {issues.getIssueDetailError()}</p></div>
{:else}
  {@const detail = issues.getIssueDetail()}
  {@const staleLoadError = staleIssue && issues.getIssueDetailError() !== null}
  {#if detail !== null}
    {@const issue = detail.issue}
    {@const labels = issue.labels ?? []}
    {@const capabilities = detail.repo?.capabilities ?? defaultProviderCapabilities}
    <ScrollBox label="Issue conversation">
    <div class="issue-detail">
      <div class="issue-detail-content">
      {#if staleLoadError}
        <div class="detail-load-error" data-testid="detail-load-error">
          Couldn't load this issue: {issues.getIssueDetailError()}
        </div>
      {/if}
      {#if issues.isIssueStaleRefreshing() && !manualRefreshPending}
        <div class="refresh-banner">
          <StatusDot status="working" label="Refreshing issue details" size={5} animated />
          <span aria-hidden="true">Refreshing...</span>
        </div>
      {/if}
      <!-- Header -->
      <div class="detail-header">
        <h2 class="detail-title">{issue.Title}</h2>
        {#if !uiConfig.hideStar && !staleIssue}
          <button
            class="star-btn"
            onclick={handleStarClick}
            title={issue.Starred ? "Unstar" : "Star"}
          >
            {#if issue.Starred}
              <svg class="star-detail-icon star-detail-icon--active" width="16" height="16" viewBox="0 0 16 16" fill="currentColor">
                <path d="M8 .25a.75.75 0 01.673.418l1.882 3.815 4.21.612a.75.75 0 01.416 1.279l-3.046 2.97.719 4.192a.75.75 0 01-1.088.791L8 12.347l-3.766 1.98a.75.75 0 01-1.088-.79l.72-4.194L.818 6.374a.75.75 0 01.416-1.28l4.21-.611L7.327.668A.75.75 0 018 .25z"/>
              </svg>
            {:else}
              <svg class="star-detail-icon" width="16" height="16" viewBox="0 0 16 16" fill="currentColor">
                <path d="M8 .25a.75.75 0 01.673.418l1.882 3.815 4.21.612a.75.75 0 01.416 1.279l-3.046 2.97.719 4.192a.75.75 0 01-1.088.791L8 12.347l-3.766 1.98a.75.75 0 01-1.088-.79l.72-4.194L.818 6.374a.75.75 0 01.416-1.28l4.21-.611L7.327.668A.75.75 0 018 .25zm0 2.445L6.615 5.5a.75.75 0 01-.564.41l-3.097.45 2.24 2.184a.75.75 0 01.216.664l-.528 3.084 2.769-1.456a.75.75 0 01.698 0l2.77 1.456-.53-3.084a.75.75 0 01.216-.664l2.24-2.183-3.096-.45a.75.75 0 01-.564-.41L8 2.694z"/>
              </svg>
            {/if}
          </button>
        {/if}
        <a class="gh-link" href={issue.URL} target="_blank" rel="noopener noreferrer" title="Open on GitHub">
          <svg width="14" height="14" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg">
            <path d="M6 3H3a1 1 0 0 0-1 1v9a1 1 0 0 0 1 1h9a1 1 0 0 0 1-1v-3" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>
            <path d="M10 2h4v4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
            <path d="M8 8L14 2" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>
          </svg>
        </a>
      </div>

      <!-- Meta row -->
      <div class="meta-row">
        <span class="meta-item">{detail.repo_owner}/{detail.repo_name}</span>
        <span class="meta-sep">·</span>
        <CopyItemNumber kind="issue" number={issue.Number} url={issue.URL} />
        <span class="meta-sep">·</span>
        <span class="meta-item">{issue.Author}</span>
        {#if (issue.assignees && issue.assignees.length > 0) || capabilities.assignee_mutation}
          <span class="meta-sep">·</span>
          <UserListEditor
            label="Assignees"
            users={issue.assignees ?? []}
            canEdit={capabilities.assignee_mutation}
            disabled={staleIssue || assigneeGate.unavailable}
            disabledReason={assigneeGate.unavailable ? assigneeGate.reason : undefined}
            loadCandidates={loadUserCandidates}
            avatarUrlForUser={userAvatarURL}
            onchange={(next, callbacks) => issues.setIssueAssignees(owner, name, number, next, callbacks)}
          >
            {#snippet icon()}
              <UsersIcon size={12} aria-hidden="true" />
            {/snippet}
          </UserListEditor>
        {/if}
        <span class="meta-sep">·</span>
        <span class="meta-item">{formatRelativeTime(issue.CreatedAt)}</span>
        <span class="meta-sep">·</span>
        <Chip size="xs" tone={issue.State === "open" ? "success" : "merged"} class="issue-state-chip">
          {issue.State === "open" ? "Open" : "Closed"}
        </Chip>
        {#if labels.length > 0 || (capabilities.read_labels && capabilities.label_mutation)}
          <span class="meta-sep">·</span>
          <LabelRow {labels} />
          {#if capabilities.read_labels && capabilities.label_mutation}
            <div class="label-editor-anchor" bind:this={labelPickerAnchor}>
              <Button
                class="btn--labels"
                label="Labels"
                shortLabel="Labels"
                size="sm"
                surface="soft"
                tone="neutral"
                disabled={staleIssue || labelGate.unavailable}
                title={labelGate.unavailable ? labelGate.reason : undefined}
                onclick={openLabelPicker}
              >
                <TagsIcon size="16" aria-hidden="true" />
              </Button>
              {#if labelPickerOpen}
                <!-- Escape precedence: a non-empty filter claims Escape to clear itself
                     (kit SearchInput stops propagation); only an empty-field Escape
                     bubbles here and dismisses the picker. -->
                <div
                  class="label-editor-popover"
                  style={labelPickerStyle}
                  bind:this={labelPickerPopover}
                  role="presentation"
                  onkeydown={(event) => {
                    if (event.key === "Escape") {
                      event.stopPropagation();
                      closeLabelPicker();
                    }
                  }}
                >
                  <LabelPicker
                    catalogLabels={labelCatalog}
                    selectedLabels={labels}
                    syncing={labelCatalogSyncing}
                    {pendingLabel}
                    error={labelPickerError}
                    autofocusFilter={labelPickerAutofocusFilter}
                    disabled={labelGate.unavailable}
                    disabledReason={labelGate.unavailable ? labelGate.reason : undefined}
                    ontoggle={toggleLabel}
                    onclear={clearLabels}
                    onclose={closeLabelPicker}
                  />
                </div>
              {/if}
            </div>
          {/if}
        {/if}
        {#if issues.isIssueDetailSyncing() && !manualRefreshPending}
          <span class="meta-sep">·</span>
          <span class="sync-indicator" title="Syncing from GitHub">
            <Spinner size={12} label="Syncing" />
            Syncing
          </span>
        {/if}
      </div>

      <!-- Actions. The kit grid owns the row geometry: every control inherits
           its 28px control height and md type from the grid item, so no
           button here asks for its own size. -->
      {#snippet workspaceAction()}
        {#if workspace}
          {#if inlineWorkspace}
            <Button
              class="btn--workspace"
              disabled={staleIssue || workspaceDeletionLifecycle}
              onclick={() => {
                if (staleIssue || workspaceDeletionLifecycle) return;
                inlineWorkspace.focusTerminal();
              }}
              tone="info"
              surface="soft"
              label="Focus Terminal"
              shortLabel="Terminal"
            >
              <MonitorUpIcon size="14" strokeWidth="2.2" aria-hidden="true" />
            </Button>
          {:else}
            <Button
              class="btn--workspace"
              disabled={staleIssue}
              onclick={() => {
                if (staleIssue) return;
                if (workspaceDeletionLifecycle) {
                  if (onViewWorkspaces) onViewWorkspaces();
                  else navigate("/workspaces");
                } else if (onOpenWorkspace) {
                  onOpenWorkspace(workspace.id);
                } else {
                  navigate(`/terminal/${workspace.id}`);
                }
              }}
              tone="info"
              surface="soft"
              label={workspaceDeletionLifecycle ? "View in Workspaces" : "Open Workspace"}
              shortLabel={workspaceDeletionLifecycle ? "Workspaces" : "Workspace"}
            >
              <MonitorUpIcon size="14" strokeWidth="2.2" aria-hidden="true" />
            </Button>
          {/if}
        {:else}
          <WorkspaceCreateSplitButton
            label="Create Workspace"
            busyLabel="Creating..."
            launchTargets={settings.getLaunchTargets()}
            busy={workspaceCreateBlocked}
            disabled={staleIssue}
            disabledReason={staleIssue
              ? "Refresh details before creating a workspace."
              : createWorkspaceTitle}
            descriptionId={createWorkspaceDescriptionId}
            onCreate={(targetKey) => void createWorkspace(
              targetKey === undefined ? {} : { launchTargetKey: targetKey },
            )}
          />
        {/if}
      {/snippet}
      {#snippet workspaceSecondaryAction()}
        {#if workspace && inlineWorkspace}
          <Button
            class="btn--workspace-secondary"
            disabled={staleIssue}
            onclick={() => {
              if (staleIssue) return;
              if (workspaceDeletionLifecycle) {
                navigate("/workspaces");
              } else {
                inlineWorkspace.openInWorkspaces(workspace);
              }
            }}
            tone="neutral"
            surface="soft"
            label={workspaceDeletionLifecycle ? "View in Workspaces" : "Open in Workspaces"}
            shortLabel="Workspaces"
          >
            <ExternalLinkIcon size="14" strokeWidth="2.2" aria-hidden="true" />
          </Button>
        {/if}
      {/snippet}
      {#snippet stateAction()}
        {#if issue.State === "open" && capabilities.state_mutation}
          {@const closeGate = operationGate(repoOperations?.close_issue)}
          <Button
            class="btn--close"
            disabled={stateSubmitting || staleIssue || closeGate.unavailable}
            title={closeGate.unavailable ? closeGate.reason : undefined}
            onclick={() => {
              if (staleIssue || closeGate.unavailable) return;
              handleStateChange("closed");
            }}
            tone="danger"
            surface="outline"
            label={stateSubmitting ? "Closing..." : "Close issue"}
            shortLabel={stateSubmitting ? "Closing..." : "Close"}
          >
            <XIcon size="14" strokeWidth="2.2" aria-hidden="true" />
          </Button>
        {:else if capabilities.state_mutation}
          {@const reopenGate = operationGate(repoOperations?.reopen_issue)}
          <Button
            class="btn--reopen"
            disabled={stateSubmitting || staleIssue || reopenGate.unavailable}
            title={reopenGate.unavailable ? reopenGate.reason : undefined}
            onclick={() => {
              if (staleIssue || reopenGate.unavailable) return;
              handleStateChange("open");
            }}
            tone="success"
            surface="solid"
            label={stateSubmitting ? "Reopening..." : "Reopen issue"}
            shortLabel={stateSubmitting ? "Reopening..." : "Reopen"}
          >
            <RefreshCwIcon size="14" strokeWidth="2.2" aria-hidden="true" />
          </Button>
        {/if}
      {/snippet}
      {#snippet embeddingActions()}
        <div class="embedding-actions">
          {#each actions.issue ?? [] as action (action.id)}
            <Button
              class="btn--embedding-action"
              onclick={() => {
                if (staleIssue) return;
                action.handler({
                  surface: "issue-detail", owner, name, number,
                });
              }}
              disabled={staleIssue}
              tone="neutral"
              surface="outline"
            >
              {action.label}
            </Button>
          {/each}
        </div>
      {/snippet}
      {#if !workspace}
        <span id={createWorkspaceDescriptionId} class="kit-sr-only">
          {staleIssue
            ? "Refresh details before creating a workspace."
            : createWorkspaceTitle}
        </span>
      {/if}
      <AdaptiveActionGrid
        class="issue-actions-grid"
        ariaLabel="Issue actions"
        frame="none"
        padding={0}
        rowGap={4}
        columnGap={4}
        items={[
          { id: "workspace", content: workspaceAction },
          ...(workspace && inlineWorkspace ? [{ id: "workspace-secondary", content: workspaceSecondaryAction }] : []),
          ...(capabilities.state_mutation ? [{ id: "state", content: stateAction }] : []),
          ...((actions.issue ?? []).length > 0 ? [{ id: "embedding", content: embeddingActions }] : []),
        ]}
      />

      <!-- Issue body -->
      {#if issue.Body}
        <div class="section body-section">
          {#key descriptionItemKey}
            <CollapsibleDescription
              source={issue.Body}
              copied={bodyCopied}
              oncopy={() => copyBody(issue.Body)}
            >
              <!-- svelte-ignore a11y_click_events_have_key_events -->
              <!-- svelte-ignore a11y_no_static_element_interactions -->
              <div
                class="inset-box__content markdown-body"
                class:dragging={dragSourceIndex !== null}
                onclick={onBodyClick}
                ondragstart={onBodyDragStart}
                ondragover={onBodyDragOver}
                ondragleave={onBodyDragLeave}
                ondrop={onBodyDrop}
                ondragend={onBodyDragEnd}
              >
                <MarkdownHtml
                  raw={issue.Body}
                  repo={{ provider, platformHost, owner, name, repoPath }}
                  options={{
                    interactiveTasks: capabilities.state_mutation && !contentGate.unavailable,
                    collapseSingleLineBreaks: settings.getDetailSettings().collapse_single_line_breaks,
                  }}
                />
              </div>
            </CollapsibleDescription>
          {/key}
        </div>
      {/if}

      <!-- Comment box -->
      <div class="section">
        <IssueCommentBox
          {owner}
          {name}
          {number}
          provider={detail.repo.provider}
          platformHost={detail.platform_host}
          repoPath={detail.repo.repo_path}
          disabled={staleIssue || !capabilities.comment_mutation || addCommentGate.unavailable}
          disabledReason={addCommentGate.unavailable ? addCommentGate.reason : undefined}
        />
      </div>

      <!-- Activity -->
      <div class="section">
        <div class="section-title-row">
          <h3 class="section-title">Activity</h3>
          <div class="section-title-actions">
            <DetailRefreshButton
              disabled={issues.isIssueDetailLoading() || issues.isIssueDetailSyncing() || staleIssue}
              refreshing={manualRefreshPending}
              onRefresh={refreshDetail}
            />
            <DetailActivityViewMenu
              viewMode={detailActivityView.getMode()}
              onViewChange={(mode) => detailActivityView.setMode(mode)}
            />
          </div>
        </div>
        {#if issues.getIssueDetailLoaded()}
          <EventTimeline
            events={detail.events ?? []}
            {provider}
            {platformHost}
            repoOwner={owner}
            repoName={name}
            {repoPath}
            {number}
            itemType="issue"
            activityViewMode={detailActivityView.getMode()}
            initialEntryLimit={settings.getDetailSettings().initial_timeline_entry_limit}
            itemIdentity={`${provider}:${platformHost ?? ""}:${repoPath}:${number}`}
            onEditComment={capabilities.comment_mutation && !staleIssue && !editCommentGate.unavailable
              ? editTimelineComment
              : undefined}
            onDeleteComment={capabilities.comment_mutation && !staleIssue && !deleteCommentGate.unavailable
              ? deleteTimelineComment
              : undefined}
          />
        {:else if issues.isIssueDetailSyncing()}
          <div class="loading-placeholder">
            <Spinner size={14} label="Syncing" />
            Loading comments...
          </div>
        {:else}
          <div class="loading-placeholder">Detail not yet loaded</div>
        {/if}
      </div>
      </div>
    </div>
    </ScrollBox>

    {#if branchConflict}
      {@const conflict = branchConflict}
      <Modal
        title={conflict.existingDirectory
          ? "Existing Workspace Directory"
          : "Branch Name Conflict"}
        width="min(560px, 92vw)"
        maxWidth="min(560px, 92vw)"
        onclose={closeBranchConflictDialog}
      >
          <div class="conflict-body">
            {#if conflict.existingDirectory}
              <p class="modal-copy">
                Kenn Forge's workspace directory for this issue already contains branch
                <code>{conflict.existingBranch}</code>. A different branch cannot use the same directory.
              </p>
            {:else}
              <p class="modal-copy">
                The branch <code>{conflict.existingBranch}</code> already exists locally.
              </p>
            {/if}

            {#if !conflict.existingDirectory}
              <div class="branch-conflict-option">
                <div>
                  <div class="branch-conflict-heading">
                    Reuse the existing branch
                  </div>
                  <div class="branch-conflict-copy">
                    Reopen the workspace on the branch that is already present in the local clone.
                  </div>
                </div>
                <Button
                  class="btn btn--primary"
                  onclick={() => void createWorkspace({
                    gitHeadRef: conflict.existingBranch,
                    reuseExistingBranch: true,
                    fromConflictDialog: true,
                  })}
                  disabled={workspaceCreating}
                  tone="neutral"
                  surface="outline"
                  size="sm"
                >
                  {workspaceCreating ? "Creating..." : "Use Existing Branch"}
                </Button>
              </div>
            {/if}

            {#if conflict.existingDirectory}
              <div class="branch-conflict-option">
                <div>
                  <div class="branch-conflict-heading">
                    Use the existing Kenn Forge directory
                  </div>
                  <div class="branch-conflict-copy">
                    Recover the worktree already present at the directory Kenn Forge expects for this issue.
                  </div>
                </div>
                <Button
                  class="btn btn--primary"
                  onclick={() => void createWorkspace({
                    gitHeadRef: conflict.existingBranch,
                    reuseExistingDirectory: true,
                    fromConflictDialog: true,
                  })}
                  disabled={workspaceCreating}
                  tone="neutral"
                  surface="outline"
                  size="sm"
                >
                  {workspaceCreating ? "Creating..." : "Use Existing Directory"}
                </Button>
              </div>
            {/if}

            {#if !conflict.existingDirectory}
              <div class="field">
                <label
                  class="field-label"
                  for="issue-workspace-branch-name"
                >
                  New branch name
                </label>
                <input
                  id="issue-workspace-branch-name"
                  class="field-input"
                  type="text"
                  bind:value={conflict.branchInput}
                  oninput={() => {
                    if (branchConflict) {
                      branchConflict.error = null;
                    }
                  }}
                />
                <p class="field-hint">
                  Suggested: <code>{conflict.suggestedBranch}</code>
                </p>
              </div>
            {/if}

            {#if conflict.error}
              <p class="merge-error">{conflict.error}</p>
            {/if}
          </div>

        {#snippet footer()}
          <Button
            class="btn btn--secondary"
            onclick={closeBranchConflictDialog}
            disabled={workspaceCreating}
            tone="neutral"
            surface="outline"
          >
            Cancel
          </Button>
          {#if !conflict.existingDirectory}
            <Button
              class="btn btn--primary btn--green"
              onclick={() => void createWorkspace({
                gitHeadRef: conflict.branchInput,
                fromConflictDialog: true,
              })}
              disabled={workspaceCreating}
              tone="success"
              surface="solid"
            >
              {workspaceCreating ? "Creating..." : "Create New Branch"}
            </Button>
          {/if}
        {/snippet}
      </Modal>
    {/if}
  {/if}
{/if}

<style>
  .state-center {
    display: flex;
    align-items: center;
    justify-content: center;
    height: 100%;
  }

  .state-msg {
    font-size: var(--font-size-root);
    color: var(--text-muted);
  }

  .state-msg--error {
    color: var(--accent-red);
  }

  .issue-detail {
    padding: 20px 24px;
    display: flex;
    flex-direction: column;
    min-width: 0;
    overflow-x: hidden;
    width: 100%;
  }

  /* Wrap long lines inside fenced code blocks at all widths (see
     PullDetail): scope to <pre> only so the wrap inherits to the inner
     <code> without touching inline code, which must keep the table-cell
     reset in app.css. */
  .issue-detail :global(.markdown-body pre) {
    max-width: 100%;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    word-break: break-word;
  }

  .issue-detail-content {
    container: issue-detail / inline-size;
    display: flex;
    flex-direction: column;
    gap: 16px;
    width: 100%;
    max-width: 800px;
    margin-inline: auto;
  }

  .label-editor-anchor {
    position: relative;
  }

  .label-editor-popover {
    position: fixed;
    z-index: 20;
  }

  .detail-header {
    display: flex;
    align-items: flex-start;
    gap: var(--space-4);
  }

  .detail-title {
    font-size: var(--font-size-xl);
    font-weight: 600;
    color: var(--text-primary);
    line-height: 1.35;
    flex: 1;
    min-width: 0;
  }

  .star-btn {
    flex-shrink: 0;
    display: flex;
    align-items: center;
    margin-top: 3px;
    cursor: pointer;
    background: none;
    border: none;
    padding: 0;
  }

  .star-detail-icon {
    color: var(--text-muted);
    transition: color 0.1s;
  }

  .star-detail-icon:hover {
    color: var(--accent-amber);
  }

  .star-detail-icon--active {
    color: var(--accent-amber);
  }

  .gh-link {
    flex-shrink: 0;
    color: var(--text-muted);
    display: flex;
    align-items: center;
    margin-top: 3px;
    transition: color 0.1s;
  }

  .gh-link:hover {
    color: var(--accent-blue);
    text-decoration: none;
  }

  .meta-row {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 4px;
  }

  .meta-item {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }

  .meta-sep {
    font-size: var(--font-size-sm);
    color: var(--text-muted);
  }

  .meta-row :global(.btn--labels) {
    min-height: 18px;
    padding: 0 6px;
    border-radius: 8px;
    font-size: var(--font-size-2xs);
    font-weight: 600;
  }

  .meta-row :global(.btn--labels svg) {
    width: 12px;
    height: 12px;
  }

  .sync-indicator {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: var(--font-size-xs);
    color: var(--accent-blue);
  }

  .section {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .section-title-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .section-title-actions {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }

  .section-title {
    font-size: var(--font-size-sm);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
  }

  .inset-box__content {
    padding: 10px 12px;
    font-size: var(--font-size-root);
    color: var(--text-primary);
    word-break: break-word;
    line-height: 1.6;
  }

  .issue-detail :global(.issue-actions-grid) {
    padding: var(--space-4) 0;
  }

  /* Embed-host actions are one custom grid item so each host action stays a
   * separate button; the wrapper lays them out like the grid's own row. */
  .issue-detail :global(.embedding-actions) {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-4);
  }

  .refresh-banner {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 4px 12px;
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    margin-bottom: 8px;
  }

  .detail-load-error {
    padding: 6px 16px;
    background: var(--accent-red-soft, color-mix(in srgb, var(--accent-red) 12%, transparent));
    color: var(--accent-red);
    border-bottom: 1px solid var(--border-subtle);
    font-size: var(--font-size-sm);
    flex-shrink: 0;
    margin-bottom: 8px;
  }


  .loading-placeholder {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 8px;
    padding: 24px 0;
    font-size: var(--font-size-sm);
    color: var(--text-muted);
  }

  .conflict-body {
    display: grid;
    gap: var(--space-5);
  }

  .modal-copy {
    margin: 0;
    font-size: var(--font-size-root);
    color: var(--text-secondary);
    line-height: 1.5;
  }

  .branch-conflict-option {
    display: flex;
    justify-content: space-between;
    gap: 12px;
    padding: 12px;
    border: 1px solid var(--border-muted);
    border-radius: 10px;
    background: var(--bg-inset);
  }

  .branch-conflict-heading {
    font-size: var(--font-size-root);
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 4px;
  }

  .branch-conflict-copy {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
    line-height: 1.5;
  }

  .field {
    display: grid;
    gap: 6px;
  }

  .field-label {
    font-size: var(--font-size-sm);
    font-weight: 600;
    color: var(--text-primary);
  }

  .field-input {
    width: 100%;
    min-width: 0;
    padding: 9px 11px;
    border: 1px solid var(--border-muted);
    border-radius: 8px;
    background: var(--bg-canvas);
    color: var(--text-primary);
    font-size: var(--font-size-root);
  }

  .field-hint {
    margin: 0;
    font-size: var(--font-size-xs);
    color: var(--text-muted);
  }

  .merge-error {
    margin: 0;
    font-size: var(--font-size-sm);
    color: var(--accent-red, #d73a49);
  }

  @media (max-width: 640px) {
    .issue-detail {
      --detail-mobile-type-xs: var(--mobile-type-xs, var(--font-size-xs));
      --detail-mobile-type-sm: var(--mobile-type-sm, var(--font-size-sm));
      --detail-mobile-type-body: var(--mobile-type-body, 13px);
      --detail-mobile-type-title: var(--mobile-type-title, var(--font-size-xl));
      --detail-mobile-space-xs: 6.5px;
      --detail-mobile-space-sm: 10px;
      --detail-mobile-space-md: 13px;
      --detail-mobile-hit-target: 37px;
      padding: var(--detail-mobile-space-md);
      font-size: var(--font-size-md);
      line-height: 1.5;
    }

    .issue-detail-content {
      gap: var(--detail-mobile-space-md);
      max-width: 100%;
    }

    .detail-header {
      gap: var(--detail-mobile-space-sm);
    }

    .detail-title {
      font-size: var(--font-size-xl);
      line-height: 1.25;
    }

    .star-btn,
    .gh-link {
      min-width: var(--detail-mobile-hit-target);
      min-height: var(--detail-mobile-hit-target);
      justify-content: center;
      padding: var(--detail-mobile-space-xs);
      margin-top: 0;
    }

    .meta-row :global(.copy-number-btn) {
      min-width: 24px;
      min-height: 24px;
      padding: 0;
      font-size: var(--font-size-sm);
      line-height: 1.35;
    }

    .meta-row {
      gap: var(--detail-mobile-space-xs);
    }

    .meta-item,
    .meta-sep,
    .sync-indicator,
    .section-title,
    .refresh-banner,
    .loading-placeholder {
      font-size: var(--font-size-sm);
      line-height: 1.35;
    }

    .inset-box__content,
    .modal-copy,
    .branch-conflict-heading,
    .branch-conflict-copy,
    .field-label,
    .field-input,
    .field-hint,
    .merge-error,
    .detail-load-error,
    :global(.markdown-body) {
      font-size: var(--font-size-md);
      line-height: 1.55;
    }

    .inset-box__content {
      padding: var(--detail-mobile-space-sm) var(--detail-mobile-space-md);
    }

    :global(.markdown-body pre),
    :global(.markdown-body code) {
      max-width: 100%;
      white-space: pre-wrap;
      overflow-wrap: anywhere;
      word-break: break-word;
    }

    :global(.markdown-body code) {
      font-size: 0.9em;
    }

    .issue-detail :global(.kit-chip),
    .issue-detail :global(.state-chip),
    .issue-detail :global(.status-chip) {
      min-height: calc(var(--detail-mobile-hit-target) * 0.65);
      padding: 2.5px var(--detail-mobile-space-xs);
      border-radius: 999px;
      font-size: var(--font-size-xs);
      line-height: 1.25;
    }

    .issue-detail :global(.issue-actions-grid .kit-button),
    .field-input {
      min-height: var(--detail-mobile-hit-target);
      font-size: var(--font-size-sm);
    }

  }
</style>

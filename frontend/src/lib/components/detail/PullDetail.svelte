<script module lang="ts">
  const pullDetailScrollPositions: Record<string, number> = Object.create(null) as Record<string, number>;
</script>

<script lang="ts">
  import { tablistKeyTarget } from "../shared/tablist-keyboard.js";
  import { Effect } from "effect";
  import { pollWhileVisible } from "../../effect/poll-while-visible.js";
  import { onDestroy, tick, untrack, type ComponentProps } from "svelte";
  import type { Attachment } from "svelte/attachments";
  import type { ApiProblemError, TransientTransportError } from "../../api/effect-errors.js";
  import { executeGeneratedApiRequest } from "../../api/generated-api.js";
  import { apiErrorMessage } from "../../api/runtime.js";
  import { retryIdempotentRead } from "../../api/retry-policy.js";
  import type { AppExecution } from "../../app/runtime.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { nextAnimationFrame } from "../../browser/animation-frame.js";
  import { transientClipboardFeedback } from "../../browser/clipboard-feedback.js";
  import type {
    KanbanStatus,
    Label,
    ProviderCapabilities,
    PullDetail,
    PullRequest,
    RepoOperations,
  } from "../../api/types.js";
  import type { DetailSyncMode } from "../../stores/detail.svelte.js";
  import type { MutationCallbacks } from "../../stores/ordered-mutations.js";
  import type { ConflictReason } from "../../api/problems.js";
  import { showFlash } from "../../stores/flash.svelte.js";
  import {
    getStores, getActions,
    getUIConfig, getNavigate,
  } from "../../context.js";
  import MarkdownHtml from "../shared/MarkdownHtml.svelte";
  import WorkflowDispatchDialog from "../actions/WorkflowDispatchDialog.svelte";
  import type { WorkflowDispatchRequest } from "../actions/WorkflowDispatchForm.svelte";
  import { workflowDispatchPresentation } from "../actions/workflow-dispatch-presentation.js";
  import { buildPullRequestFilesRoute } from "../../routes.js";
  import { moveTaskListItem, toggleTaskListItem } from "../../utils/task-list.js";
  import type { ApplySuggestionRequest } from "../../utils/markdown-suggestions.js";
  import { firstUnavailableGate, operationGate } from "./operation-gates.js";
  import { AdaptiveActionGrid, FitStages, copyToClipboard, formatRelativeTime } from "@kenn-io/kit-ui";
  import EventTimeline from "./EventTimeline.svelte";
  import CollapsibleDescription from "./CollapsibleDescription.svelte";
  import DetailActivityViewMenu from "./DetailActivityViewMenu.svelte";
  import DetailRefreshButton from "./DetailRefreshButton.svelte";
  import CommentBox from "./CommentBox.svelte";
  import ApproveButton from "./ApproveButton.svelte";
  import ApproveWorkflowsButton from "./ApproveWorkflowsButton.svelte";
  import MergeModal from "./MergeModal.svelte";
  import ReadyForReviewButton from "./ReadyForReviewButton.svelte";
  import ReviewDecisionChip from "./ReviewDecisionChip.svelte";
  import {
    runOpenMerge,
    type PRDetailActionInput,
  } from "./keyboard-actions.js";
    import { SelectDropdown } from "@kenn-io/kit-ui";
  import ChevronDownIcon from "@lucide/svelte/icons/chevron-down";
  import CheckIcon from "@lucide/svelte/icons/check";
  import ClockIcon from "@lucide/svelte/icons/clock";
  import ExternalLinkIcon from "@lucide/svelte/icons/external-link";
  import GitMergeIcon from "@lucide/svelte/icons/git-merge";
  import MonitorUpIcon from "@lucide/svelte/icons/monitor-up";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import SendHorizontalIcon from "@lucide/svelte/icons/send-horizontal";
  import TagsIcon from "@lucide/svelte/icons/tags";
  import UserCheckIcon from "@lucide/svelte/icons/user-check";
  import UsersIcon from "@lucide/svelte/icons/users";
  import WorkflowIcon from "@lucide/svelte/icons/workflow";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, Chip } from "@kenn-io/kit-ui";
  import { Spinner } from "@kenn-io/kit-ui";
  import LabelRow from "../shared/LabelRow.svelte";
  import WorkspaceCreateSplitButton from "../workspace/WorkspaceCreateSplitButton.svelte";
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
  import DiffFilesLayout from "../diff/DiffFilesLayout.svelte";
  import { reviewThreadsFromEvents } from "../diff/review-thread-context.js";
  import {
    reviewThreadTargetLine,
    reviewThreadTargetSide,
    type ReviewThread,
  } from "../diff/review-thread-context.js";
  import CIStatus from "./CIStatus.svelte";
  import StackStatus from "./StackStatus.svelte";
  import MergeWarningsChip, { type MergeWarningEntry } from "./MergeWarningsChip.svelte";
  import { providerDisplayLabel } from "../../api/provider-labels.js";
  import DiffSummaryChip from "./DiffSummaryChip.svelte";
  import CopyItemNumber from "./CopyItemNumber.svelte";
  import { DiffSummaryFilesResult } from "./diff-summary.js";
  import {
    canonicalProvider,
    providerItemPath,
    providerRepoPath,
    providerRouteParams,
    resolvedPlatformHost,
    type ProviderRouteRef, providerHostRouteParams, providerUsesHostRoute } from "../../api/provider-routes.js";
  import { supportsLocked } from "../../api/provider-capabilities.js";
  import { buildDiffSummaryKey } from "./diff-summary-key.js";
  import {
    activePRTimelineFilterCount,
    filterPREvents,
    loadPRTimelineFilter,
    savePRTimelineFilter,
    type PRTimelineFilterState,
  } from "./prTimelineFilter.js";
  import type { PullRequestRouteRef } from "../../routes.js";
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
  import type { WorkflowDefinition } from "../../stores/workflow-actions.svelte.js";

  type ChipTrailing = ComponentProps<typeof Chip>["trailing"];

  const CLEAR_LABELS_PENDING = "__clear-label-selection__";

  const {
    detail: detailStore,
    pulls,
    activity,
    diff: diffStore,
    detailActivityView,
    settings,
    sync,
    workflowActions,
  } = getStores();
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
      || detailStore.isDetailLoading()
      || detailStore.isDetailSyncing()
      || sync?.getProviderAvailable() === false
      || stalePR
    ) return;
    const requestIdentity = $state.snapshot(itemIdentity);
    const requestGeneration = ++manualRefreshGeneration;
    manualRefreshPending = true;
    detailStore.syncDetailNow(
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
    return detailStore.getDetail()?.repo?.capabilities
      ?? defaultProviderCapabilities;
  }

  interface Props {
    owner: string;
    name: string;
    number: number;
    provider: string;
    platformHost?: string | undefined;
    repoPath: string;
    hideTabs?: boolean;
    hideWorkspaceAction?: boolean;
    /**
     * Phone-like PR routes render the primary actions as one kit
     * AdaptiveActionGrid instead of the desktop fit stages and actions menu.
     */
    phonePresentation?: boolean;
    hideStaleWhileLoading?: boolean;
    autoSync?: DetailSyncMode;
    workflowApprovalSync?: boolean;
    onStackMemberNavigate?: (ref: PullRequestRouteRef) => boolean | void;
    onDetailTabChange?: ((tab: "conversation" | "files") => void) | undefined;
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
    hideTabs = false,
    hideWorkspaceAction = false,
    phonePresentation = false,
    hideStaleWhileLoading = false,
    autoSync = "background",
    workflowApprovalSync = true,
    onStackMemberNavigate,
    onDetailTabChange,
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
    itemType: "pull" as const,
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
    itemType: "pull",
  });
  const descriptionItemKey = $derived(
    `${canonicalProvider(provider)}:${resolvedPlatformHost(provider, platformHost)}:${owner}/${name}:pull:${number}`,
  );

  let activeTab = $state<"conversation" | "files">("conversation");
  const detailTabKeys = ["conversation", "files"] as const;

  // The tab strip is one tab stop: arrows move between the tabs and select
  // them, so Tab from the active tab continues into the panel below.
  function handleDetailTabKeydown(event: KeyboardEvent): void {
    const target = tablistKeyTarget(event.key, detailTabKeys.indexOf(activeTab), detailTabKeys.length);
    if (target === null) return;
    event.preventDefault();
    const next = detailTabKeys[target];
    if (next === undefined || next === activeTab) return;
    activeTab = next;
    const strip = (event.currentTarget as HTMLElement).parentElement;
    strip?.querySelectorAll<HTMLElement>("[role='tab']")[target]?.focus();
  }
  let expandedPanel = $state<"ci" | "stack" | "merge" | null>(null);
  let pullDetailScroller: HTMLDivElement | undefined = $state();
  let pullDetailScrollRestoreExecution: AppExecution<void, never> | undefined;
  let keepStackExpandedOnRouteChange = false;
  let timelineFilter = $state<PRTimelineFilterState>(
    loadPRTimelineFilter(),
  );
  const timelineEvents = $derived(detailStore.getDetail()?.events ?? []);
  const filteredTimelineEvents = $derived.by(() =>
    filterPREvents(timelineEvents, timelineFilter),
  );
  const hasActiveTimelineFilters = $derived(
    activePRTimelineFilterCount(timelineFilter) > 0,
  );
  const timelineFilterScope = $derived(JSON.stringify(timelineFilter));

  function ciChecksHavePending(checksJSON: string): boolean {
    if (!checksJSON) return false;
    try {
      const checks = JSON.parse(checksJSON) as Array<{ status?: string }>;
      return checks.some((check) => check.status !== "completed");
    } catch {
      return false;
    }
  }

  function ciStatusIsPending(status: string): boolean {
    return ["pending", "in_progress", "queued"].includes(status.toLowerCase());
  }

  function ciStatusHasFailed(status: string): boolean {
    return [
      "failure",
      "failed",
      "error",
      "cancelled",
      "canceled",
      "timed_out",
    ].includes(status.toLowerCase());
  }

  function shouldDeferMergeForCI(status: string, checksJSON: string): boolean {
    // The backend rejects a deferred merge once aggregate CI has failed, so a
    // failed pipeline with a check still running must use the normal merge path
    // rather than route to the deferred endpoint that would 409.
    if (ciStatusHasFailed(status)) return false;
    return ciStatusIsPending(status) || ciChecksHavePending(checksJSON);
  }

  function requiredStatusChecksHaveNotPassed(checksJSON: string): boolean {
    if (!checksJSON) return false;
    try {
      const checks = JSON.parse(checksJSON) as Array<{
        required?: boolean;
        status?: string;
        conclusion?: string;
      }>;
      return checks.some((check) =>
        check.required === true &&
        (
          check.status !== "completed" ||
          !["success", "neutral", "skipped"].includes(check.conclusion ?? "")
        ),
      );
    } catch {
      return false;
    }
  }

  function mergeWarningEntries(
    pr: Pick<PullRequest, "State" | "MergeableState" | "CIChecksJSON">,
    warnings: readonly string[] | null | undefined,
    stale: boolean,
  ): MergeWarningEntry[] {
    const entries: MergeWarningEntry[] = [];
    if (!stale && pr.State === "open") {
      if (pr.MergeableState === "dirty") {
        entries.push({
          kind: "conflict",
          text: "This branch has conflicts that must be resolved before merging.",
        });
      }
      if (pr.MergeableState === "blocked") {
        entries.push({
          kind: "blocked",
          text: "Branch protection rules may prevent this merge.",
        });
      }
      if (pr.MergeableState === "behind") {
        entries.push({
          kind: "behind",
          text: "This branch is behind the base branch and may need to be updated.",
        });
      }
      if (requiredStatusChecksHaveNotPassed(pr.CIChecksJSON)) {
        entries.push({
          kind: "required-checks",
          text: "Required status checks have not passed.",
        });
      }
    }
    for (const warning of warnings ?? []) {
      entries.push({ kind: "server", text: warning });
    }
    return entries;
  }
  function editTimelineComment(
    event: { PlatformID: number | null },
    body: string,
    callbacks: MutationCallbacks,
  ): void {
    if (stalePR || event.PlatformID === null) {
      callbacks.onFailure?.(
        stalePR ? "Refresh pull request details before editing a comment" : "Comment identifier is unavailable",
      );
      callbacks.onSettled?.();
      return;
    }
    detailStore.editComment(owner, name, number, event.PlatformID, body, callbacks);
  }

  function deleteTimelineComment(event: { PlatformID: number | null }, callbacks: MutationCallbacks): void {
    if (stalePR || event.PlatformID === null) {
      callbacks.onFailure?.(
        stalePR ? "Refresh pull request details before deleting a comment" : "Comment identifier is unavailable",
      );
      callbacks.onSettled?.();
      return;
    }
    detailStore.deleteComment(owner, name, number, event.PlatformID, callbacks);
  }

  function applyTimelineSuggestion(
    input: ApplySuggestionRequest,
    callbacks: {
      readonly onResult: (result: boolean | { ok: false; error: string }) => void;
      readonly onSettled: () => void;
    },
  ): void {
    if (stalePR || headActionsBlocked || applySuggestionGate.unavailable || currentPR()?.State !== "open") {
      callbacks.onResult(false);
      callbacks.onSettled();
      return;
    }
    const requestGeneration = mutationRouteGeneration;
    let durableConflict = false;
    detailStore.applyReviewSuggestions(routeRef, number, input, {
      onConflict: (conflict) => {
        durableConflict = handleStateConflict(
          conflict.reason,
          conflict.context,
          conflict.expectedHeadSha,
          conflict.ref,
          conflict.number,
          requestGeneration,
        );
      },
      onResult: (ok) => {
        if (!ok && durableConflict) {
          callbacks.onResult({
            ok: false,
            error: detailStore.getDetailError() ?? "Pull request state changed.",
          });
          return;
        }
        callbacks.onResult(ok);
      },
      onSettled: callbacks.onSettled,
    });
  }

  function updateTimelineFilter(next: PRTimelineFilterState): void {
    timelineFilter = next;
    savePRTimelineFilter(next);
  }

  function pullDetailScrollKey(): string {
    return [
      provider,
      platformHost,
      repoPath,
      number,
      "conversation",
    ].join("\0");
  }

  function rememberPullDetailScroll(): void {
    if (!pullDetailScroller) return;
    pullDetailScrollPositions[pullDetailScrollKey()] = pullDetailScroller.scrollTop;
  }

  function restorePullDetailScroll(): void {
    const restoreKey = pullDetailScrollKey();
    pullDetailScrollRestoreExecution?.interrupt();
    pullDetailScrollRestoreExecution = runtime.runCommand(
      Effect.promise(() => tick()).pipe(
        Effect.andThen(nextAnimationFrame),
        Effect.andThen(
          Effect.sync(() => {
            if (!pullDetailScroller) return;
            pullDetailScroller.scrollTop = pullDetailScrollPositions[restoreKey] ?? 0;
          }),
        ),
      ),
      {
        operation: "restore pull request detail scroll",
        safeContext: { owner, name, number },
        onFailure: () => {},
      },
    );
  }

  function handlePullDetailScroll(): void {
    rememberPullDetailScroll();
  }

  function jumpToReviewThread(thread: ReviewThread): void {
    diffStore.requestScrollToLine(
      thread.path,
      reviewThreadTargetLine(thread),
      reviewThreadTargetSide(thread),
    );
    if (hideTabs) {
      if (onDetailTabChange) {
        onDetailTabChange("files");
        return;
      }
      navigate(buildPullRequestFilesRoute({ ...routeRef, number }));
      return;
    }
    activeTab = "files";
  }

  // Mutating actions (close/reopen, kanban state, star, save title/body,
  // workspace creation, etc.) read the (owner, name, number) PROPS, but
  // the visible detail is whatever loadDetail last produced. During a
  // route change those drift apart for the brief window before the new
  // load completes. `stalePR` is true in that window, and every mutation
  // handler short-circuits on it so a click during the transition can't
  // operate on the freshly-routed PR while showing the previous one.
  const stalePR = $derived.by(() => {
    const d = detailStore.getDetail();
    if (d == null) return false;
    return (
      d.repo_owner !== owner ||
      d.repo_name !== name ||
      (d.merge_request?.Number ?? -1) !== number ||
      // Props may carry provider aliases (gh) or omit the default host
      // (Activity URLs) while the payload is canonical and concrete;
      // treating those as stale would disable every mutation action on a
      // detail that is in fact current.
      canonicalProvider(d.repo?.provider ?? "") !== canonicalProvider(provider) ||
      resolvedPlatformHost(provider, d.repo?.platform_host) !==
        resolvedPlatformHost(provider, platformHost) ||
      d.repo?.repo_path !== repoPath
    );
  });

  // Same comparison shape as PRListView's detailMatchesSelected, but
  // against the inline workspace identity rather than a route ref: the
  // reconcile effect below must not fire the override reconciliation for
  // a detail payload that belongs to a different PR (e.g. one still
  // in-flight from a prior route).
  function detailMatchesIdentity(
    detail: PullDetail,
    identity: WorkspaceItemIdentity,
  ): boolean {
    return (
      detail.repo_owner === identity.owner &&
      detail.repo_name === identity.name &&
      detail.merge_request.Number === identity.number &&
      canonicalProvider(detail.repo?.provider ?? "") === canonicalProvider(identity.provider) &&
      resolvedPlatformHost(identity.provider, detail.repo?.platform_host) ===
        resolvedPlatformHost(identity.provider, identity.platformHost) &&
      detail.repo?.repo_path === identity.repoPath
    );
  }

  const shouldAutoRefreshCI = $derived.by(() => {
    const pr = currentPR();
    return Boolean(
      expandedPanel === "ci" &&
      !stalePR &&
      pr?.State === "open" &&
      ciChecksHavePending(pr.CIChecksJSON),
    );
  });

  $effect(() => {
    if (!shouldAutoRefreshCI) return;
    const execution = untrack(() =>
      runtime.runCommand(
        pollWhileVisible(
          Effect.sync(() => {
            detailStore.refreshPendingCI(owner, name, number, {
              provider,
              platformHost,
              repoPath,
              workflowApprovalSync,
            });
          }),
          "15 seconds",
          { immediate: true },
        ),
        {
          operation: "refresh pending pull request checks",
          safeContext: { owner, name, number },
          onFailure: () => {},
        },
      ),
    );
    return execution.interrupt;
  });

  let lastDetailLoadIdentity: WorkspaceItemIdentity | null = null;
  let lastDetailLoadAutoSync: DetailSyncMode | undefined;
  let lastDetailLoadWorkflowApprovalSync: boolean | undefined;

  $effect(() => {
    const requestOwner = owner;
    const requestName = name;
    const requestNumber = number;
    const requestProvider = provider;
    const requestPlatformHost = platformHost;
    const requestRepoPath = repoPath;
    const requestAutoSync = autoSync;
    const requestWorkflowApprovalSync = workflowApprovalSync;
    const requestIdentity = $state.snapshot(itemIdentity);
    const shouldLoad =
      lastDetailLoadIdentity === null
      || !identityEquals(lastDetailLoadIdentity, requestIdentity)
      || lastDetailLoadAutoSync !== requestAutoSync
      || lastDetailLoadWorkflowApprovalSync !== requestWorkflowApprovalSync;
    if (shouldLoad) {
      lastDetailLoadIdentity = requestIdentity;
      lastDetailLoadAutoSync = requestAutoSync;
      lastDetailLoadWorkflowApprovalSync = requestWorkflowApprovalSync;
    }
    untrack(() => {
      if (shouldLoad) {
        detailStore.loadDetail(
          requestOwner,
          requestName,
          requestNumber,
          {
            sync: requestAutoSync,
            workflowApprovalSync: requestWorkflowApprovalSync,
            provider: requestProvider,
            platformHost: requestPlatformHost,
            repoPath: requestRepoPath,
          },
        );
      }
      detailStore.startDetailPolling(
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
    return () => detailStore.stopDetailPolling();
  });

  $effect(() => {
    const handler = (event: Event) => onOpenLabelPickerCommand(event);
    window.addEventListener(OPEN_LABEL_PICKER_EVENT, handler);
    return () => window.removeEventListener(OPEN_LABEL_PICKER_EVENT, handler);
  });

  $effect(() => {
    void pullDetailScroller;
    void activeTab;
    void detailStore.getDetail()?.detail_fetched_at;
    if (activeTab !== "conversation") return;
    restorePullDetailScroll();
  });

  onDestroy(() => {
    pullDetailScrollRestoreExecution?.interrupt();
    labelCatalogGeneration += 1;
    labelCatalogExecution?.interrupt();
    branchCopyExecution?.interrupt();
    bodyCopyExecution?.interrupt();
    flushBodySave();
    componentDestroyed = true;
  });

  let mutationRouteGeneration = $state(0);

  // Clear modal/edit state on route change so PR A's open modal
  // can't reappear for PR B once `stalePR` clears.
  //
  // Tracks the last identity this effect reset for: a route transition can
  // re-express the same item (gh vs github, omitted vs concrete default
  // host), and an alias-only change must not bump the generations — that
  // would discard an in-flight create's success and re-enable the button
  // for a duplicate request.
  let lastResetIdentity: WorkspaceItemIdentity | null = null;
  $effect(() => {
    // Full provider-aware PR identity (via itemIdentity's deps): the same
    // owner/name/number can exist on another provider or host, and stale
    // head-conflict state must not leak across that navigation either.
    const current = $state.snapshot(itemIdentity);
    if (lastResetIdentity !== null && identityEquals(lastResetIdentity, current)) return;
    lastResetIdentity = current;
    manualRefreshGeneration += 1;
    manualRefreshPending = false;
    mutationRouteGeneration = untrack(() => mutationRouteGeneration) + 1;
    wsRequestGen += 1;
    // The generation bump above stops an in-flight create's finally from
    // resetting this; the new selection starts from a clean flag.
    wsCreating = false;
    const keepStackExpanded = untrack(() => {
      const keepExpanded = keepStackExpandedOnRouteChange &&
        expandedPanel === "stack";
      keepStackExpandedOnRouteChange = false;
      return keepExpanded;
    });
    showMergeModal = false;
    conflictRefreshRequestID += 1;
    conflictRefreshBusy = false;
    stateConflict = null;
    headConflictContext = null;
    conflictReviewedHead = null;
    conflictRefreshError = null;
    expandedPanel = keepStackExpanded ? "stack" : null;
    editingTitle = false;
    editingBody = false;
    titleDraft = "";
    bodyDraft = "";
    // Flush any pending checkbox/reorder save before clearing state.
    // pendingBodySave captures the previous PR's identity at schedule
    // time, so this fires against the correct target even though
    // owner/name/number have already changed.
    flushBodySave();
    clearDragState();
    closeLabelPicker();
  });


  let copiedBranch = $state<string | null>(null);
  let branchCopyExecution: AppExecution<void, never> | undefined;
  let branchCopySeq = 0;

  function copyBranch(text: string): void {
    const seq = branchCopySeq;
    branchCopyExecution?.interrupt();
    branchCopyExecution = runtime.runCommand(
      transientClipboardFeedback({
        text,
        write: copyToClipboard,
        isActive: () => !componentDestroyed && seq === branchCopySeq,
        onCopied: () => {
          copiedBranch = text;
        },
        onExpired: () => {
          copiedBranch = null;
        },
      }),
      {
        operation: "copy pull request branch",
        safeContext: { owner, name, number },
        onFailure: () => {},
      },
    );
  }

  function onBranchCopyKeydown(event: KeyboardEvent, text: string): void {
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    copyBranch(text);
  }

  let stateSubmitting = $state(false);

  // Title editing
  let editingTitle = $state(false);
  let titleDraft = $state("");
  let savingTitle = $state(false);

  function currentPR() {
    return detailStore.getDetail()?.merge_request;
  }

  function startEditTitle(): void {
    if (stalePR || contentGate.unavailable) return;
    if (!currentCapabilities().state_mutation) return;
    const mr = currentPR();
    if (!mr) return;
    titleDraft = mr.Title;
    editingTitle = true;
  }

  function cancelEditTitle(): void {
    editingTitle = false;
    titleDraft = "";
  }

  function handleStarClick(): void {
    if (stalePR) return;
    const mr = currentPR();
    if (!mr) return;
    detailStore.toggleDetailPRStar(routeRef, number, mr.Starred);
  }

  function saveTitle(): void {
    if (stalePR || contentGate.unavailable) return;
    if (!currentCapabilities().state_mutation) return;
    const mr = currentPR();
    const trimmed = titleDraft.trim();
    if (!trimmed || trimmed === mr?.Title) {
      cancelEditTitle();
      return;
    }
    savingTitle = true;
    detailStore.updatePRContent(routeRef, number, { title: trimmed }, {
      onSuccess: () => {
        editingTitle = false;
        titleDraft = "";
      },
      onSettled: () => {
        savingTitle = false;
      },
    });
  }

  function onTitleKeydown(e: KeyboardEvent): void {
    if (e.key === "Enter") {
      e.preventDefault();
      saveTitle();
    } else if (e.key === "Escape") {
      cancelEditTitle();
    }
  }

  // Body editing
  let editingBody = $state(false);
  let bodyDraft = $state("");
  let savingBody = $state(false);

  function startEditBody(): void {
    if (stalePR || contentGate.unavailable) return;
    if (!currentCapabilities().state_mutation) return;
    const mr = currentPR();
    if (!mr) return;
    bodyDraft = mr.Body;
    editingBody = true;
  }

  function cancelEditBody(): void {
    editingBody = false;
    bodyDraft = "";
  }

  function saveBody(): void {
    if (stalePR || contentGate.unavailable) return;
    if (!currentCapabilities().state_mutation) return;
    const mr = currentPR();
    if (bodyDraft === mr?.Body) {
      cancelEditBody();
      return;
    }
    savingBody = true;
    detailStore.updatePRContent(routeRef, number, { body: bodyDraft }, {
      onSuccess: () => {
        editingBody = false;
        bodyDraft = "";
      },
      onSettled: () => {
        savingBody = false;
      },
    });
  }

  function onBodyKeydown(e: KeyboardEvent): void {
    if (e.key === "Escape") {
      cancelEditBody();
    }
  }

  function handleStateChange(
    newState: "open" | "closed" | "draft",
  ): void {
    if (stalePR) return;
    const caps = currentCapabilities();
    if (newState === "draft") {
      if (!caps.draft_mutation) return;
    } else if (!caps.state_mutation) {
      return;
    }
    stateSubmitting = true;
    detailStore.setPullState(routeRef, number, newState, {
      onSuccess: () => activity.loadActivity(),
      onSettled: () => {
        stateSubmitting = false;
      },
    });
  }

  type RepoSettings = {
    allowSquash: boolean;
    allowMerge: boolean;
    allowRebase: boolean;
    viewerCanMerge: boolean;
    operations?: RepoOperations;
  };

  let repoSettings = $state<RepoSettings | null>(null);
  let showMergeModal = $state(false);
  let mergeModalSettings = $state<RepoSettings | null>(null);

  // Head-pinning conflict state (context/provider-architecture.md
  // "Head binding"). Merge echoes the rendered reviewed head, while
  // approve sends the latest synced provider head when known. A 409
  // typed merge or head-binding conflicts land here and force a refresh.
  let stateConflict = $state<Exclude<ConflictReason, "conflict"> | null>(null);
  // Provider side-effect context from the conflict response (an
  // approval that could not be revoked, posted review text a retry
  // would repeat); rendered with the stale banner so the consequence
  // is not hidden behind the generic re-review prompt.
  let headConflictContext = $state<string | null>(null);
  let conflictReviewedHead = $state<string | null>(null);
  let conflictRefreshBusy = $state(false);
  let conflictRefreshError = $state<string | null>(null);
  let conflictRefreshRequestID = 0;
  const detailHeadSha = $derived(
    detailStore.getDetail()?.reviewed_head_sha ?? "",
  );
  const latestPlatformHeadSha = $derived(
    detailStore.getDetail()?.platform_head_sha ?? "",
  );
  // A typed conflict invalidates the state the user reviewed. Keep every
  // head-bound action closed until route navigation or explicit conflict
  // recovery verifies a fresh workflow context.
  const headActionsBlocked = $derived(stateConflict !== null);
  // Preflight guard for merge: a head-binding provider must never merge
  // against an unbound reviewed diff, so merge stays disabled until diff
  // sync proves the rendered code matches the current head — no request,
  // no 409 round trip.
  const headPinMissing = $derived(
    (detailStore.getDetail()?.repo?.capabilities?.mutation_head_binding ?? false)
      && detailHeadSha === "",
  );
  // A background "merge after CI" worker is waiting on this PR. The merge
  // action is replaced by a queued indicator until the worker reports
  // completion (deferred_merge_completed refreshes the detail).
  const deferredMergePending = $derived(
    detailStore.getDetail()?.deferred_merge_pending ?? false,
  );
  const midStackBlocker = $derived.by(() => {
    const stack = detailStore.getDetail()?.stack;
    if (!stack) return undefined;
    return stack.members?.find(
      (member) => member.position < stack.position && member.state !== "merged",
    );
  });
  const allowMidStackMerges = $derived(
    settings.getPullRequestSettings().allow_mid_stack_merges,
  );
  const midStackMergeBlocked = $derived(
    midStackBlocker !== undefined && !allowMidStackMerges,
  );

  function handleStateConflict(
    reason: Exclude<ConflictReason, "conflict">,
    context?: string,
    failedHeadSha?: string,
    failedRef: ProviderRouteRef = routeRef,
    failedNumber: number = number,
    failedGeneration: number = mutationRouteGeneration,
  ): boolean {
    if (
      failedGeneration !== mutationRouteGeneration
      || failedNumber !== number
      || failedRef.provider !== routeRef.provider
      || failedRef.platformHost !== routeRef.platformHost
      || failedRef.owner !== routeRef.owner
      || failedRef.name !== routeRef.name
      || failedRef.repoPath !== routeRef.repoPath
    ) return false;
    conflictReviewedHead = failedHeadSha ?? detailHeadSha;
    stateConflict = reason;
    headConflictContext = context ?? null;
    conflictRefreshError = null;
    showMergeModal = false;
    refreshConflictState(false);
    return true;
  }

  function conflictHasFreshContext(
    reason: Exclude<ConflictReason, "conflict">,
    reviewedHeadAtConflict: string | null,
  ): boolean {
    const detail = detailStore.getDetail();
    if (!detail) return false;
    const currentReviewedHead = detail.reviewed_head_sha.trim();
    const currentPlatformHead = detail.platform_head_sha.trim();
    const headIsCurrent = currentReviewedHead !== ""
      && currentReviewedHead === currentPlatformHead;

    if (reason === "not_open") {
      return detail.merge_request.State === "open" && headIsCurrent;
    }
    if (reason === "head_repo_unknown") {
      return detail.merge_request.HeadRepoCloneURL.trim() !== "" && headIsCurrent;
    }
    if (reason === "stale_state") {
      return headIsCurrent && currentReviewedHead !== reviewedHeadAtConflict;
    }
    return headIsCurrent;
  }

  function refreshConflictState(allowRecovery = true): void {
    const reason = stateConflict;
    if (!reason || conflictRefreshBusy) return;
    const requestID = ++conflictRefreshRequestID;
    const routeKey = `${provider}\n${platformHost}\n${repoPath}\n${owner}\n${name}\n${number}`;
    const reviewedHeadAtConflict = conflictReviewedHead;
    conflictRefreshBusy = true;
    conflictRefreshError = null;
    const finish = (refreshed: boolean): void => {
      const currentRouteKey = `${provider}\n${platformHost}\n${repoPath}\n${owner}\n${name}\n${number}`;
      if (requestID !== conflictRefreshRequestID || routeKey !== currentRouteKey) return;
      if (stateConflict !== reason) {
        conflictRefreshBusy = false;
        return;
      }
      if (!refreshed) {
        conflictRefreshError = "Could not refresh the pull request. Try again.";
      } else if (allowRecovery && conflictHasFreshContext(reason, reviewedHeadAtConflict)) {
        stateConflict = null;
        headConflictContext = null;
        conflictReviewedHead = null;
      }
      conflictRefreshBusy = false;
    };
    detailStore.syncDetailNow(
      owner,
      name,
      number,
      { provider, platformHost, repoPath },
      { onSuccess: finish, onFailure: () => finish(false) },
    );
  }

  function handleHeadConflict(
    reason: "stale_state" | "head_unknown",
    context: string | undefined,
    failedHeadSha: string,
    failedRef: ProviderRouteRef,
    failedNumber: number,
    failedGeneration: number,
  ): void {
    handleStateConflict(reason, context, failedHeadSha, failedRef, failedNumber, failedGeneration);
  }

  $effect(() => {
    const selectedRef = {
      provider: routeRef.provider,
      platformHost: routeRef.platformHost,
      owner: routeRef.owner,
      name: routeRef.name,
      repoPath: routeRef.repoPath,
    };
    repoSettings = null;
    const program = executeGeneratedApiRequest("GET repository merge settings", (client, signal) =>
      providerUsesHostRoute(selectedRef) ? client.RepositoriesService.getRepoOnHost({ ...providerHostRouteParams(selectedRef) }, { signal }) : client.RepositoriesService.getRepo({ ...providerRouteParams(selectedRef) }, { signal }),
    ).pipe(
      retryIdempotentRead,
      Effect.flatMap((settings) =>
        Effect.sync(() => {
          repoSettings = {
            allowSquash: settings.AllowSquashMerge,
            allowMerge: settings.AllowMergeCommit,
            allowRebase: settings.AllowRebaseMerge,
            viewerCanMerge: settings.ViewerCanMerge,
            operations: settings.operations,
          };
        }),
      ),
    );
    const execution = runtime.runCommand(program, {
      operation: "load repository merge settings",
      safeContext: { provider: selectedRef.provider, owner: selectedRef.owner, name: selectedRef.name },
      onFailure: () => {
        repoSettings = null;
      },
    });
    return execution.interrupt;
  });

  const workflowApproval = $derived(
    detailStore.getDetail()?.workflow_approval,
  );

  // Per-operation mutation availability. The detail payload is the
  // primary source — it is present as soon as the PR loads, so gating
  // never waits on the separate /repo settings request (which can
  // still be in flight or have failed). The settings response is the
  // fallback for older detail payloads without operations.
  const repoOperations = $derived(
    detailStore.getDetail()?.repo?.operations ?? repoSettings?.operations,
  );
  const addCommentGate = $derived(operationGate(repoOperations?.add_comment));
  const editCommentGate = $derived(operationGate(repoOperations?.edit_comment));
  const deleteCommentGate = $derived(operationGate(repoOperations?.delete_comment));
  const labelGate = $derived(firstUnavailableGate(
    repoOperations?.add_label, repoOperations?.remove_label,
  ));
  const assigneeGate = $derived(operationGate(repoOperations?.set_assignees));
  const reviewerGate = $derived(operationGate(repoOperations?.set_reviewers));
  // Content edits (title, body, task-list toggles/reorder) and the
  // review-thread affordances are first-class operations, so rate
  // limits gate them just like credential failures.
  const contentGate = $derived(operationGate(repoOperations?.update_content));
  const replyThreadGate = $derived(operationGate(repoOperations?.reply_review_thread));
  const resolveThreadGate = $derived(operationGate(repoOperations?.resolve_review_thread));
  const applySuggestionGate = $derived(operationGate(repoOperations?.apply_review_suggestion));

  const kanbanOptions: { value: KanbanStatus; label: string }[] = [
    { value: "new", label: "New" },
    { value: "reviewing", label: "Reviewing" },
    { value: "waiting", label: "Waiting" },
    { value: "awaiting_merge", label: "Awaiting Merge" },
  ];

  function onKanbanChange(value: string): void {
    if (stalePR) return;
    const option = kanbanOptions.find((candidate) => candidate.value === value);
    if (option === undefined) return;
    detailStore.updateKanbanState(routeRef, number, option.value);
  }

  function mergeActionLabel(settings: RepoSettings): string {
    if (settings.allowSquash && !settings.allowMerge && !settings.allowRebase) {
      return "Squash and merge";
    }
    if (!settings.allowSquash && settings.allowMerge && !settings.allowRebase) {
      return "Merge";
    }
    if (!settings.allowSquash && !settings.allowMerge && settings.allowRebase) {
      return "Rebase and merge";
    }
    return "Merge";
  }

  function mergeActionHasMenu(settings: RepoSettings): boolean {
    return [settings.allowSquash, settings.allowMerge, settings.allowRebase]
      .filter(Boolean).length > 1;
  }

  function hasEnabledMergeMethod(settings: RepoSettings): boolean {
    return settings.allowSquash || settings.allowMerge || settings.allowRebase;
  }

  function mergeActionShortLabel(settings: RepoSettings): string {
    if (settings.allowSquash && !settings.allowMerge && !settings.allowRebase) {
      return "Squash";
    }
    if (!settings.allowSquash && !settings.allowMerge && settings.allowRebase) {
      return "Rebase";
    }
    return "Merge";
  }

  function hasMergeConflicts(
    pr: Pick<PullRequest, "State" | "MergeableState">,
  ): boolean {
    return pr.State === "open" && pr.MergeableState === "dirty";
  }

  function buildOpenMergeInput(
    pr: Pick<PullRequest, "State" | "IsDraft" | "MergeableState" | "platform_head_sha">,
    capabilities: ProviderCapabilities,
  ): PRDetailActionInput {
    return {
      pr: {
        State: pr.State,
        IsDraft: pr.IsDraft,
        MergeableState: pr.MergeableState,
        // Same rendered-head source as the button gating and MergeModal:
        // the detail envelope's reviewed_head_sha. The raw platform head
        // can advance before diff sync proves the new code was reviewed.
        platform_head_sha: detailHeadSha,
      },
      ref: routeRef,
      number,
      viewerCan: {
        approve: false,
        merge: capabilities.merge_mutation,
        markReady: false,
        approveWorkflows: false,
      },
      repoSettings,
      // Treat a blocked head as stale for gating: the merge modal must
      // not open while the reviewed head is unknown.
      stale: stalePR || headActionsBlocked || midStackMergeBlocked,
      stores: { detail: detailStore },
      requireHeadPin: capabilities.mutation_head_binding,
      ...(detailHeadSha !== "" && { expectedHeadSha: detailHeadSha }),
      setMergeModalOpen: (open: boolean) => {
        if (open) {
          if (repoSettings === null) return;
          mergeModalSettings = { ...repoSettings };
        }
        showMergeModal = open;
      },
      onAfterOpenMerge: closeActionMenu,
    };
  }

  const worktreeLinks = $derived(
    detailStore.getDetail()?.worktree_links ?? [],
  );
  const hasWorktreeLinks = $derived(
    worktreeLinks.length > 0,
  );
  const importAction = $derived(
    (actions.pull ?? []).find(
      (a) => a.id === "import-worktree",
    ),
  );
  const navigateAction = $derived(
    (actions.pull ?? []).find(
      (a) => a.id === "navigate-worktree",
    ),
  );
  const otherActions = $derived(
    (actions.pull ?? []).filter(
      (a) =>
        a.id !== "import-worktree" &&
        a.id !== "navigate-worktree",
    ),
  );
  const labels = $derived(detailStore.getDetail()?.merge_request?.labels ?? []);
  const prAssignees = $derived(detailStore.getDetail()?.merge_request?.assignees ?? []);
  const prReviewers = $derived(detailStore.getDetail()?.merge_request?.requested_reviewers ?? []);
  let labelPickerOpen = $state(false);
  let labelCatalog = $state<Label[]>([]);
  let labelCatalogSyncing = $state(false);
  let labelPickerError = $state<string | null>(null);
  let pendingLabel = $state<string | null>(null);
  let labelPickerAnchor = $state<HTMLDivElement>();
  let labelPickerPopover = $state<HTMLDivElement>();
  let labelPickerLaunchedFromActionMenu = $state(false);
  let labelPickerAutofocusFilter = $state(false);
  let labelPickerStyle = $state("");
  let labelCatalogExecution: AppExecution<void, ApiProblemError | TransientTransportError> | null = null;
  let labelCatalogGeneration = 0;

  const workspace = $derived(
    stalePR
      ? null
      : inlineWorkspace
      ? inlineWorkspace.effectiveWorkspaceRef(itemIdentity, detailStore.getDetail()?.workspace ?? null)
      : // Without a controller there is no override store: the shared
        // resolver stands in — the confirmed created record wins over a
        // stale cached envelope, and an envelope still carrying a
        // session-deleted workspace ID is masked instead of re-offering
        // "Open Workspace" for a workspace that no longer exists.
        resolveControllerlessWorkspaceRef(itemIdentity, detailStore.getDetail()?.workspace ?? null),
  );
  const workspaceDeletionLifecycle = $derived(
    workspace?.status === "deleting" || workspace?.status === "deletion_failed",
  );

  // Once a detail load lands for the identity this component is currently
  // showing, let the inline controller drop its override if the envelope
  // now agrees (or update its bookkeeping if the envelope disagrees). A
  // load for a stale/mismatched identity must not reconcile — that would
  // let a slow response for PR A clear an override this component just
  // recorded for PR B.
  $effect(() => {
    const detail = detailStore.getDetail();
    if (!detail) return;
    if (!detailMatchesIdentity(detail, itemIdentity)) return;
    // The shared created-record reconciles the same way, controller or
    // not: an identity-matched envelope that carries the workspace is
    // authoritative, and a null envelope whose request started after the
    // creation was recorded (the tick comparison) is authoritative for
    // absence — another client deleted the workspace. Reading the tick
    // here also reruns this effect when a refreshed envelope's content
    // is identical and the detail object itself is not reassigned.
    const envelopeTick = detailStore.getDetailEnvelopeTick();
    reconcileWorkspaceCreated(itemIdentity, detail.workspace ?? null, envelopeTick);
    if (inlineWorkspace) inlineWorkspace.reconcile(itemIdentity, detail.workspace ?? null, envelopeTick);
  });

  let wsCreating = $state(false);
  // The shared pending store outlives this component and its local flag:
  // route resets and remounts clear wsCreating while the POST is still in
  // flight, and a round-trip back to this PR must keep the button disabled
  // or a second click sends a duplicate create.
  const wsCreateBlocked = $derived(wsCreating || isWorkspaceCreatePending(itemIdentity));
  // Bumped per create request and on identity change (route-reset effect
  // above): a workspace-create response whose generation no longer matches
  // arrived for a PR this component stopped showing and must not touch any
  // state. Destroy alone is weaker — the same PR can still be selected
  // (tab/layout change), so a success still records its store-level
  // override; only local state, flash, and navigation are skipped.
  let wsRequestGen = 0;
  let componentDestroyed = false;
  const createWorkspaceTitle =
    "Create a PR head worktree, then open Workspaces to launch agents, shells, or local review sessions on that branch.";
  const createWorkspaceDescriptionId =
    "pull-create-workspace-description";
  let actionMenuOpen = $state(false);
  let primaryActionStage = $state(0);
  let actionMenuWrapEl = $state<HTMLDivElement>();
  let workflowActionControlEl = $state<HTMLDivElement>();
  let stateMenuOpen = $state(false);
  let stateMenuWrapEl = $state<HTMLSpanElement>();

  function closeActionMenu(): void {
    actionMenuOpen = false;
  }

  let workflowDialogWorkflowId = $state<string | null>(null);
  let actionMenuTriggerEl = $state<HTMLButtonElement>();
  const captureActionMenuTrigger: Attachment<HTMLButtonElement> = (button) => {
    actionMenuTriggerEl = button;
    return () => {
      if (actionMenuTriggerEl === button) actionMenuTriggerEl = undefined;
    };
  };
  const workflowCatalogDemandEnabled = $derived(
    settings.isModeVisible("actions")
      && currentCapabilities().read_workflows
      && currentCapabilities().workflow_dispatch,
  );
  const workflowCatalog = $derived(
    workflowCatalogDemandEnabled
      ? workflowActions.getCatalog(routeRef)?.workflows ?? []
      : [],
  );
  const workflowDialogWorkflow = $derived(
    workflowCatalog.find((workflow) => workflow.id === workflowDialogWorkflowId) ?? null,
  );
  $effect(() => {
    if (workflowDialogWorkflowId !== null && workflowDialogWorkflow === null) workflowDialogWorkflowId = null;
  });
  const workflowProviderLabel = $derived(
    `${providerDisplayLabel(detailStore.getDetail()?.repo?.provider ?? provider)} Actions`,
  );
  const hasPrimaryPRActions = $derived(
    detailStore.getDetail()?.merge_request.State !== "merged" && !stalePR,
  );
  const hasWorkflowActions = $derived(
    workflowCatalogDemandEnabled && workflowCatalog.length > 0,
  );
  const showActionSurface = $derived(
    hasPrimaryPRActions || hasWorkflowActions,
  );
  const workflowInitialRef = $derived.by(() => {
    const detail = detailStore.getDetail();
    if (!detail) return "";
    return detail.merge_request.State === "open" && detail.head_repo_kind === "same_repo"
      ? detail.merge_request.HeadBranch
      : detail.merge_request.BaseBranch;
  });
  const workflowDialogPresentation = $derived.by(() =>
    workflowDispatchPresentation(
      workflowActions.getSnapshot(routeRef),
      workflowDialogWorkflow?.id ?? null,
    )
  );

  function openWorkflowDialog(workflow: WorkflowDefinition): void {
    if (!workflowCatalogDemandEnabled || !workflow.available) return;
    workflowDialogWorkflowId = workflow.id;
    closeActionMenu();
  }

  function submitWorkflow(request: WorkflowDispatchRequest): void {
    const workflow = workflowDialogWorkflow;
    if (!workflowCatalogDemandEnabled || !workflow) return;
    workflowActions.dispatch({
      ref: routeRef,
      workflowId: workflow.id,
      expectedDefinitionSha: workflow.definition_sha,
      dispatchRef: request.ref,
      inputs: request.inputs,
    });
  }

  function reloadWorkflowCatalog(): void {
    const workflow = workflowDialogWorkflow;
    if (!workflowCatalogDemandEnabled || !workflow) return;
    workflowActions.refreshCatalog(routeRef, workflow.id);
  }

  function newWorkflowDispatchCycle(): void {
    const workflow = workflowDialogWorkflow;
    if (!workflowCatalogDemandEnabled || !workflow) return;
    workflowActions.newDispatchCycle(routeRef, workflow.id);
  }

  function closeWorkflowDialog(): void {
    const workflow = workflowDialogWorkflow;
    if (workflow && workflowActions.getSnapshot(routeRef)?.catalogRefreshErrors[workflow.id] !== undefined) {
      workflowActions.clearCatalogRefreshError(routeRef, workflow.id);
    }
    workflowDialogWorkflowId = null;
  }

  function loadWorkflowCatalog(ref: ProviderRouteRef | null): Attachment {
    return () => {
      if (!ref) return;
      untrack(() => workflowActions.loadCatalog(ref));
      return () => {
        workflowDialogWorkflowId = null;
        closeActionMenu();
      };
    };
  }

  function closeStateMenu(): void {
    stateMenuOpen = false;
  }

  function visiblePRState(pr: Pick<PullRequest, "State" | "IsDraft">): "merged" | "closed" | "draft" | "open" {
    if (pr.State === "merged") return "merged";
    if (pr.State === "closed") return "closed";
    if (pr.IsDraft) return "draft";
    return "open";
  }

  function visiblePRStateLabel(pr: Pick<PullRequest, "State" | "IsDraft">): string {
    const state = visiblePRState(pr);
    return state.charAt(0).toUpperCase() + state.slice(1);
  }

  function toggleStateMenu(): void {
    if (stalePR) return;
    stateMenuOpen = !stateMenuOpen;
  }

  function chooseState(newState: "open" | "closed" | "draft"): void {
    closeStateMenu();
    handleStateChange(newState);
  }

  function closeLabelPicker(): void {
    labelCatalogGeneration += 1;
    labelCatalogExecution?.interrupt();
    labelCatalogExecution = null;
    labelPickerOpen = false;
    labelPickerError = null;
    pendingLabel = null;
    labelPickerLaunchedFromActionMenu = false;
    labelPickerAutofocusFilter = false;
  }

  function positionLabelPicker(): void {
    if (labelPickerLaunchedFromActionMenu) {
      labelPickerStyle = [
        "left: 50%",
        "top: 50%",
        "width: min(360px, calc(100dvw - 24px))",
        "transform: translate(-50%, -50%)",
        "--label-picker-max-height: min(560px, calc(100dvh - 48px))",
      ].join("; ");
      return;
    }

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

  function visibleLabelPickerAnchor(): HTMLDivElement | undefined {
    const anchors = Array.from(document.querySelectorAll<HTMLDivElement>(".label-editor-anchor"));
    return anchors.find((anchor) => {
      const rect = anchor.getBoundingClientRect();
      const style = getComputedStyle(anchor);
      return style.display !== "none" && style.visibility !== "hidden" && rect.width > 0 && rect.height > 0;
    }) ?? anchors[0];
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
      ?? visibleLabelPickerAnchor();
    const launchedFromActionMenu = Boolean(labelPickerAnchor?.closest(".actions-menu-popover"));
    if (event !== undefined && labelPickerOpen) {
      if (launchedFromActionMenu) {
        closeActionMenu();
      }
      closeLabelPicker();
      return;
    }
    labelPickerLaunchedFromActionMenu = launchedFromActionMenu;
    labelPickerAutofocusFilter = event !== undefined && !(window.matchMedia?.("(pointer: coarse)").matches ?? false);
    if (labelPickerLaunchedFromActionMenu) {
      closeActionMenu();
    }
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
        loadOnce: executeGeneratedApiRequest("GET pull request label catalog", (client, signal) =>
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
      operation: "load pull request label catalog",
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
    if (labelGate.unavailable) return;
    if (pendingLabel !== null) return;
    pendingLabel = labelName;
    labelPickerError = null;
    const nextLabels = nextCatalogLabels(labels, labelCatalog, labelName);
    detailStore.setPullLabels(owner, name, number, nextLabels, {
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
    if (pendingLabel !== null || labels.length === 0) return;
    pendingLabel = CLEAR_LABELS_PENDING;
    labelPickerError = null;
    detailStore.setPullLabels(owner, name, number, [], {
      onFailure: (message) => {
        labelPickerError = message;
      },
      onSettled: () => {
        pendingLabel = null;
      },
    });
  }

  function loadUserCandidates(query: string) {
    return executeGeneratedApiRequest("GET pull request user candidates", (client, signal) =>
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
    const host = detailStore.getDetail()?.repo?.platform_host
      ?? detailStore.getDetail()?.platform_host
      ?? platformHost
      ?? "";
    if (login === "" || host === "") return "";
    return `https://${host}/${login}.png?size=40`;
  }

  function onActionMenuKeydown(e: KeyboardEvent): void {
    if (actionMenuOpen && e.key === "Escape") {
      actionMenuOpen = false;
    }
    if (stateMenuOpen && e.key === "Escape") {
      stateMenuOpen = false;
    }
  }

  function isLabelPickerControlTarget(target: Node): boolean {
    if (!(target instanceof Element)) return false;
    return Boolean(
      target.closest(".label-editor-anchor")
      || target.closest(".actions-menu-trigger"),
    );
  }

  function onDocumentMousedown(e: MouseEvent): void {
    const target = e.target as Node;
    if (actionMenuOpen
      && !actionMenuWrapEl?.contains(target)
      && !workflowActionControlEl?.contains(target)) {
      closeActionMenu();
    }
    if (stateMenuOpen && !stateMenuWrapEl?.contains(target)) {
      closeStateMenu();
    }
    if (labelPickerOpen) {
      if (
        !labelPickerPopover?.contains(target)
        && !labelPickerAnchor?.contains(target)
        && !isLabelPickerControlTarget(target)
      ) {
        closeLabelPicker();
      }
    }
  }

  // Re-checks the identity at call time (not just at the caller's check)
  // before refetching: refetchDetailForIdentity is fired without an
  // await on its result, so the selection can move on in the interim.
  function refetchDetailForIdentity(identity: WorkspaceItemIdentity): void {
    if (!identityEquals(identity, $state.snapshot(itemIdentity))) return;
    detailStore.refreshDetailOnly(owner, name, number, {
      provider,
      platformHost,
      repoPath,
    });
  }

  function createWorkspace(launchTargetKey?: string): void {
    if (stalePR) return;
    const detail = detailStore.getDetail();
    if (!detail) return;
    const requestIdentity = $state.snapshot(itemIdentity);
    // A create for this item is already in flight somewhere (this
    // instance before a round-trip, or a predecessor before a remount).
    if (isWorkspaceCreatePending(requestIdentity)) return;
    const requestGen = ++wsRequestGen;
    // The identity comparison also covers the microtask gap where props
    // already moved to a new item but the route-reset effect (which bumps
    // the generation) hasn't flushed yet.
    const identityLeft = () =>
      requestGen !== wsRequestGen ||
      !identityEquals(requestIdentity, $state.snapshot(itemIdentity));
    const responseIsStale = () => componentDestroyed || identityLeft();
    const requestBody = {
      provider: requestIdentity.provider,
      platform_host: detail.platform_host,
      owner: detail.repo_owner,
      name: detail.repo_name,
      mr_number: detail.merge_request.Number,
    };

    wsCreating = true;
    beginWorkspaceCreate(requestIdentity, launchTargetKey);
    const program = executeGeneratedApiRequest("POST pull request workspace", (client, signal) =>
      client.WorkspacesService.createWorkspace(requestBody, { signal }),
    ).pipe(
      Effect.flatMap((data) =>
        Effect.sync(() => {
          if (data?.id) {
            // Publish the confirmed creation to identity-scoped shared state
            // BEFORE any liveness guard: the workspace exists server-side
            // even when the selection moved on or this component unmounted.
            const createdRef = {
              id: data.id,
              status: data.status ?? "provisioning",
            };
            promoteWorkspaceCreateLaunch(requestIdentity, createdRef.id, undefined);
            recordWorkspaceCreated(requestIdentity, createdRef);
            inlineWorkspace?.recordCreated(requestIdentity, createdRef);
          }
          if (responseIsStale() || !data?.id) return;
          if (inlineWorkspace) {
            refetchDetailForIdentity(requestIdentity);
          } else if (onOpenWorkspace) {
            // Hosts without an inline controller (phone-like PR routes)
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
          if (!responseIsStale()) wsCreating = false;
        }),
      ),
    );
    runtime.runCommand(program, {
      operation: "create pull request workspace",
      safeContext: {
        provider: requestIdentity.provider,
        owner: requestIdentity.owner,
        name: requestIdentity.name,
        number: requestIdentity.number,
      },
      onFailure: (failure) => {
        if (responseIsStale()) return;
        showFlash(
          failure._tag === "ApiProblemError"
            ? failure.problem.detail ?? failure.problem.title ?? "failed to create workspace"
            : "Could not reach Kenn Forge",
          { tone: "danger" },
        );
      },
    });
  }

  // Task-list checkbox clicks update the body locally for instant
  // feedback, then debounce a PATCH so a flurry of clicks collapses
  // into a single save. Avoids GitHub-style per-click blocking saves.
  // The target (owner/name/number) AND the body to save are captured
  // when scheduling so a route change before the debounce settles can't
  // redirect the save or lose the edit.
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
        operation: "debounce pull request task body save",
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
    detailStore.savePRBodyInBackground(
      {
        provider: target.provider,
        platformHost: target.platformHost,
        owner: target.owner,
        name: target.name,
        repoPath: target.repoPath,
      },
      target.number,
      target.body,
    );
  }

  function onBodyClick(event: MouseEvent): void {
    const target = event.target as HTMLElement | null;
    if (!target) return;
    if (target.tagName !== "INPUT") return;
    if ((target as HTMLInputElement).type !== "checkbox") return;
    const raw = target.getAttribute("data-task-index");
    if (raw === null) return;
    if (stalePR || !currentCapabilities().state_mutation || contentGate.unavailable) {
      event.preventDefault();
      return;
    }
    const index = parseInt(raw, 10);
    if (Number.isNaN(index)) return;
    const mr = currentPR();
    if (!mr) return;
    const newBody = toggleTaskListItem(mr.Body, index);
    if (newBody === mr.Body) return;
    // We manage state ourselves; let the visual flip persist via the
    // optimistic store update rather than the browser's default toggle
    // (which would race with our re-render).
    event.preventDefault();
    detailStore.setLocalPRBody(
      provider, platformHost, owner, name, number, newBody,
    );
    scheduleBodySave(newBody);
  }

  // Drag-to-reorder for task-list items. The handle (rendered by the
  // markdown layer as `<span class="task-drag-handle" draggable>`) is
  // the drag source; the enclosing `<li class="task-list-item">` is
  // the drop target. Drop position relative to the target's vertical
  // midpoint decides before/after placement.
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
    if (stalePR || !currentCapabilities().state_mutation) return;
    const target = event.target as HTMLElement | null;
    if (!target?.classList?.contains("task-drag-handle")) return;
    const raw = target.getAttribute("data-task-index");
    if (raw === null) return;
    const idx = parseInt(raw, 10);
    if (Number.isNaN(idx)) return;
    dragSourceIndex = idx;
    if (event.dataTransfer) {
      event.dataTransfer.effectAllowed = "move";
      // Firefox requires a non-empty payload to start a drag.
      event.dataTransfer.setData("text/plain", String(idx));
    }
  }

  function onBodyDragOver(event: DragEvent): void {
    if (dragSourceIndex === null) return;
    const target = event.target as HTMLElement | null;
    const idx = findTaskItemIndex(target);
    if (idx === null) return;
    event.preventDefault(); // allow drop
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
    if (stalePR || !currentCapabilities().state_mutation || contentGate.unavailable) return;
    const mr = currentPR();
    if (!mr) return;
    // "before X" with from < X means landing one slot earlier than X
    // after the splice; "after X" means landing on X. Adjust target.
    let target = to;
    if (from < to && side === "before") target = to - 1;
    else if (from > to && side === "after") target = to + 1;
    if (target === from) return;
    const newBody = moveTaskListItem(mr.Body, from, target);
    if (newBody === mr.Body) return;
    detailStore.setLocalPRBody(
      provider, platformHost, owner, name, number, newBody,
    );
    scheduleBodySave(newBody);
  }

  function onBodyDragEnd(event: DragEvent): void {
    clearDragState(event.currentTarget as HTMLElement);
  }

  function clearDragState(root?: HTMLElement | null): void {
    dragSourceIndex = null;
    dropTargetIndex = null;
    dropTargetSide = "before";
    if (root) clearDropIndicatorClasses(root);
  }

  function loadDiffSummaryFiles() {
    return executeGeneratedApiRequest("GET pull request diff summary files", (client, signal) =>
      providerUsesHostRoute(routeRef) ? client.PullRequestsService.getPullFilesOnHost({ ...providerHostRouteParams(routeRef), number: number }, { signal }) : client.PullRequestsService.getPullFiles({ ...providerRouteParams(routeRef), number: number }, { signal }),
    ).pipe(
      retryIdempotentRead,
      Effect.map((data) => new DiffSummaryFilesResult(data.stale ?? true, data.files ?? [])),
      Effect.mapError((failure) =>
        new Error(
          failure._tag === "ApiProblemError"
            ? failure.problem.detail ?? failure.problem.title ?? "failed to load changed files"
            : "Could not reach Kenn Forge",
        ),
      ),
    );
  }
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
        operation: "copy pull request body",
        safeContext: { owner, name, number },
        onFailure: () => {},
      },
    );
  }

  $effect(() => {
    // The component is reused across item navigation; the copied feedback
    // (and its pending reset timer) belongs to the item it was copied from.
    void [provider, platformHost, owner, name, number];
    branchCopySeq++;
    branchCopyExecution?.interrupt();
    branchCopyExecution = undefined;
    copiedBranch = null;
    bodyCopySeq++;
    bodyCopyExecution?.interrupt();
    bodyCopyExecution = undefined;
    bodyCopied = false;
  });
</script>

<svelte:window onkeydown={onActionMenuKeydown} />
<svelte:document onmousedown={onDocumentMousedown} />

{#if detailStore.isDetailLoading() && (detailStore.getDetail() === null || (stalePR && hideStaleWhileLoading))}
  <div class="state-center"><p class="state-msg">Loading…</p></div>
{:else if detailStore.getDetailError() !== null && (detailStore.getDetail() === null || (stalePR && hideStaleWhileLoading))}
  <div class="state-center"><p class="state-msg state-msg--error">Error: {detailStore.getDetailError()}</p></div>
{:else}
  {@const detail = detailStore.getDetail()}
  {@const staleLoadError = stalePR && detailStore.getDetailError() !== null}
  {#if detail !== null}
    {@const pr = detail.merge_request}
    {@const capabilities = detail.repo?.capabilities ?? defaultProviderCapabilities}
    {@const canReadyAction = pr.State === "open" && pr.IsDraft && capabilities.ready_for_review}
    {@const canApproveAction = pr.State === "open" && capabilities.review_mutation}
    {@const canWorkflowsAction = pr.State === "open"
      && capabilities.workflow_approval
      && workflowApproval?.checked === true
      && workflowApproval.required === true}
    {@const canMergeAction = pr.State === "open"
      && repoSettings !== null
      && hasEnabledMergeMethod(repoSettings)
      && (repoOperations?.merge_pr !== undefined
        || (capabilities.merge_mutation && repoSettings.viewerCanMerge))}
    {@const canCloseAction = pr.State === "open" && capabilities.state_mutation}
    {@const canReopenAction = pr.State === "closed" && capabilities.state_mutation}
    {@const lockedSupported = supportsLocked(
      detail.repo?.provider ?? provider,
      detail.repo?.platform_host ?? detail.platform_host,
      detail.repo?.owner ?? owner,
      detail.repo?.name ?? name,
    )}
    <div class="pull-detail-wrap" {@attach loadWorkflowCatalog(workflowCatalogDemandEnabled ? routeRef : null)}>
      {#if staleLoadError}
        <div class="detail-load-error" data-testid="detail-load-error">
          Couldn't load this pull request: {detailStore.getDetailError()}
        </div>
      {/if}
      {#if !hideTabs}
        <div class="detail-tabs" role="tablist" aria-label="Pull request detail">
          <button
            type="button"
            class="detail-tab"
            class:detail-tab--active={activeTab === "conversation"}
            role="tab"
            aria-selected={activeTab === "conversation"}
            tabindex={activeTab === "conversation" ? 0 : -1}
            onclick={() => { activeTab = "conversation"; }}
            onkeydown={handleDetailTabKeydown}
          >
            Conversation
          </button>
          <button
            type="button"
            class="detail-tab"
            class:detail-tab--active={activeTab === "files"}
            role="tab"
            aria-selected={activeTab === "files"}
            tabindex={activeTab === "files" ? 0 : -1}
            onclick={() => { activeTab = "files"; }}
            onkeydown={handleDetailTabKeydown}
          >
            Files changed
            {#if pr.Additions > 0}
              <span class="files-stat files-stat--add">+{pr.Additions}</span>
            {/if}
            {#if pr.Deletions > 0}
              <span class="files-stat files-stat--del">-{pr.Deletions}</span>
            {/if}
          </button>
        </div>
      {/if}
      {#if !hideTabs && activeTab === "files"}
        <DiffFilesLayout
          {provider}
          {platformHost}
          {owner}
          {name}
          {repoPath}
          {number}
          diffHeadSHA={detail.diff_head_sha}
          {capabilities}
          operations={repoOperations}
          reviewThreads={reviewThreadsFromEvents(detail.events)}
        />
      {:else}
        <ScrollBox
          label="Pull request conversation"
          bind:viewport={pullDetailScroller}
          onscroll={handlePullDetailScroll}
        >
        <div class="pull-detail">
          {#if phonePresentation && detailStore.isDetailSyncing() && !manualRefreshPending}
            <!-- Phones get a top progress bar instead of the inline "Syncing" row:
                 the row wraps under the branch line and shifts the whole page. -->
            <div class="sync-bar" role="status" aria-label="Syncing from GitHub"></div>
          {/if}
          <div
            class="pull-detail-content"
            class:pull-detail-content--has-compact-actions={showActionSurface}
            class:pull-detail-content--actions-menu={!phonePresentation && hasPrimaryPRActions && primaryActionStage === 2}
          >
            {#snippet labelActionButton(iconSize = 16)}
              <Button
                class="btn--labels"
                label="Labels"
                shortLabel="Labels"
                size="sm"
                surface="soft"
                tone="neutral"
                disabled={stalePR || labelGate.unavailable}
                title={labelGate.unavailable ? labelGate.reason : undefined}
                onclick={openLabelPicker}
              >
                <TagsIcon size={iconSize} aria-hidden="true" />
              </Button>
            {/snippet}

      <!-- Header -->
      <div class="detail-header">
        {#if editingTitle}
          <div class="title-edit">
            <!-- svelte-ignore a11y_autofocus -->
            <input
              type="text"
              class="title-edit-input"
              bind:value={titleDraft}
              onkeydown={onTitleKeydown}
              disabled={savingTitle}
              autofocus
            />
            <button
              class="title-edit-save"
              onclick={saveTitle}
              disabled={savingTitle || !titleDraft.trim() || contentGate.unavailable}
              title={contentGate.unavailable ? contentGate.reason : undefined}
            >
              {savingTitle ? "Saving..." : "Save"}
            </button>
            <button
              class="title-edit-cancel"
              onclick={cancelEditTitle}
              disabled={savingTitle}
            >
              Cancel
            </button>
          </div>
        {:else if capabilities.state_mutation}
          <div class="title-line">
            <h2 class="detail-title">{pr.Title}</h2>
            {#if !stalePR}
              <button
                class="edit-title-btn"
                onclick={startEditTitle}
                disabled={contentGate.unavailable}
                title={contentGate.unavailable ? contentGate.reason : undefined}
              >Edit</button>
            {/if}
            {#if !uiConfig.hideStar && !stalePR}
              <button
                class="star-btn"
                onclick={handleStarClick}
                title={pr.Starred ? "Unstar" : "Star"}
              >
                {#if pr.Starred}
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
            <a class="gh-link" href={pr.URL} target="_blank" rel="noopener noreferrer" title="Open on GitHub">
              <svg width="14" height="14" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg">
                <path d="M6 3H3a1 1 0 0 0-1 1v9a1 1 0 0 0 1 1h9a1 1 0 0 0 1-1v-3" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>
                <path d="M10 2h4v4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
                <path d="M8 8L14 2" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>
              </svg>
            </a>
          </div>
          {#if !stalePR && !phonePresentation}
            <SelectDropdown
              class="kanban-select kanban-select--header kanban-select--{pr.KanbanStatus.replace('_', '-')}"
              value={pr.KanbanStatus}
              options={kanbanOptions}
              onchange={onKanbanChange}
              title="Change workflow status"
            />
          {/if}
        {/if}
      </div>

      <!-- Meta row -->
      <div class="meta-row">
        <span class="meta-item">{detail.repo_owner}/{detail.repo_name}</span>
        <span class="meta-sep">·</span>
        <CopyItemNumber kind="pull" number={pr.Number} url={pr.URL} />
        <span class="meta-sep">·</span>
        <span class="meta-item">{pr.Author}</span>
        <span class="meta-sep">·</span>
        <span class="meta-item">{formatRelativeTime(pr.CreatedAt)}</span>
        {#if pr.HeadBranch}
          <span class="meta-sep meta-sep--branch">·</span>
          <span class="meta-branch">
            <span
              role="button"
              tabindex="0"
              class="branch-name-btn branch-name-btn--head"
              class:branch-name-btn--copied={copiedBranch === pr.HeadBranch}
              title={copiedBranch === pr.HeadBranch ? "Copied!" : "Click to copy"}
              onclick={() => copyBranch(pr.HeadBranch)}
              onkeydown={(event) => onBranchCopyKeydown(event, pr.HeadBranch)}
            >
              <svg class="branch-icon" width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
                <path d="M11.75 2.5a.75.75 0 100 1.5.75.75 0 000-1.5zm-2.25.75a2.25 2.25 0 113 2.122V6c0 .73-.593 1.322-1.325 1.322H9.457A4.377 4.377 0 006.5 8.579V11.128a2.251 2.251 0 11-1.5 0V4.872a2.251 2.251 0 111.5 0v1.836A5.877 5.877 0 0111.175 5.5h.075V5.372A2.25 2.25 0 019.5 3.25zM4.75 12a.75.75 0 100 1.5.75.75 0 000-1.5zM4 3.25a.75.75 0 111.5 0 .75.75 0 01-1.5 0z"/>
              </svg>
              {pr.HeadBranch}
            </span><span class="branch-target">&#8288;
              <span class="branch-arrow">&rarr;</span>
              <button
                class="branch-name-btn"
                class:branch-name-btn--copied={copiedBranch === pr.BaseBranch}
                title={copiedBranch === pr.BaseBranch ? "Copied!" : "Click to copy"}
                onclick={() => copyBranch(pr.BaseBranch)}
              >{pr.BaseBranch}</button>
            </span>
          </span>
        {/if}
        {#if detailStore.isDetailSyncing() && !manualRefreshPending && !phonePresentation}
          <span class="meta-sep meta-sep--sync">·</span>
          <span class="sync-indicator" title="Syncing from GitHub">
            <Spinner size={12} label="Syncing" />
            Syncing
          </span>
        {/if}
      </div>

      <div class="chips-row">
        {#if !(pr.IsLocked && lockedSupported)}
          {@const stateLabel = visiblePRStateLabel(pr)}
          {@const markDraftGate = operationGate(repoOperations?.mark_draft)}
          {@const canOpenStateMenu = pr.State === "open" && !pr.IsDraft && capabilities.draft_mutation}
          <span class="state-menu-wrap" bind:this={stateMenuWrapEl}>
            {#snippet stateChevron()}
              <ChevronDownIcon size="12" strokeWidth="2.2" aria-hidden="true" />
            {/snippet}
            <Chip size="sm"
              tone={visiblePRState(pr) === "merged"
                ? "merged"
                : visiblePRState(pr) === "closed"
                  ? "danger"
                  : visiblePRState(pr) === "draft"
                    ? "warning"
                    : "success"}
              interactive={canOpenStateMenu}
              disabled={stateSubmitting || stalePR || markDraftGate.unavailable}
              expanded={canOpenStateMenu ? stateMenuOpen : undefined}
              title={markDraftGate.unavailable ? markDraftGate.reason : undefined}
              ariaLabel={canOpenStateMenu ? `State: ${stateLabel}` : undefined}
              onclick={canOpenStateMenu ? toggleStateMenu : undefined}
              trailing={canOpenStateMenu ? (stateChevron as ChipTrailing) : undefined}
            >
              {stateLabel}
            </Chip>
            {#if canOpenStateMenu && stateMenuOpen}
              <div class="state-menu" role="menu" aria-label="Pull request state">
                <button
                  type="button"
                  role="menuitem"
                  class="state-menu-item"
                  disabled={stateSubmitting || markDraftGate.unavailable}
                  title={markDraftGate.unavailable ? markDraftGate.reason : undefined}
                  onclick={() => chooseState("draft")}
                >
                  Draft
                </button>
              </div>
            {/if}
          </span>
        {/if}
        {#if pr.IsLocked && lockedSupported}
          <Chip size="sm" tone="warning" title="This pull request is locked">Locked</Chip>
        {/if}
        <CIStatus
          status={pr.CIStatus}
          checksJSON={pr.CIChecksJSON}
          detailLoaded={detailStore.getDetailLoaded()}
          detailSyncing={detailStore.isDetailSyncing()}
          owner={owner}
          name={name}
          number={pr.Number}
          prKey={pr.PlatformExternalID}
          expanded={expandedPanel === "ci"}
          ontoggle={(next) => { expandedPanel = next ? "ci" : null; }}
          showPanel={false}
        />
        <MergeWarningsChip
          warnings={mergeWarningEntries(pr, detail.warnings, stalePR)}
          pullURL={pr.URL}
          providerLabel={providerDisplayLabel(detail.repo?.provider ?? provider)}
          expanded={expandedPanel === "merge"}
          ontoggle={(next) => { expandedPanel = next ? "merge" : null; }}
        />
        <StackStatus
          {owner}
          {name}
          {number}
          {provider}
          {platformHost}
          {repoPath}
          initialStack={detail.stack ?? null}
          expanded={expandedPanel === "stack"}
          ontoggle={(next) => { expandedPanel = next ? "stack" : null; }}
          onmembernavigate={(ref) => {
            keepStackExpandedOnRouteChange = true;
            expandedPanel = "stack";
            return onStackMemberNavigate?.(ref);
          }}
        />
        {#if pr.ReviewDecision}
          <ReviewDecisionChip
            decision={pr.ReviewDecision}
            events={detail.events}
          />
        {/if}
        {#if pr.Additions > 0 || pr.Deletions > 0}
          <DiffSummaryChip
            additions={pr.Additions}
            deletions={pr.Deletions}
            summaryKey={buildDiffSummaryKey(owner, name, number, detail, pr)}
            loadFiles={loadDiffSummaryFiles}
          />
        {/if}
        {#if hasWorktreeLinks}
          <Chip size="sm" tone="workspace">Worktree</Chip>
        {/if}
        <LabelRow {labels} />
        {#if capabilities.read_labels && capabilities.label_mutation}
          <div class="label-editor-anchor label-editor-anchor--inline">
            {@render labelActionButton()}
          </div>
        {/if}
        <UserListEditor
          label="Assignees"
          users={prAssignees}
          canEdit={capabilities.assignee_mutation}
          disabled={stalePR || assigneeGate.unavailable}
          disabledReason={assigneeGate.unavailable ? assigneeGate.reason : undefined}
          loadCandidates={loadUserCandidates}
          avatarUrlForUser={userAvatarURL}
          onchange={(next, callbacks) => detailStore.setPullAssignees(owner, name, number, next, callbacks)}
        >
          {#snippet icon()}
            <UsersIcon size={12} aria-hidden="true" />
          {/snippet}
        </UserListEditor>
        <UserListEditor
          label="Reviewers"
          users={prReviewers}
          canEdit={capabilities.reviewer_mutation}
          disabled={stalePR || reviewerGate.unavailable}
          disabledReason={reviewerGate.unavailable ? reviewerGate.reason : undefined}
          tooltipNote="User review requests only; team requests are not shown"
          loadCandidates={loadUserCandidates}
          avatarUrlForUser={userAvatarURL}
          onchange={(next, callbacks) => detailStore.setPullReviewers(owner, name, number, next, callbacks)}
        >
          {#snippet icon()}
            <UserCheckIcon size={12} aria-hidden="true" />
          {/snippet}
        </UserListEditor>
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
        <CIStatus
          status={pr.CIStatus}
          checksJSON={pr.CIChecksJSON}
          detailLoaded={detailStore.getDetailLoaded()}
          detailSyncing={detailStore.isDetailSyncing()}
          owner={owner}
          name={name}
          number={pr.Number}
          prKey={pr.PlatformExternalID}
          expanded={expandedPanel === "ci"}
          showButton={false}
        />
      </div>

      {#if !stalePR && !phonePresentation}
        <SelectDropdown
          class="kanban-select kanban-select--below-chips kanban-select--{pr.KanbanStatus.replace('_', '-')}"
          value={pr.KanbanStatus}
          options={kanbanOptions}
          onchange={onKanbanChange}
          title="Change workflow status"
        />
      {/if}


      {#snippet readyAction(compactLabels = false)}
        {@const readyGate = operationGate(repoOperations?.mark_ready_for_review)}
        <ReadyForReviewButton
          {owner}
          {name}
          {number}
          {provider}
          {platformHost}
          {repoPath}
          size="sm"
          compactLabel={compactLabels}
          disabled={stalePR || readyGate.unavailable}
          title={readyGate.unavailable ? readyGate.reason : undefined}
          oncompleted={closeActionMenu}
        />
      {/snippet}

      {#snippet approveAction(compactLabels = false)}
        {@const approveGate = operationGate(repoOperations?.submit_review)}
        <ApproveButton
          {owner}
          {name}
          {number}
          {provider}
          {platformHost}
          {repoPath}
          size="sm"
          disabled={stalePR || headActionsBlocked || approveGate.unavailable}
          expectedHeadSha={detailHeadSha}
          platformHeadSha={latestPlatformHeadSha}
          requireHeadPin={capabilities.mutation_head_binding}
          supportedReviewActions={capabilities.supported_review_actions ?? []}
          routeGeneration={mutationRouteGeneration}
          title={approveGate.unavailable ? approveGate.reason : undefined}
          onheadconflict={handleHeadConflict}
        />
      {/snippet}

      {#snippet workflowsAction(compactLabels = false)}
        {@const workflowGate = operationGate(repoOperations?.approve_workflow)}
        <ApproveWorkflowsButton
          {owner}
          {name}
          {number}
          {provider}
          {platformHost}
          {repoPath}
          count={workflowApproval?.count ?? 0}
          size="sm"
          compactLabel={compactLabels}
          disabled={stalePR || workflowGate.unavailable}
          title={workflowGate.unavailable ? workflowGate.reason : undefined}
          oncompleted={closeActionMenu}
        />
      {/snippet}

      {#snippet mergeAction(compactLabels = false)}
        {@const mergeOp = repoOperations?.merge_pr}
        {@const mergeGate = operationGate(mergeOp)}
        {@const mergeOpUnavailable = mergeGate.unavailable}
        {#if repoSettings && hasEnabledMergeMethod(repoSettings) && (mergeOp !== undefined
            || (capabilities.merge_mutation && repoSettings.viewerCanMerge))}
          {@const mergeSettings = repoSettings}
          {@const mergeDisabledByConflicts = hasMergeConflicts(pr)}
          {@const mergeTitle = midStackMergeBlocked
            ? `Merge #${midStackBlocker?.number ?? "the bottom branch"} first; mid-stack merges are disabled in settings`
            : mergeDisabledByConflicts
              ? "Resolve merge conflicts before merging"
            : stateConflict
              ? "Refresh and re-review the pull request before merging"
              : headPinMissing
              ? "The reviewed head commit has not been synced yet; merging is disabled until the next sync records it"
              : mergeOpUnavailable
                ? mergeGate.reason
                : deferredMergePending
                  ? "A background merge is queued to run if its pending CI checks pass. Click to merge immediately; close the pull request to cancel."
                  : ""}
          <Button
            class={deferredMergePending ? "btn--merge btn--merge-queued" : "btn--merge"}
            disabled={stalePR || midStackMergeBlocked || mergeDisabledByConflicts || mergeOpUnavailable || headActionsBlocked || headPinMissing}
            title={mergeTitle}
            onclick={() => {
              if (stalePR || midStackMergeBlocked || mergeOpUnavailable || headActionsBlocked || headPinMissing) return;
              runOpenMerge(buildOpenMergeInput(pr, capabilities));
            }}
            tone="success"
            surface={deferredMergePending ? "soft" : "solid"}
            size="sm"
            ariaLabel={compactLabels
              ? deferredMergePending
                ? "Merge queued"
                : mergeActionLabel(mergeSettings)
              : undefined}
            label={compactLabels
              ? deferredMergePending
                ? "Queued"
                : mergeActionShortLabel(mergeSettings)
              : deferredMergePending
                ? "Merge queued"
                : mergeActionLabel(mergeSettings)}
          >
            {#if deferredMergePending}
              <ClockIcon size="14" strokeWidth="2.2" aria-hidden="true" />
            {:else}
              <GitMergeIcon size="14" strokeWidth="2.2" aria-hidden="true" />
            {/if}
            {#snippet trailing()}
              {#if !deferredMergePending && mergeActionHasMenu(mergeSettings)}
                <ChevronDownIcon size="13" strokeWidth="2.2" aria-hidden="true" />
              {/if}
            {/snippet}
          </Button>
        {/if}
      {/snippet}

      {#snippet closeAction(compactLabels = false)}
        {@const closeGate = operationGate(repoOperations?.close_pr)}
        <Button
          class="btn--close"
          disabled={stateSubmitting || stalePR || closeGate.unavailable}
          title={closeGate.unavailable ? closeGate.reason : undefined}
          onclick={() => {
            if (stalePR || closeGate.unavailable) return;
            closeActionMenu();
            handleStateChange("closed");
          }}
          tone="danger"
          surface="outline"
          size="sm"
          label={stateSubmitting ? "Closing..." : "Close"}
        >
          <XIcon size="14" strokeWidth="2.2" aria-hidden="true" />
        </Button>
      {/snippet}

      {#snippet reopenAction(compactLabels = false)}
        {@const reopenGate = operationGate(repoOperations?.reopen_pr)}
        <Button
          class="btn--reopen"
          disabled={stateSubmitting || stalePR || reopenGate.unavailable}
          title={reopenGate.unavailable ? reopenGate.reason : undefined}
          onclick={() => {
            if (stalePR || reopenGate.unavailable) return;
            closeActionMenu();
            handleStateChange("open");
          }}
          tone="success"
          surface="solid"
          size="sm"
          label={stateSubmitting ? "Reopening..." : "Reopen"}
        >
          <RefreshCwIcon size="14" strokeWidth="2.2" aria-hidden="true" />
        </Button>
      {/snippet}

      {#snippet primaryActionButtons(compactLabels = false)}
        {#if canReadyAction}{@render readyAction(compactLabels)}{/if}
        {#if canApproveAction}{@render approveAction(compactLabels)}{/if}
        {#if canWorkflowsAction}{@render workflowsAction(compactLabels)}{/if}
        {#if canMergeAction}{@render mergeAction(compactLabels)}{/if}
        {#if canCloseAction}{@render closeAction(compactLabels)}{/if}
        {#if canReopenAction}{@render reopenAction(compactLabels)}{/if}
      {/snippet}

      {#snippet workspaceActionButton(compactLabels = false)}
        {#if workspace}
          {#if inlineWorkspace}
            <Button
              class="btn--workspace"
              disabled={stalePR || workspaceDeletionLifecycle}
              onclick={() => {
                if (stalePR || workspaceDeletionLifecycle) return;
                closeActionMenu();
                inlineWorkspace.focusTerminal();
              }}
              tone="info"
              surface="soft"
              size="sm"
              ariaLabel={compactLabels ? "Focus Terminal" : undefined}
              label={compactLabels ? "Terminal" : "Focus Terminal"}
            >
              <MonitorUpIcon size="14" strokeWidth="2.2" aria-hidden="true" />
            </Button>
            <Button
              class="btn--workspace-secondary"
              disabled={stalePR}
              onclick={() => {
                if (stalePR) return;
                closeActionMenu();
                if (workspaceDeletionLifecycle) {
                  navigate("/workspaces");
                } else {
                  inlineWorkspace.openInWorkspaces(workspace);
                }
              }}
              tone="neutral"
              surface="soft"
              size="sm"
              ariaLabel={compactLabels
                ? workspaceDeletionLifecycle ? "View in Workspaces" : "Open in Workspaces"
                : undefined}
              label={workspaceDeletionLifecycle
                ? compactLabels ? "Workspaces" : "View in Workspaces"
                : compactLabels ? "Workspaces" : "Open in Workspaces"}
            >
              <ExternalLinkIcon size="14" strokeWidth="2.2" aria-hidden="true" />
            </Button>
          {:else}
            <Button
              class="btn--workspace"
              disabled={stalePR}
              onclick={() => {
                if (stalePR) return;
                closeActionMenu();
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
              size="sm"
              ariaLabel={compactLabels
                ? workspaceDeletionLifecycle ? "View in Workspaces" : "Open Workspace"
                : undefined}
              label={workspaceDeletionLifecycle
                ? compactLabels ? "Workspaces" : "View in Workspaces"
                : compactLabels ? "Workspace" : "Open Workspace"}
            >
              <MonitorUpIcon size="14" strokeWidth="2.2" aria-hidden="true" />
            </Button>
          {/if}
        {:else}
          <WorkspaceCreateSplitButton
            label="Create Workspace"
            busyLabel="Creating..."
            size="sm"
            launchTargets={settings.getLaunchTargets()}
            busy={wsCreateBlocked}
            disabled={stalePR}
            disabledReason={stalePR
              ? "Refresh details before creating a workspace."
              : createWorkspaceTitle}
            descriptionId={createWorkspaceDescriptionId}
            onCreate={createWorkspace}
          />
        {/if}
      {/snippet}
      {#snippet workflowActionsMenu(floating: boolean)}
        <section
          class={[
            "workflow-actions-menu",
            {
              "actions-menu-popover workflow-actions-menu--floating": floating,
            },
          ]}
          aria-label={workflowProviderLabel}
        >
          <div class="workflow-actions-menu__label">{workflowProviderLabel}</div>
          {#each workflowCatalog as workflow (workflow.id)}
            <button
              type="button"
              class="workflow-actions-menu__item"
              disabled={!workflow.available}
              title={!workflow.available ? workflow.unavailable_reason || "Unavailable" : undefined}
              onclick={() => openWorkflowDialog(workflow)}
            >
              <WorkflowIcon size="14" strokeWidth="2.2" aria-hidden="true" />
              <span>{workflow.name}</span>
            </button>
          {/each}
        </section>
      {/snippet}
      {#snippet workflowDispatchAction(compactLabels = false)}
        <div class="workflow-actions-control" bind:this={workflowActionControlEl}>
          <button
            {@attach captureActionMenuTrigger}
            type="button"
            class="actions-menu-trigger"
            aria-haspopup="true"
            aria-expanded={actionMenuOpen}
            onclick={() => { actionMenuOpen = !actionMenuOpen; }}
          >
            <WorkflowIcon size="14" strokeWidth="2.2" aria-hidden="true" />
            <span>{compactLabels ? "Workflow" : "Run workflow"}</span>
            <ChevronDownIcon size="14" strokeWidth="2.2" aria-hidden="true" />
          </button>
          {#if actionMenuOpen}
            {@render workflowActionsMenu(true)}
          {/if}
        </div>
      {/snippet}

      {#snippet measuredPrimaryActions(compactLabels = false)}
        <div
          class="actions-row actions-row--primary actions-row--measure"
          aria-hidden="true"
          inert
        >
          {#if pr.State === "open"}
            {#if pr.IsDraft && capabilities.ready_for_review}
              <Button
                size="sm"
                label={compactLabels ? "Ready" : "Ready for review"}
              >
                <SendHorizontalIcon size="14" strokeWidth="2.2" aria-hidden="true" />
              </Button>
            {/if}
            {#if capabilities.review_mutation}
              <Button size="sm" label="Approve">
                <CheckIcon size="14" strokeWidth="2.4" aria-hidden="true" />
              </Button>
            {/if}
            {#if capabilities.workflow_approval && workflowApproval?.checked && workflowApproval.required}
              {@const count = workflowApproval.count ?? 0}
              <Button
                size="sm"
                label={compactLabels
                  ? count > 1
                    ? `Workflows (${count})`
                    : "Workflows"
                  : count > 1
                    ? `Approve workflows (${count})`
                    : "Approve workflows"}
              >
                <WorkflowIcon size="14" strokeWidth="2.2" aria-hidden="true" />
              </Button>
            {/if}
            {@const mergeOp = repoOperations?.merge_pr}
            {#if repoSettings && (mergeOp !== undefined
                || (capabilities.merge_mutation && repoSettings.viewerCanMerge))}
              <Button
                size="sm"
                label={compactLabels
                  ? deferredMergePending
                    ? "Queued"
                    : mergeActionShortLabel(repoSettings)
                  : deferredMergePending
                    ? "Merge queued"
                    : mergeActionLabel(repoSettings)}
              >
                {#if deferredMergePending}
                  <ClockIcon size="14" strokeWidth="2.2" aria-hidden="true" />
                {:else}
                  <GitMergeIcon size="14" strokeWidth="2.2" aria-hidden="true" />
                {/if}
                {#snippet trailing()}
                  {#if !deferredMergePending && repoSettings !== null && mergeActionHasMenu(repoSettings)}
                    <ChevronDownIcon size="13" strokeWidth="2.2" aria-hidden="true" />
                  {/if}
                {/snippet}
              </Button>
            {/if}
            {#if capabilities.state_mutation}
              <Button
                size="sm"
                label={stateSubmitting ? "Closing..." : "Close"}
              >
                <XIcon size="14" strokeWidth="2.2" aria-hidden="true" />
              </Button>
            {/if}
          {:else if pr.State === "closed" && capabilities.state_mutation}
            <Button
              size="sm"
              label={stateSubmitting ? "Reopening..." : "Reopen"}
            >
              <RefreshCwIcon size="14" strokeWidth="2.2" aria-hidden="true" />
            </Button>
          {/if}
        </div>
      {/snippet}

      {#snippet fullPrimaryActionMeasure()}
        {@render measuredPrimaryActions(false)}
      {/snippet}

      {#snippet compactPrimaryActionMeasure()}
        {@render measuredPrimaryActions(true)}
      {/snippet}

      {#snippet menuPrimaryActionMeasure()}
        <div
          class="actions-row actions-row--primary actions-row--measure"
          aria-hidden="true"
          inert
        >
          <button type="button" class="actions-menu-trigger">
            <span>Actions</span>
            <ChevronDownIcon size="14" strokeWidth="2.2" aria-hidden="true" />
          </button>
        </div>
      {/snippet}

      <!-- Approve / Merge / Close / Reopen actions -->
      {#if !workspace}
        <span id={createWorkspaceDescriptionId} class="kit-sr-only">
          {stalePR ? "Refresh details before creating a workspace." : createWorkspaceTitle}
        </span>
      {/if}
      {#if showActionSurface}
        <div class="primary-actions-wrap">
          {#if phonePresentation}
            {@const phoneActionItems = [
              ...(canReadyAction ? [{ id: "ready", content: readyAction }] : []),
              ...(canApproveAction ? [{ id: "approve", content: approveAction }] : []),
              ...(canWorkflowsAction ? [{ id: "workflows", content: workflowsAction }] : []),
              ...(canMergeAction ? [{ id: "merge", content: mergeAction }] : []),
              ...(canCloseAction ? [{ id: "close", content: closeAction }] : []),
              ...(canReopenAction ? [{ id: "reopen", content: reopenAction }] : []),
              ...(hideWorkspaceAction ? [] : [{ id: "workspace", content: workspaceActionButton }]),
              ...(hasWorkflowActions ? [{ id: "dispatch-workflow", content: workflowDispatchAction }] : []),
            ]}
            <AdaptiveActionGrid
              class="phone-actions-grid"
              items={phoneActionItems}
              ariaLabel="Pull request actions"
              layout="fill"
              frame="none"
              padding={0}
              rowGap={2}
              columnGap={2}
              collapseBelow={0}
            />
          {:else}
            {#if hasPrimaryPRActions}
            <FitStages
              class="primary-actions-fit"
              bind:stage={primaryActionStage}
              onstagechange={(stage) => {
                if (stage !== 2) closeActionMenu();
              }}
              stages={[
                fullPrimaryActionMeasure,
                compactPrimaryActionMeasure,
                menuPrimaryActionMeasure,
              ]}
            />
          {/if}
          <div
            class={[
              "actions-menu-wrap",
              {
                "actions-menu-wrap--menu": hasPrimaryPRActions && primaryActionStage === 2,
              },
            ]}
            bind:this={actionMenuWrapEl}
          >
            <div
              class={[
                "primary-actions-live",
                {
                  "actions-menu-popover": hasPrimaryPRActions && primaryActionStage === 2,
                  "primary-actions-live--open":
                    hasPrimaryPRActions && primaryActionStage === 2 && actionMenuOpen,
                },
              ]}
              aria-hidden={hasPrimaryPRActions && primaryActionStage === 2 && !actionMenuOpen}
              inert={hasPrimaryPRActions && primaryActionStage === 2 && !actionMenuOpen}
            >
              {#if hasPrimaryPRActions}
                <div class="actions-row actions-row--primary">
                  {@render primaryActionButtons(primaryActionStage === 1)}
                </div>
                {#if actionMenuOpen && capabilities.read_labels && capabilities.label_mutation}
                  <div class="actions-menu-popover__item actions-menu-popover__item--labels label-editor-anchor">
                    {@render labelActionButton(14)}
                  </div>
                {/if}
              {/if}
              {#if !hideWorkspaceAction
                || (hasWorkflowActions && (!hasPrimaryPRActions || primaryActionStage !== 2))}
                <div
                  class="actions-row actions-row--utility"
                  class:actions-row--workspace={!hideWorkspaceAction}
                >
                  {#if !hideWorkspaceAction}
                    {@render workspaceActionButton(primaryActionStage === 1)}
                  {/if}
                  {#if hasWorkflowActions && (!hasPrimaryPRActions || primaryActionStage !== 2)}
                    {@render workflowDispatchAction()}
                  {/if}
                </div>
              {/if}
              {#if actionMenuOpen && hasWorkflowActions
                && hasPrimaryPRActions && primaryActionStage === 2}
                {@render workflowActionsMenu(false)}
              {/if}
            </div>
            {#if hasPrimaryPRActions && primaryActionStage === 2}
              <button
                {@attach captureActionMenuTrigger}
                type="button"
                class="actions-menu-trigger"
                aria-haspopup="true"
                aria-expanded={actionMenuOpen}
                onclick={() => { actionMenuOpen = !actionMenuOpen; }}
              >
                <span>Actions</span>
                <ChevronDownIcon size="14" strokeWidth="2.2" aria-hidden="true" />
              </button>
            {/if}
          </div>
          {/if}
          {#if hasPrimaryPRActions}
            {#if stateConflict === "stale_state"}
              <span class="action-error action-error--state" role="status">
                The head commit changed since this pull request was reviewed. Re-review the latest changes before approving or merging.
                {#if headConflictContext}
                  {headConflictContext}.
                {/if}
              </span>
            {:else if stateConflict === "not_open"}
              <span class="action-error action-error--state" role="status">
                This pull request is no longer open. Its current state is being refreshed before any further action.
              </span>
            {:else if stateConflict === "head_repo_unknown"}
              <span class="action-error action-error--state" role="status">
                The head repository is no longer available. Merge is unavailable while the pull request state refreshes.
              </span>
            {:else if headActionsBlocked}
              <span class="action-error action-error--state" role="status">
                The head commit has not been synced yet. After the next sync records it, refresh the reviewed state before approving or merging.
              </span>
            {/if}
            {#if stateConflict}
              <div class="state-conflict-recovery">
                <Button
                  class="btn--conflict-refresh"
                  disabled={conflictRefreshBusy}
                  onclick={() => refreshConflictState()}
                  tone="neutral"
                  surface="soft"
                  size="sm"
                  label={conflictRefreshBusy ? "Refreshing reviewed state..." : "Refresh reviewed state"}
                >
                  <RefreshCwIcon size="14" aria-hidden="true" />
                </Button>
                {#if conflictRefreshError}
                  <span class="action-error" role="alert">{conflictRefreshError}</span>
                {/if}
              </div>
            {/if}
          {/if}
        </div>
      {/if}

      {#if workflowDialogWorkflow && workflowCatalogDemandEnabled}
        <WorkflowDispatchDialog
          open={true}
          workflow={workflowDialogWorkflow}
          environments={workflowActions.getEnvironments(routeRef)}
          initialRef={workflowInitialRef}
          operation={repoOperations?.dispatch_workflow}
          state={workflowDialogPresentation}
          trigger={actionMenuTriggerEl ?? null}
          onsubmit={submitWorkflow}
          onclose={closeWorkflowDialog}
          onreload={reloadWorkflowCatalog}
          onnewcycle={newWorkflowDispatchCycle}
        />
      {/if}

      {#if !hasWorktreeLinks && importAction}
        <div class="actions-row">
          <Button
            class="btn--embedding-action"
            onclick={() => {
              if (stalePR) return;
              importAction.handler({
                surface: "pull-detail", owner, name, number,
              });
            }}
            disabled={stalePR}
            tone="neutral"
            surface="outline"
            size="sm"
          >
            {importAction.label}
          </Button>
        </div>
      {/if}
      {#if hasWorktreeLinks && navigateAction}
        <div class="actions-row">
          {#each worktreeLinks as link (link.worktree_key)}
            <Button
              class="btn--embedding-action"
              onclick={() => {
                if (stalePR) return;
                navigateAction.handler({
                  surface: "pull-detail", owner, name, number,
                  meta: {
                    worktree_key: link.worktree_key,
                    host_key: link.host_key,
                  },
                });
              }}
              disabled={stalePR}
              tone="neutral"
              surface="outline"
              size="sm"
            >
              {navigateAction.label}: {link.worktree_key}
            </Button>
          {/each}
        </div>
      {/if}
      {#if otherActions.length > 0}
        <div class="actions-row">
          {#each otherActions as action (action.id)}
            <Button
              class="btn--embedding-action"
              onclick={() => {
                if (stalePR) return;
                action.handler({
                  surface: "pull-detail", owner, name, number,
                });
              }}
              disabled={stalePR}
              tone="neutral"
              surface="outline"
              size="sm"
            >
              {action.label}
            </Button>
          {/each}
        </div>
      {/if}

      {#if showMergeModal && mergeModalSettings}
        {@const d = detailStore.getDetail()!}
        {@const p = d.merge_request}
        {@const settings = mergeModalSettings}
        <MergeModal
          {owner}
          {name}
          {number}
          {provider}
          {platformHost}
          {repoPath}
          prTitle={p.Title}
          prBody={p.Body}
          prAuthor={p.Author}
          prAuthorDisplayName={p.AuthorDisplayName}
          allowSquash={settings.allowSquash}
          allowMerge={settings.allowMerge}
          allowRebase={settings.allowRebase}
          expectedHeadSha={detailHeadSha}
          requireHeadPin={capabilities.mutation_head_binding}
          routeGeneration={mutationRouteGeneration}
          deferUntilChecksPass={shouldDeferMergeForCI(p.CIStatus, p.CIChecksJSON)}
          alreadyQueued={deferredMergePending}
          workspaceId={d.workspace?.id}
          midStackWarning={midStackBlocker
            ? `This is stack position ${d.stack?.position ?? "?"} of ${d.stack?.size ?? "?"}. Branch #${midStackBlocker.number} below it has not been merged.`
            : undefined}
          onstateconflict={handleStateConflict}
          onclose={() => { showMergeModal = false; }}
          onqueued={() => {
            showMergeModal = false;
            // Pick up deferred_merge_pending so the merge action renders
            // as queued until the background worker completes.
            detailStore.refreshDetailOnly(owner, name, number, {
              provider,
              platformHost,
              repoPath,
            });
          }}
          onmerged={() => {
            showMergeModal = false;
            detailStore.loadDetail(owner, name, number, {
              provider,
              platformHost,
              repoPath,
            });
            pulls.loadPulls();
            activity.loadActivity();
          }}
        />
      {/if}

      <!-- PR body -->
      <div class="section body-section">
        {#if editingBody}
          <div class="section-header">
            <span class="section-title-inline">Description</span>
          </div>
          <div class="body-edit">
            <!-- svelte-ignore a11y_autofocus -->
            <textarea
              class="body-edit-textarea"
              bind:value={bodyDraft}
              onkeydown={onBodyKeydown}
              disabled={savingBody}
              autofocus
            ></textarea>
            <div class="body-edit-actions">
              <button
                class="title-edit-save"
                onclick={saveBody}
                disabled={savingBody || contentGate.unavailable}
                title={contentGate.unavailable ? contentGate.reason : undefined}
              >
                {savingBody ? "Saving..." : "Save"}
              </button>
              <button
                class="title-edit-cancel"
                onclick={cancelEditBody}
                disabled={savingBody}
              >
                Cancel
              </button>
            </div>
          </div>
        {:else if pr.Body}
          {#key descriptionItemKey}
            <CollapsibleDescription
              source={pr.Body}
              copied={bodyCopied}
              oncopy={() => copyBody(pr.Body)}
            >
              {#snippet headerActions()}
                {#if capabilities.state_mutation && !stalePR}
                  <button
                    class="edit-body-btn"
                    onclick={startEditBody}
                    disabled={contentGate.unavailable}
                    title={contentGate.unavailable ? contentGate.reason : undefined}
                  >
                    Edit
                  </button>
                {/if}
              {/snippet}
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
                  raw={pr.Body}
                  repo={{ provider, platformHost, owner, name, repoPath }}
                  options={{
                    interactiveTasks: capabilities.state_mutation && !contentGate.unavailable,
                    collapseSingleLineBreaks: settings.getDetailSettings().collapse_single_line_breaks,
                  }}
                />
              </div>
            </CollapsibleDescription>
          {/key}
        {:else}
          <div class="section-header">
            <span class="section-title-inline">Description</span>
            {#if capabilities.state_mutation && !stalePR}
              <button
                class="edit-body-btn"
                onclick={startEditBody}
                disabled={contentGate.unavailable}
                title={contentGate.unavailable ? contentGate.reason : undefined}
              >
                Edit
              </button>
            {/if}
          </div>
          {#if capabilities.state_mutation && !stalePR}
            <button
              class="add-description-btn"
              onclick={startEditBody}
              disabled={contentGate.unavailable}
              title={contentGate.unavailable ? contentGate.reason : undefined}
            >
              Add a description
            </button>
          {/if}
        {/if}
      </div>

      <!-- Comment box -->
      <div class="section">
        <CommentBox
          {owner}
          {name}
          {number}
          provider={detail.repo.provider}
          platformHost={detail.platform_host}
          repoPath={detail.repo.repo_path}
          disabled={stalePR || !capabilities.comment_mutation || addCommentGate.unavailable}
          disabledReason={addCommentGate.unavailable ? addCommentGate.reason : undefined}
        />
      </div>

      <!-- Activity -->
      <div class="section">
        <div class="section-title-row">
          <h3 class="section-title">Activity</h3>
          <div class="section-title-actions">
            <DetailRefreshButton
              disabled={detailStore.isDetailLoading() || detailStore.isDetailSyncing() || stalePR || sync?.getProviderAvailable() === false}
              disabledReason={sync?.getProviderAvailable() === false ? "Hub unavailable" : undefined}
              refreshing={manualRefreshPending}
              onRefresh={refreshDetail}
            />
            <DetailActivityViewMenu
              viewMode={detailActivityView.getMode()}
              onViewChange={(mode) => detailActivityView.setMode(mode)}
              timelineOrder={detailActivityView.getOrder()}
              onOrderChange={(order) => detailActivityView.setOrder(order)}
              filter={timelineFilter}
              onFilterChange={updateTimelineFilter}
            />
          </div>
        </div>
        {#if detailStore.getDetailLoaded()}
          <EventTimeline
            events={filteredTimelineEvents}
            orderingEvents={timelineEvents}
            {provider}
            {platformHost}
            repoOwner={owner}
            repoName={name}
            {repoPath}
            {number}
            itemType="pull"
            currentHeadSHA={latestPlatformHeadSha}
            canResolveReviewThreads={capabilities.review_thread_resolution && !resolveThreadGate.unavailable}
            canReplyToThreads={capabilities.thread_reply && !stalePR && !replyThreadGate.unavailable}
            filtered={hasActiveTimelineFilters}
            filterScope={timelineFilterScope}
            showCommitDetails={timelineFilter.showCommitDetails}
            activityViewMode={detailActivityView.getMode()}
            timelineOrder={detailActivityView.getOrder()}
            initialEntryLimit={settings.getDetailSettings().initial_timeline_entry_limit}
            itemIdentity={`${provider}:${platformHost ?? ""}:${repoPath}:${number}`}
            onEditComment={capabilities.comment_mutation && !stalePR && !editCommentGate.unavailable
              ? editTimelineComment
              : undefined}
            onDeleteComment={capabilities.comment_mutation && !stalePR && !deleteCommentGate.unavailable
              ? deleteTimelineComment
              : undefined}
            onApplySuggestion={capabilities.review_suggestion_application
              && !stalePR
              && pr.State === "open"
              && !headActionsBlocked
              && !applySuggestionGate.unavailable
                ? applyTimelineSuggestion
                : undefined}
            {jumpToReviewThread}
          />
        {:else if detailStore.isDetailSyncing()}
          <div class="loading-placeholder">
            <Spinner size={14} label="Syncing" />
            Loading discussion...
          </div>
        {:else}
          <div class="loading-placeholder">Detail not yet loaded</div>
        {/if}
      </div>
          </div>
        </div>
        </ScrollBox>
      {/if}
    </div>
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

  .pull-detail-wrap {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
    overflow: hidden;
  }

  .detail-load-error {
    padding: 6px 16px;
    background: var(--accent-red-soft, color-mix(in srgb, var(--accent-red) 12%, transparent));
    color: var(--accent-red);
    border-bottom: 1px solid var(--border-subtle);
    font-size: var(--font-size-sm);
    flex-shrink: 0;
  }

  .pull-detail {
    position: relative;
    padding: 20px 24px;
    display: flex;
    flex-direction: column;
    min-width: 0;
    overflow-x: hidden;
    width: 100%;
  }

  /* Wrap long lines inside fenced code blocks at all widths. The detail
     panel clips horizontal overflow (overflow-x: hidden above), so an
     unwrapped <pre> line in a PR body or comment gets cut off; this view
     also renders in the desktop-width yet equally narrow workspace
     sidebar. Scope to <pre> only -- white-space/overflow-wrap/word-break
     all inherit to the inner <code> -- so inline code keeps the
     table-cell reset in app.css that lets wide tables scroll instead of
     squeezing columns. */
  .pull-detail :global(.markdown-body pre) {
    max-width: 100%;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    word-break: break-word;
  }

  .pull-detail-content {
    container: pull-detail / inline-size;
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
    z-index: 60;
  }

  .detail-header {
    position: relative;
    display: flex;
    align-items: flex-start;
    gap: var(--space-4);
  }

  .title-line {
    display: flex;
    align-items: flex-start;
    gap: var(--space-4);
    flex: 1;
    min-width: 0;
  }

  .detail-title {
    font-size: var(--font-size-xl);
    font-weight: 600;
    color: var(--text-primary);
    line-height: 1.35;
    min-width: 0;
  }

  .edit-title-btn {
    background: none;
    border: none;
    color: var(--text-muted);
    cursor: pointer;
    padding: 0;
    font-size: var(--font-size-2xs);
    flex-shrink: 0;
    margin-top: 3px;
  }

  .edit-title-btn:hover {
    color: var(--accent-blue);
  }

  .title-edit {
    display: flex;
    align-items: center;
    gap: 6px;
    flex: 1;
  }

  .title-edit-input {
    flex: 1;
    font-size: var(--font-size-lg);
    font-weight: 600;
    font-family: var(--font-sans);
    padding: 4px 8px;
    background: var(--bg-inset);
    border: 1px solid var(--accent-blue);
    border-radius: var(--radius-sm);
    color: var(--text-primary);
    outline: none;
  }

  .title-edit-save,
  .title-edit-cancel {
    font-size: var(--font-size-2xs);
    padding: 4px 10px;
    border-radius: var(--radius-sm);
    cursor: pointer;
    white-space: nowrap;
  }

  .title-edit-save {
    background: var(--accent-blue);
    color: var(--text-on-accent);
    border: none;
  }

  .title-edit-save:disabled {
    opacity: var(--opacity-disabled);
    cursor: not-allowed;
  }

  .title-edit-cancel {
    background: transparent;
    color: var(--text-secondary);
    border: 1px solid var(--border-default);
  }

  .title-edit-cancel:disabled {
    opacity: var(--opacity-disabled);
    cursor: not-allowed;
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

  .sync-bar {
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
    height: 2px;
    overflow: hidden;
    pointer-events: none;
    background: color-mix(in srgb, var(--accent-blue) 25%, transparent);
  }

  .sync-bar::before {
    content: "";
    position: absolute;
    top: 0;
    bottom: 0;
    left: 0;
    width: 40%;
    background: var(--accent-blue);
    animation: sync-bar-slide 1.2s ease-in-out infinite;
  }

  @keyframes sync-bar-slide {
    from {
      transform: translateX(-100%);
    }
    to {
      transform: translateX(250%);
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .sync-bar::before {
      animation: none;
      width: 100%;
    }
  }

  .sync-indicator {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: var(--font-size-xs);
    color: var(--accent-blue);
  }

  .meta-branch {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--font-size-sm);
  }

  .branch-icon {
    color: var(--text-muted);
    margin-right: var(--space-1);
    vertical-align: -0.1em;
  }

  .branch-target {
    display: inline-flex;
    align-items: baseline;
    gap: var(--space-1);
    white-space: nowrap;
  }

  .branch-name-btn {
    min-width: 0;
    position: relative;
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    background: none;
    border: none;
    padding: 1px 4px;
    border-radius: 3px;
    cursor: pointer;
    transition: background 0.15s, color 0.15s;
  }

  .branch-name-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .branch-name-btn--copied {
    color: var(--accent-green);
    background: color-mix(
      in srgb, var(--accent-green) 10%, transparent
    );
  }

  .branch-name-btn--copied::after {
    content: "Copied!";
    position: absolute;
    bottom: calc(100% + 6px);
    left: 50%;
    transform: translateX(-50%);
    font-family: inherit;
    font-size: var(--font-size-2xs);
    font-weight: 600;
    letter-spacing: 0.02em;
    color: var(--text-on-accent);
    background: var(--accent-green);
    padding: 2px 8px;
    border-radius: 4px;
    white-space: nowrap;
    pointer-events: none;
    animation: copied-pop 0.2s ease-out;
  }

  @keyframes copied-pop {
    from {
      opacity: 0;
      transform: translateX(-50%) translateY(4px);
    }
    to {
      opacity: 1;
      transform: translateX(-50%) translateY(0);
    }
  }

  .branch-arrow {
    color: var(--text-muted);
  }

  .chips-row {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    min-width: 0;
  }

  .state-menu-wrap {
    position: relative;
    display: inline-flex;
    z-index: 70;
  }

  .state-menu {
    position: absolute;
    z-index: 25;
    top: calc(100% + 6px);
    left: 0;
    min-width: 120px;
    padding: 4px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md, 8px);
    background: var(--bg-surface);
    box-shadow: var(--shadow-lg);
  }

  .state-menu-item {
    width: 100%;
    min-height: 30px;
    padding: 0 10px;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    font-weight: 600;
    text-align: left;
    cursor: pointer;
  }

  .state-menu-item:hover:not(:disabled) {
    background: var(--bg-surface-hover);
  }

  .state-menu-item:disabled {
    color: var(--text-muted);
    cursor: not-allowed;
  }

  .chips-row :global(.btn--labels) {
    min-height: 22px;
    padding: 0 8px;
    border-radius: 8px;
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  .chips-row :global(.btn--labels svg) {
    width: 13px;
    height: 13px;
  }

  :global(.kanban-select) {
    min-width: 150px;
  }

  :global(.kanban-select--header) {
    flex-shrink: 0;
    margin-left: auto;
  }

  :global(.kanban-select--below-chips) {
    display: none;
  }

  :global(.kanban-select--new .kit-select-dropdown__trigger) {
    color: var(--kanban-new);
  }

  :global(.kanban-select--reviewing .kit-select-dropdown__trigger) {
    color: var(--accent-amber);
  }

  :global(.kanban-select--waiting .kit-select-dropdown__trigger) {
    color: var(--accent-purple);
  }

  :global(.kanban-select--awaiting-merge .kit-select-dropdown__trigger) {
    color: var(--accent-green);
  }

  @container pull-detail (max-width: 640px) {
    .detail-header {
      flex-wrap: wrap;
    }

    :global(.kanban-select--header) {
      display: none;
    }

    :global(.kanban-select--below-chips) {
      display: block;
      min-width: min(100%, 150px);
      width: fit-content;
    }
  }

  .primary-actions-wrap {
    position: relative;
    min-width: 0;
  }

  .primary-actions-wrap :global(.primary-actions-fit) {
    position: absolute;
    inset: 0 0 auto; /* kit-ui-check-ignore: zero-height hidden FitStages measurement layer, not an overlay */
    width: 100%;
    height: 0;
    overflow: hidden;
    visibility: hidden;
    pointer-events: none;
  }

  .primary-actions-wrap :global(.actions-row--measure) {
    flex-wrap: nowrap;
  }

  .primary-actions-wrap :global(.actions-row--measure > .actions-menu-trigger) {
    display: inline-flex;
  }

  .actions-row {
    display: flex;
    align-items: flex-start;
    flex-wrap: wrap;
    gap: 8px;
    min-width: 0;
    max-width: 100%;
  }

  .actions-row :global(.approve-section),
  .actions-row :global(.ready-section),
  .actions-row :global(.workflow-approval-section) {
    min-width: 0;
  }

  .actions-row :global(.kit-button) {
    max-width: 100%;
  }

  /* Phone routes: one kit action grid. Kit stretches its own direct controls in
   * grid mode; the compound wrappers here (approve, ready, workflows, create
   * split button) are custom items and need the same fill. */
  .primary-actions-wrap :global(.phone-actions-grid .kit-adaptive-action-grid__item > .approve-section),
  .primary-actions-wrap :global(.phone-actions-grid .kit-adaptive-action-grid__item > .ready-section),
  .primary-actions-wrap :global(.phone-actions-grid .kit-adaptive-action-grid__item > .workflow-approval-section),
  .primary-actions-wrap :global(.phone-actions-grid .kit-adaptive-action-grid__item > .workspace-create-split) {
    width: 100%;
    min-width: 0;
  }

  .primary-actions-wrap :global(.phone-actions-grid.kit-adaptive-action-grid--grid .approve-section > .kit-button),
  .primary-actions-wrap :global(.phone-actions-grid.kit-adaptive-action-grid--grid .ready-section > .kit-button),
  .primary-actions-wrap :global(.phone-actions-grid.kit-adaptive-action-grid--grid .workflow-approval-section > .kit-button),
  .primary-actions-wrap :global(.phone-actions-grid.kit-adaptive-action-grid--grid .workspace-create-split > .create-primary) {
    width: 100%;
  }

  .primary-actions-wrap :global(.phone-actions-grid .kit-button) {
    min-height: var(--detail-mobile-hit-target, 37px);
  }

  .primary-actions-live {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 8px;
  }

  .actions-menu-wrap {
    display: contents;
  }


  .workflow-actions-control {
    position: relative;
    min-width: 0;
  }

  .actions-menu-wrap--menu {
    display: block;
    position: relative;
    z-index: 65;
  }

  .actions-menu-trigger {
    display: none;
    align-items: center;
    gap: 6px;
    min-height: 28px;
    padding: 5px 11px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: 600;
    cursor: pointer;
  }

  .workflow-actions-control > .actions-menu-trigger {
    display: inline-flex;
  }

  .actions-menu-wrap--menu > .actions-menu-trigger {
    display: inline-flex;
  }

  .actions-menu-wrap--menu .primary-actions-live {
    display: none;
  }

  .actions-menu-wrap--menu .primary-actions-live--open {
    display: flex;
  }

  .primary-actions-live > .actions-menu-popover__item {
    display: none;
  }

  .actions-menu-wrap--menu .primary-actions-live > .actions-menu-popover__item {
    display: block;
  }

  .actions-menu-trigger:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .actions-menu-popover {
    position: absolute;
    z-index: 20;
    top: calc(100% + 6px);
    left: 0;
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-width: min(240px, calc(100cqw - 48px));
    max-width: calc(100cqw - 48px);
    padding: 8px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md, 8px);
    background: var(--bg-surface);
    box-shadow: var(--shadow-lg);
  }

  .workflow-actions-menu {
    width: 100%;
    min-width: 0;
    padding-top: var(--space-3);
    border-top: 1px solid var(--border-default);
  }

  .workflow-actions-menu--floating {
    right: auto;
    left: 0;
    width: min(240px, calc(100cqw - 48px));
    padding-top: var(--space-4);
  }

  .workflow-actions-menu__label {
    padding: 0 var(--space-2) var(--space-2);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  .workflow-actions-menu__item {
    display: flex;
    width: 100%;
    align-items: center;
    gap: var(--space-3);
    min-height: 28px;
    padding: var(--space-2) var(--space-4);
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    text-align: left;
    cursor: pointer;
  }

  .workflow-actions-menu__item:hover {
    background: var(--bg-surface-hover);
  }

  .workflow-actions-menu__item:focus-visible {
    outline: 2px solid var(--focus-ring);
    outline-offset: 1px;
  }

  .workflow-actions-menu__item:disabled {
    color: var(--text-muted);
    cursor: not-allowed;
    opacity: var(--opacity-disabled);
  }

  .actions-menu-popover :global(.kit-button) {
    width: 100%;
    justify-content: flex-start;
  }

  .actions-menu-popover__item {
    width: 100%;
    min-width: 0;
  }

  .actions-menu-popover__item--labels {
    position: relative;
  }

  .actions-menu-popover__item--labels.label-editor-anchor {
    width: 100%;
  }

  .actions-menu-popover :global(.approve-section),
  .actions-menu-popover :global(.ready-section),
  .actions-menu-popover :global(.workflow-approval-section) {
    width: 100%;
  }

  .actions-menu-popover :global(.approve-section--open) {
    gap: 8px;
  }

  .actions-menu-popover :global(.approve-popover) {
    position: static;
    width: 100%;
    box-shadow: none;
  }

  .actions-menu-popover :global(.approve-actions) {
    flex-wrap: wrap;
  }

  .actions-menu-popover > .actions-row--primary {
    flex-direction: column;
    width: 100%;
  }

  .pull-detail-content--actions-menu .label-editor-anchor--inline {
    display: none;
  }

  .action-error {
    font-size: var(--font-size-xs);
    color: var(--accent-red, #d73a49);
  }

  .action-error--state {
    display: block;
    margin-top: 6px;
  }

  .state-conflict-recovery {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-top: 6px;
  }

  .section {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .section-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
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

  .section-title-inline {
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

  .files-stat {
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    font-weight: 600;
  }

  .files-stat--add {
    color: var(--accent-green);
  }

  .files-stat--del {
    color: var(--accent-red);
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

  .detail-tabs {
    display: flex;
    gap: 0;
    border-bottom: 1px solid var(--border-default);
    background: var(--bg-surface);
    flex-shrink: 0;
  }

  .detail-tab {
    font-size: var(--font-size-sm);
    font-weight: 500;
    padding: 8px 16px;
    color: var(--text-secondary);
    border-bottom: 2px solid transparent;
    transition: color 0.1s, border-color 0.1s;
    display: flex;
    align-items: center;
    gap: 6px;
    background: none;
    border-top: none;
    border-left: none;
    border-right: none;
    cursor: pointer;
    font-family: inherit;
  }

  .detail-tab:hover {
    color: var(--text-primary);
    background: var(--bg-surface-hover);
  }

  .detail-tab--active {
    color: var(--text-primary);
    border-bottom-color: var(--accent-blue);
  }

  .edit-body-btn {
    background: none;
    border: none;
    color: var(--text-muted);
    cursor: pointer;
    padding: 0;
    font-size: var(--font-size-2xs);
  }

  .edit-body-btn:hover {
    color: var(--accent-blue);
  }

  .body-edit {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .body-edit-textarea {
    width: 100%;
    min-height: 120px;
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    line-height: 1.5;
    padding: 10px;
    background: var(--bg-inset);
    border: 1px solid var(--accent-blue);
    border-radius: var(--radius-sm);
    color: var(--text-primary);
    resize: vertical;
    outline: none;
  }

  .body-edit-actions {
    display: flex;
    gap: 6px;
  }

  .add-description-btn {
    background: none;
    border: 1px dashed var(--border-default);
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    padding: 12px;
    width: 100%;
    cursor: pointer;
    font-size: var(--font-size-xs);
    text-align: center;
  }

  .add-description-btn:hover {
    border-color: var(--accent-blue);
    color: var(--accent-blue);
  }

  @media (max-width: 640px) {
    .pull-detail {
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

    .pull-detail-content {
      gap: var(--detail-mobile-space-md);
      max-width: 100%;
    }

    .detail-header,
    .title-line {
      gap: var(--detail-mobile-space-sm);
    }

    .detail-title {
      font-size: var(--font-size-xl);
      line-height: 1.25;
    }

    .edit-title-btn,
    .edit-body-btn,
    .star-btn,
    .gh-link {
      min-width: var(--detail-mobile-hit-target);
      min-height: var(--detail-mobile-hit-target);
      justify-content: center;
      padding: var(--detail-mobile-space-xs);
      margin-top: 0;
      font-size: var(--font-size-sm);
    }

    .pull-detail-content .meta-row :global(.copy-number-btn) {
      min-width: 24px;
      min-height: 24px;
      padding: 0;
      border-radius: 3px;
      font-size: var(--font-size-sm);
      line-height: 1.35;
    }

    .meta-row,
    .chips-row,
    .actions-row,
    .body-edit-actions {
      gap: var(--detail-mobile-space-xs);
    }

    .meta-item,
    .meta-sep,
    .meta-branch,
    .branch-name-btn,
    .sync-indicator,
    .section-title,
    .section-title-inline,
    .files-stat,
    .action-error,
    .loading-placeholder,
    .detail-tab {
      font-size: var(--font-size-sm);
      line-height: 1.35;
    }

    .meta-sep--branch,
    .meta-sep--sync {
      display: none;
    }

    .inset-box__content,
    .body-edit-textarea,
    .title-edit-input,
    .title-edit-save,
    .title-edit-cancel,
    .add-description-btn,
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

    .pull-detail :global(.kit-chip),
    .pull-detail :global(.state-chip),
    .pull-detail :global(.status-chip) {
      min-height: calc(var(--detail-mobile-hit-target) * 0.65);
      padding: 2.5px var(--detail-mobile-space-xs);
      border-radius: 999px;
      font-size: var(--font-size-xs);
      line-height: 1.25;
    }

    .actions-row :global(.kit-button),
    .actions-menu-trigger,
    .detail-tab,
    .title-edit-save,
    .title-edit-cancel,
    .add-description-btn {
      min-height: var(--detail-mobile-hit-target);
    }

  }
</style>

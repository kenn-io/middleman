<script lang="ts">
  import CheckIcon from "@lucide/svelte/icons/check";
  import ChevronDownIcon from "@lucide/svelte/icons/chevron-down";
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import CopyIcon from "@lucide/svelte/icons/copy";
  import EyeOffIcon from "@lucide/svelte/icons/eye-off";
  import LinkIcon from "@lucide/svelte/icons/link";
  import MessageSquareReplyIcon from "@lucide/svelte/icons/message-square-reply";
  import PencilIcon from "@lucide/svelte/icons/pencil";
  import Trash2Icon from "@lucide/svelte/icons/trash-2";
  import XIcon from "@lucide/svelte/icons/x";
  import { onDestroy, untrack } from "svelte";
  import { slide } from "svelte/transition";
  import type { IssueEvent, PREvent } from "../../api/types.js";
  import type {
    DetailActivityViewMode,
    DetailTimelineOrder,
  } from "../../stores/detail-activity-view.svelte.js";
  import type { MutationCallbacks } from "../../stores/ordered-mutations.js";
  import { pushModalFrame } from "../../stores/keyboard/modal-stack.svelte.js";
  import type { StoreInstances } from "../../types.js";
  import type { AppExecution } from "../../app/runtime.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { transientClipboardFeedback } from "../../browser/clipboard-feedback.js";
  import MarkdownHtml from "../shared/MarkdownHtml.svelte";
  import {
    Button,
    Card,
    CommentCard,
    IconButton,
    Modal,
    Timeline,
    TimelineItem,
    copyToClipboard,
    formatRelativeTime,
    type TimelineTone,
  } from "@kenn-io/kit-ui";
  import {
    parseMarkdownSuggestions,
    type ApplySuggestionRequest,
    type MarkdownSuggestionBlock,
  } from "../../utils/markdown-suggestions.js";
  import { getStores } from "../../context.js";
  import { createProgressiveMountController } from "../../utils/progressive-mount.js";
  import {
    buildItemReferenceLink,
    type ItemReferenceDataAttributes,
  } from "../../utils/item-reference.js";
  import CommentEditor from "./CommentEditor.svelte";
  import DiffReviewThreadSnippet from "../diff/DiffReviewThreadSnippet.svelte";
  import ReviewSuggestionBlock from "./ReviewSuggestionBlock.svelte";
  import {
    reviewThreadContext,
    reviewThreadLineLabel,
    type ReviewThread,
  } from "../diff/review-thread-context.js";

  interface Props {
    events: Array<PREvent | IssueEvent>;
    orderingEvents?: Array<PREvent | IssueEvent> | undefined;
    provider?: string | undefined;
    platformHost?: string | undefined;
    repoOwner?: string;
    repoName?: string;
    repoPath?: string | undefined;
    number?: number | undefined;
    itemType?: "pull" | "issue" | undefined;
    currentHeadSHA?: string | undefined;
    canResolveReviewThreads?: boolean;
    canReplyToThreads?: boolean;
    filtered?: boolean;
    filterScope?: string;
    showCommitDetails?: boolean;
    activityViewMode?: DetailActivityViewMode;
    timelineOrder?: DetailTimelineOrder;
    initialEntryLimit?: number;
    itemIdentity?: string;
    onEditComment?:
      | ((event: PREvent | IssueEvent, body: string, callbacks: MutationCallbacks) => void)
      | undefined;
    onDeleteComment?:
      | ((event: PREvent | IssueEvent, callbacks: DeleteCommentCallbacks) => void)
      | undefined;
    onApplySuggestion?:
      | ((
          input: ApplySuggestionRequest,
          callbacks: {
            readonly onResult: (result: boolean | SuggestionApplyResult) => void;
            readonly onSettled: () => void;
          },
        ) => void)
      | undefined;
    jumpToReviewThread?: ((thread: ReviewThread) => void) | undefined;
  }

  type SuggestionApplyResult = {
    ok: boolean;
    error?: string | undefined;
  };

  interface DeleteCommentCallbacks {
    readonly onSuccess: () => void;
    readonly onFailure: (message: string) => void;
    readonly onSettled: () => void;
  }

  const {
    events,
    orderingEvents = events,
    provider,
    platformHost,
    repoOwner,
    repoName,
    repoPath,
    number = undefined,
    itemType = undefined,
    currentHeadSHA = "",
    canResolveReviewThreads = false,
    canReplyToThreads = false,
    filtered = false,
    filterScope = "",
    showCommitDetails = true,
    activityViewMode = "normal",
    timelineOrder = "grouped",
    initialEntryLimit = Number.MAX_SAFE_INTEGER,
    itemIdentity = "",
    onEditComment,
    onDeleteComment,
    onApplySuggestion,
    jumpToReviewThread,
  }: Props = $props();
  const stores = getStores() as StoreInstances | undefined;
  const runtime = getAppRuntime();
  const detailStore = stores?.detail;
  const collapseSingleLineBreaks = $derived(
    stores?.settings?.getDetailSettings().collapse_single_line_breaks ?? false,
  );
  const markdownOptions = $derived({ collapseSingleLineBreaks });
  const renderCommitMessagesAsMarkdown = $derived(
    stores?.settings?.getDetailSettings().render_commit_messages_as_markdown ?? false,
  );
  const diffStore = stores?.diff;
  const diffReviewDraft = stores?.diffReviewDraft;
  const diff = $derived(diffStore?.getDiff() ?? null);
  // The cached diff can predate the current PR head (the store skips
  // reloads for the same route). A preview built from that diff would
  // show stale surrounding code for a suggestion applied to the newer
  // head, so treat a known head mismatch as missing context.
  const diffContextStale = $derived(
    currentHeadSHA !== "" &&
      diff !== null &&
      (diff.diff_head_sha ?? "") !== "" &&
      diff.diff_head_sha !== currentHeadSHA,
  );
  const suggestionDiff = $derived(diffContextStale ? null : diff);
  let diffReloadedForHead = "";

  $effect(() => {
    if (!provider || !repoOwner || !repoName || !repoPath || number == null) return;
    const nextRef = { provider, platformHost, owner: repoOwner, name: repoName, repoPath };
    const nextNumber = number;
    untrack(() => {
      diffReviewDraft?.setRouteContext(nextRef, nextNumber);
    });
  });

  $effect(() => {
    if (!diffStore || !provider || !repoOwner || !repoName || !repoPath || number == null) return;
    if (!events.some((event) => reviewThreadFor(event) !== null)) return;
    if (diffStore.isDiffLoading()) return;
    const current = diffStore.getCurrentPR();
    if (
      diffStore.getDiff() !== null &&
      current?.provider === provider &&
      current.platformHost === platformHost &&
      current?.owner === repoOwner &&
      current.name === repoName &&
      current.repoPath === repoPath &&
      current.number === number
    ) {
      // Same route, but the loaded diff may predate the current head.
      // Reload once per observed head; if the server still serves the
      // older snapshot, the preview stays marked outdated instead of
      // looping.
      if (!diffContextStale || diffReloadedForHead === currentHeadSHA) return;
      diffReloadedForHead = currentHeadSHA;
    }
    untrack(() => {
      diffStore.loadDiff(repoOwner, repoName, number, {
        provider,
        platformHost,
        owner: repoOwner,
        name: repoName,
        repoPath,
      });
    });
  });

  const typeLabels: Record<string, string> = {
    issue_comment: "Comment",
    comment_deleted: "Comment deleted",
    review: "Review",
    commit: "Commit",
    force_push: "Force-pushed",
    review_comment: "Review Comment",
    assigned: "Assigned",
    unassigned: "Unassigned",
    merged: "Merged",
    closed: "Closed",
    reopened: "Reopened",
  };

  function eventTimelineTone(eventType: string): TimelineTone {
    switch (eventType) {
      case "issue_comment":
      case "assigned":
      case "reopened":
        return "info";
      case "review":
      case "review_comment":
      case "merged":
        return "merged";
      case "commit":
        return "success";
      case "force_push":
        return "danger";
      default:
        return "muted";
    }
  }

  const mergedCloseCoalesceWindowMs = 60_000;

  function shouldRenderMarkdown(eventType: string): boolean {
    return eventType === "issue_comment" || eventType === "review" || eventType === "review_comment";
  }

  type TimelineEntry = {
    key: string;
    event: PREvent | IssueEvent;
    threadID?: string | undefined;
    reviewThread?: TimelineReviewThread | undefined;
    replies: Array<PREvent | IssueEvent>;
    obsoleteCommits?: Array<PREvent | IssueEvent> | undefined;
  };

  type TimelineReviewThread = {
    thread: ReviewThread;
  };

  type BatchedSuggestion = {
    key: string;
    reviewedHeadSHA: string;
    request: ApplySuggestionRequest["suggestions"][number];
  };

  function threadID(event: PREvent | IssueEvent): string | null {
    return typeof event.ThreadID === "string" && event.ThreadID.length > 0
      ? event.ThreadID
      : null;
  }

  function timelineThreadID(event: PREvent | IssueEvent): string | null {
    return threadID(event) ?? reviewThreadFor(event)?.thread.id ?? null;
  }

  function isThreadedComment(event: PREvent | IssueEvent): boolean {
    return shouldRenderMarkdown(event.EventType) && timelineThreadID(event) !== null;
  }

  function eventSortValue(event: PREvent | IssueEvent): number {
    const timestamp = Date.parse(event.CreatedAt);
    return Number.isFinite(timestamp) ? timestamp : 0;
  }

  function lifecyclePreferenceScore(event: PREvent | IssueEvent): number {
    return (
      (event.Author ? 4 : 0) +
      (event.PlatformID != null ? 2 : 0) +
      (event.PlatformExternalID ? 1 : 0)
    );
  }

  function preferredLifecycleEvent(
    current: PREvent | IssueEvent | null,
    candidate: PREvent | IssueEvent,
  ): PREvent | IssueEvent {
    if (current === null) return candidate;
    const candidateScore = lifecyclePreferenceScore(candidate);
    const currentScore = lifecyclePreferenceScore(current);
    if (candidateScore !== currentScore) {
      return candidateScore > currentScore ? candidate : current;
    }
    const candidateTime = eventSortValue(candidate);
    const currentTime = eventSortValue(current);
    if (candidateTime !== currentTime) {
      return candidateTime > currentTime ? candidate : current;
    }
    return candidate.ID > current.ID ? candidate : current;
  }

  function isCoalescedMergedCloseEvent(
    event: PREvent | IssueEvent,
    mergedEvent: PREvent | IssueEvent,
  ): boolean {
    const eventTime = eventSortValue(event);
    const mergedTime = eventSortValue(mergedEvent);
    if (eventTime === 0 || mergedTime === 0) {
      return event.CreatedAt === mergedEvent.CreatedAt;
    }
    const elapsedAfterMerge = eventTime - mergedTime;
    return elapsedAfterMerge >= 0 && elapsedAfterMerge < mergedCloseCoalesceWindowMs;
  }

  function coalescedMergedCloseEvent(
    sourceEvents: Array<PREvent | IssueEvent>,
    mergedEvent: PREvent | IssueEvent,
  ): PREvent | IssueEvent | null {
    return sourceEvents
      .filter((event) => event.EventType === "closed" && isCoalescedMergedCloseEvent(event, mergedEvent))
      .reduce<PREvent | IssueEvent | null>(preferredLifecycleEvent, null);
  }

  function mergedEventWithCoalescedAuthor(
    mergedEvent: PREvent | IssueEvent,
    coalescedCloseEvent: PREvent | IssueEvent | null,
  ): PREvent | IssueEvent {
    if (mergedEvent.Author || !coalescedCloseEvent?.Author) return mergedEvent;
    return { ...mergedEvent, Author: coalescedCloseEvent.Author };
  }

  function collapseLifecycleTransitions(
    sourceEvents: Array<PREvent | IssueEvent>,
  ): Array<PREvent | IssueEvent> {
    const mergedEvent = sourceEvents
      .filter((event) => event.EventType === "merged")
      .reduce<PREvent | IssueEvent | null>(preferredLifecycleEvent, null);

    if (mergedEvent === null) return sourceEvents;
    const closeEvent = coalescedMergedCloseEvent(sourceEvents, mergedEvent);
    const displayMergedEvent = mergedEventWithCoalescedAuthor(mergedEvent, closeEvent);

    return sourceEvents
      .filter((event) => {
        if (event.EventType === "merged") return event.ID === mergedEvent.ID;
        if (event.EventType === "closed" && isCoalescedMergedCloseEvent(event, mergedEvent)) return false;
        return true;
      })
      .map((event) => (event.ID === mergedEvent.ID ? displayMergedEvent : event));
  }

  function compareEventsAscending(a: PREvent | IssueEvent, b: PREvent | IssueEvent): number {
    return eventSortValue(a) - eventSortValue(b) || a.ID - b.ID;
  }

  function compareEventsDescending(a: PREvent | IssueEvent, b: PREvent | IssueEvent): number {
    return eventSortValue(b) - eventSortValue(a) || b.ID - a.ID;
  }

  type ForcePushBoundary = {
    eventID: number;
    orderCommitID: number;
    startAfterCommitID: number;
    afterCommitID?: number | undefined;
    endAtCommitID?: number | undefined;
    pushedAt: number;
    usesAfterAnchor: boolean;
  };

  type ForcePushGeneration = ForcePushBoundary & {
    effectiveStartAfterCommitID: number;
    effectiveEndAtCommitID: number;
  };

  type TimelineDisplaySortKey = {
    time: number;
    bucketID: number;
    generationOrder: number;
    id: number;
  };

  type CommitSHAIndex = {
    exact: Map<string, PREvent | IssueEvent>;
    prefixes: Map<string, PREvent | IssueEvent | null>;
  };

  const minSHAPrefixLength = 7;
  const maxSHALength = 64;

  function normalizeSHA(value: string): string | null {
    const sha = value.trim().toLowerCase();
    if (sha.length < minSHAPrefixLength || sha.length > maxSHALength) return null;
    return /^[0-9a-f]+$/.test(sha) ? sha : null;
  }

  function commitSHA(event: PREvent | IssueEvent): string | null {
    return event.EventType === "commit" ? normalizeSHA(event.Summary) : null;
  }

  function commitOrder(event: PREvent | IssueEvent): number {
    if (event.EventType !== "commit") return event.ID;
    const metadata = parseMetadata(event);
    return metadataNumber(metadata, "commit_order_key") ?? metadataNumber(metadata, "commit_order") ?? event.ID;
  }

  function addUniquePrefix(
    prefixes: CommitSHAIndex["prefixes"],
    prefix: string,
    event: PREvent | IssueEvent,
  ): void {
    const existing = prefixes.get(prefix);
    if (existing === undefined) {
      prefixes.set(prefix, event);
      return;
    }
    if (existing?.ID !== event.ID) prefixes.set(prefix, null);
  }

  function buildCommitSHAIndex(sourceEvents: Array<PREvent | IssueEvent>): CommitSHAIndex {
    const index: CommitSHAIndex = {
      exact: new Map(),
      prefixes: new Map(),
    };

    for (const event of sourceEvents) {
      const sha = commitSHA(event);
      if (!sha) continue;
      index.exact.set(sha, event);
      for (
        let length = minSHAPrefixLength;
        length <= sha.length && length <= maxSHALength;
        length += 1
      ) {
        addUniquePrefix(index.prefixes, sha.slice(0, length), event);
      }
    }

    return index;
  }

  function lookupCommitBySHA(index: CommitSHAIndex, value: string): PREvent | IssueEvent | null {
    const sha = normalizeSHA(value);
    if (!sha) return null;
    const exact = index.exact.get(sha);
    if (exact) return exact;
    for (let length = sha.length; length >= minSHAPrefixLength; length -= 1) {
      const prefixMatch = index.prefixes.get(sha.slice(0, length));
      if (prefixMatch !== undefined) return prefixMatch;
    }
    return null;
  }

  function forcePushBeforeSHA(event: PREvent | IssueEvent): string | null {
    if (event.EventType !== "force_push") return null;
    return metadataString(parseMetadata(event), "before_sha");
  }

  function forcePushAfterSHA(event: PREvent | IssueEvent): string | null {
    if (event.EventType !== "force_push") return null;
    return metadataString(parseMetadata(event), "after_sha");
  }

  function buildForcePushBoundaries(sourceEvents: Array<PREvent | IssueEvent>): ForcePushBoundary[] {
    const commitIndex = buildCommitSHAIndex(sourceEvents);
    const boundaries: ForcePushBoundary[] = [];

    for (const event of sourceEvents) {
      const beforeSHA = forcePushBeforeSHA(event);
      const beforeCommit = beforeSHA ? lookupCommitBySHA(commitIndex, beforeSHA) : null;
      if (beforeCommit) {
        const afterSHA = forcePushAfterSHA(event);
        const afterCommit = afterSHA ? lookupCommitBySHA(commitIndex, afterSHA) : null;
        const beforeOrder = commitOrder(beforeCommit);
        const afterOrder = afterCommit ? commitOrder(afterCommit) : null;
        if (afterCommit && afterOrder !== null && afterOrder < beforeOrder) {
          boundaries.push({
            eventID: event.ID,
            orderCommitID: afterOrder,
            startAfterCommitID: 0,
            afterCommitID: afterOrder,
            endAtCommitID: afterOrder,
            pushedAt: eventSortValue(event),
            usesAfterAnchor: true,
          });
          continue;
        }
        boundaries.push({
          eventID: event.ID,
          orderCommitID: beforeOrder,
          startAfterCommitID: beforeOrder,
          afterCommitID: afterCommit ? commitOrder(afterCommit) : undefined,
          pushedAt: eventSortValue(event),
          usesAfterAnchor: false,
        });
        continue;
      }

      const afterSHA = forcePushAfterSHA(event);
      const afterCommit = afterSHA ? lookupCommitBySHA(commitIndex, afterSHA) : null;
      if (!afterCommit) continue;
      boundaries.push({
        eventID: event.ID,
        orderCommitID: commitOrder(afterCommit),
        startAfterCommitID: 0,
        afterCommitID: commitOrder(afterCommit),
        endAtCommitID: commitOrder(afterCommit),
        pushedAt: eventSortValue(event),
        usesAfterAnchor: true,
      });
    }

    return boundaries.sort((a, b) =>
      a.orderCommitID - b.orderCommitID || a.pushedAt - b.pushedAt,
    );
  }

  function buildForcePushGenerations(boundaries: ForcePushBoundary[]): ForcePushGeneration[] {
    const generations: Array<Omit<ForcePushGeneration, "effectiveEndAtCommitID">> = [];
    for (const [index, boundary] of boundaries.entries()) {
      const previous = boundaries[index - 1];
      generations.push({
        ...boundary,
        effectiveStartAfterCommitID: boundary.usesAfterAnchor
          ? previous?.afterCommitID ?? previous?.orderCommitID ?? 0
          : boundary.startAfterCommitID,
      });
    }

    return generations.map((generation, index) => {
      const nextGeneration = generations[index + 1];
      return {
        ...generation,
        effectiveEndAtCommitID: Math.min(
          generation.endAtCommitID ?? Number.POSITIVE_INFINITY,
          nextGeneration?.effectiveStartAfterCommitID ?? Number.POSITIVE_INFINITY,
        ),
      };
    });
  }

  function buildForcePushDisplaySortKeys(
    sourceEvents: Array<PREvent | IssueEvent>,
    boundaries: ForcePushBoundary[],
  ): Record<number, TimelineDisplaySortKey> {
    const generations = buildForcePushGenerations(boundaries);
    const displaySortKeys: Record<number, TimelineDisplaySortKey> = {};
    for (const event of sourceEvents) {
      displaySortKeys[event.ID] = {
        time: eventSortValue(event),
        bucketID: event.ID,
        generationOrder: 0,
        id: event.ID,
      };
    }

    const commitEvents = sourceEvents
      .filter((event) => event.EventType === "commit")
      .sort((a, b) => commitOrder(a) - commitOrder(b) || a.ID - b.ID);
    const commitOrderAt = (index: number): number => {
      const event = commitEvents[index];
      return event ? commitOrder(event) : Number.POSITIVE_INFINITY;
    };
    let commitIndex = 0;

    for (const [index, generation] of generations.entries()) {
      const nextGeneration = generations[index + 1];
      displaySortKeys[generation.eventID] = {
        time: generation.pushedAt,
        bucketID: generation.eventID,
        generationOrder: 1,
        id: generation.eventID,
      };
      while (
        commitIndex < commitEvents.length &&
        commitOrderAt(commitIndex) <= generation.effectiveStartAfterCommitID
      ) {
        commitIndex += 1;
      }
      while (
        commitIndex < commitEvents.length &&
        commitOrderAt(commitIndex) <= generation.effectiveEndAtCommitID
      ) {
        const event = commitEvents[commitIndex];
        if (!event) break;
        const lowerBounded = Math.max(eventSortValue(event), generation.pushedAt);
        const nextPushedAt = nextGeneration?.pushedAt;
        displaySortKeys[event.ID] = {
          time: nextPushedAt !== undefined && nextPushedAt >= generation.pushedAt
            ? Math.min(lowerBounded, nextPushedAt)
            : lowerBounded,
          bucketID: generation.eventID,
          generationOrder: 2,
          id: event.ID,
        };
        commitIndex += 1;
      }
    }

    return displaySortKeys;
  }

  function orderEventsForForcePushBoundaries(
    sourceEvents: Array<PREvent | IssueEvent>,
    orderingSourceEvents: Array<PREvent | IssueEvent> = sourceEvents,
  ): Array<PREvent | IssueEvent> {
    const boundaries = buildForcePushBoundaries(orderingSourceEvents);
    if (boundaries.length === 0) return sourceEvents;
    const displaySortKeys = buildForcePushDisplaySortKeys(orderingSourceEvents, boundaries);
    return [...sourceEvents].sort((a, b) => {
      const aKey = displaySortKeys[a.ID] ?? {
        time: eventSortValue(a),
        bucketID: a.ID,
        generationOrder: 0,
        id: a.ID,
      };
      const bKey = displaySortKeys[b.ID] ?? {
        time: eventSortValue(b),
        bucketID: b.ID,
        generationOrder: 0,
        id: b.ID,
      };
      return (
        bKey.time - aKey.time ||
        bKey.bucketID - aKey.bucketID ||
        bKey.generationOrder - aKey.generationOrder ||
        bKey.id - aKey.id
      );
    });
  }

  function orderEventsForDisplay(
    sourceEvents: Array<PREvent | IssueEvent>,
    orderingSourceEvents: Array<PREvent | IssueEvent>,
  ): Array<PREvent | IssueEvent> {
    // Strict date order keeps events at their own timestamps instead of
    // clamping re-pushed commits into force-push generations, so recent
    // comments stay above older commit batches.
    if (timelineOrder === "chronological") {
      return [...sourceEvents].sort(compareEventsDescending);
    }
    return orderEventsForForcePushBoundaries(sourceEvents, orderingSourceEvents);
  }

  // The backend stamps `obsolete: true` into commit-event metadata when the
  // commit is no longer reachable from the PR head in clone ancestry, and
  // clears it when reachable again. The frontend only renders that flag.
  function isObsoleteCommit(event: PREvent | IssueEvent): boolean {
    return event.EventType === "commit" && parseMetadata(event).obsolete === true;
  }

  function collapseObsoleteCommitEntries(
    entries: TimelineEntry[],
    keyPrefix: string,
  ): TimelineEntry[] {
    const collapsed: TimelineEntry[] = [];
    let run: Array<PREvent | IssueEvent> = [];
    const flushRun = (): void => {
      const [newest] = run;
      if (newest === undefined) return;
      collapsed.push({
        key: `${keyPrefix}obsolete-${newest.ID}`,
        event: newest,
        obsoleteCommits: run,
        replies: [],
      });
      run = [];
    };

    for (const entry of entries) {
      if (isObsoleteCommit(entry.event)) {
        run = [...run, entry.event];
        continue;
      }
      flushRun();
      collapsed.push(entry);
    }
    flushRun();
    return collapsed;
  }

  function buildTimelineEntries(
    sourceEvents: Array<PREvent | IssueEvent>,
    orderingSourceEvents: Array<PREvent | IssueEvent>,
  ): TimelineEntry[] {
    const orderedEvents = orderEventsForDisplay(sourceEvents, orderingSourceEvents);
    const threads: Array<{ id: string; events: Array<PREvent | IssueEvent> }> = [];

    for (const event of orderedEvents) {
      const id = timelineThreadID(event);
      if (!id || !isThreadedComment(event)) continue;
      const thread = threads.find((item) => item.id === id);
      if (thread) {
        thread.events = [...thread.events, event];
      } else {
        threads.push({ id, events: [event] });
      }
    }

    const emittedThreads: string[] = [];
    const entries: TimelineEntry[] = [];

    for (const event of orderedEvents) {
      const id = timelineThreadID(event);
      if (!id || !isThreadedComment(event)) {
        entries.push({
          key: `event-${event.ID}`,
          event,
          reviewThread: reviewThreadFor(event) ?? undefined,
          replies: [],
        });
        continue;
      }

      if (emittedThreads.includes(id)) continue;
      emittedThreads.push(id);

      const threadEvents = [...(threads.find((item) => item.id === id)?.events ?? [event])];
      const sortedThreadEvents = threadEvents.sort(compareEventsAscending);
      const reviewThread = sortedThreadEvents
        .map((threadEvent) => reviewThreadFor(threadEvent))
        .find((thread): thread is TimelineReviewThread => thread !== null);
      if (threadEvents.length === 1) {
        entries.push({
          key: `event-${event.ID}`,
          event,
          reviewThread,
          replies: [],
        });
        continue;
      }

      const [root, ...replies] = sortedThreadEvents;
      entries.push({
        key: `thread-${id}`,
        event: root ?? event,
        threadID: id,
        reviewThread,
        replies: replies.sort(compareEventsDescending),
      });
    }

    return timelineOrder === "chronological"
      ? collapseObsoleteCommitEntries(entries, "")
      : entries;
  }

  const displayEvents = $derived(collapseLifecycleTransitions(events));
  const displayOrderingEvents = $derived(collapseLifecycleTransitions(orderingEvents));
  const timelineEntries = $derived(buildTimelineEntries(displayEvents, displayOrderingEvents));
  const compactTimelineEntries = $derived(buildCompactTimelineEntries(displayEvents, displayOrderingEvents));
  const renderedTimelineEntries = $derived(
    activityViewMode === "compact" ? compactTimelineEntries : timelineEntries,
  );
  const progressiveMount = createProgressiveMountController({ batchSize: 25 });
  let mountedEntryCount = $state(0);
  let fullTimelineRequested = $state(false);
  const mountedTimelineEntries = $derived(renderedTimelineEntries.slice(0, mountedEntryCount));
  const unmountedEntryCount = $derived(renderedTimelineEntries.length - mountedEntryCount);
  const timelineMountKey = $derived(
    `${itemIdentity}\u0000${activityViewMode}\u0000${timelineOrder}\u0000${filtered}\u0000${filterScope}\u0000${initialEntryLimit}`,
  );
  let lastTimelineMountKey = "";

  $effect(() => {
    const mountKey = timelineMountKey;
    const entryCount = renderedTimelineEntries.length;
    untrack(() => {
      if (mountKey === lastTimelineMountKey) {
        progressiveMount.cancel();
        mountedEntryCount = fullTimelineRequested ? entryCount : Math.min(initialEntryLimit, entryCount);
        return;
      }
      lastTimelineMountKey = mountKey;
      progressiveMount.cancel();
      fullTimelineRequested = false;
      mountedEntryCount = Math.min(initialEntryLimit, entryCount);
    });
  });

  function loadFullTimeline(): void {
    fullTimelineRequested = true;
    progressiveMount.start(mountedEntryCount, renderedTimelineEntries.length, (count) => {
      mountedEntryCount = count;
    });
  }

  function buildCompactTimelineEntries(
    sourceEvents: Array<PREvent | IssueEvent>,
    orderingSourceEvents: Array<PREvent | IssueEvent>,
  ): TimelineEntry[] {
    const entries = orderEventsForDisplay(sourceEvents, orderingSourceEvents).map((event) => ({
      key: `compact-event-${event.ID}`,
      event,
      reviewThread: reviewThreadFor(event) ?? undefined,
      replies: [],
    }));
    return timelineOrder === "chronological"
      ? collapseObsoleteCommitEntries(entries, "compact-")
      : entries;
  }

  function isCompactEvent(eventType: string): boolean {
    return (
      eventType === "commit" ||
      eventType === "comment_deleted" ||
      eventType === "force_push" ||
      eventType === "cross_referenced" ||
      eventType === "renamed_title" ||
      eventType === "base_ref_changed" ||
      eventType === "assigned" ||
      eventType === "unassigned" ||
      eventType === "merged" ||
      eventType === "closed" ||
      eventType === "reopened"
    );
  }

  function isLifecycleTransitionEvent(eventType: string): boolean {
    return eventType === "merged" || eventType === "closed" || eventType === "reopened";
  }

  function shortCommit(summary: string): string {
    return summary.length > 7 ? summary.slice(0, 7) : summary;
  }

  function commitTitle(body: string): string {
    return body.split(/\r?\n/, 1)[0] ?? "";
  }

  function commitDetailsBody(body: string): string {
    return body.trim();
  }

  function systemEventLabel(eventType: string): string {
    switch (eventType) {
      case "cross_referenced":
        return "Referenced";
      case "comment_deleted":
        return "Comment deleted";
      case "renamed_title":
        return "Title changed";
      case "base_ref_changed":
        return "Base changed";
      case "assigned":
        return "Assigned";
      case "unassigned":
        return "Unassigned";
      case "merged":
        return "Merged";
      case "closed":
        return "Closed";
      case "reopened":
        return "Reopened";
      case "force_push":
        return "Force-pushed";
      default:
        return typeLabels[eventType] ?? eventType;
    }
  }

  function compactEventLabel(eventType: string): string {
    return typeLabels[eventType] ?? systemEventLabel(eventType);
  }

  function compactMarkdownPreview(body: string): string {
    const lines = body.replace(/\r\n/g, "\n").split("\n");
    let inFence = false;
    let codeFallback = "";

    for (const line of lines) {
      const trimmed = line.trim();
      if (trimmed.startsWith("```") || trimmed.startsWith("~~~")) {
        inFence = !inFence;
        continue;
      }
      if (trimmed.length === 0) continue;
      if (inFence) {
        codeFallback ||= trimmed;
        continue;
      }

      const text = trimmed
        .replace(/!\[[^\]]*]\([^)]+\)/g, "")
        .replace(/\[([^\]]+)]\([^)]+\)/g, "$1")
        .replace(/`([^`]+)`/g, "$1")
        .replace(/^#{1,6}\s+/, "")
        .replace(/^>\s?/, "")
        .replace(/^[-*+]\s+/, "")
        .replace(/^\d+\.\s+/, "")
        .replace(/[*_~]/g, "")
        .trim();
      if (text.length > 0) return text;
    }

    return codeFallback;
  }

  function compactEventContext(
    event: PREvent | IssueEvent,
    reviewThread: TimelineReviewThread | undefined,
  ): string {
    if (reviewThread) return reviewThreadLineLabel(reviewThread.thread);
    if (event.EventType === "commit") return shortCommit(event.Summary);
    if (event.EventType === "force_push") return event.Summary;
    return "";
  }

  function compactReviewSummary(event: PREvent | IssueEvent): string {
    const bodyPreview = compactMarkdownPreview(event.Body);
    const summary = event.Summary.trim();
    const verdict = reviewVerdictLabel(summary);
    if (verdict && bodyPreview) return `${verdict} - ${bodyPreview}`;
    if (verdict) return verdict;
    if (summary && bodyPreview) return `${summary} - ${bodyPreview}`;
    return summary || bodyPreview || "Left a review";
  }

  function reviewVerdictLabel(summary: string): string {
    switch (summary.toUpperCase()) {
      case "APPROVED":
        return "Approved";
      case "CHANGES_REQUESTED":
        return "Changes requested";
      case "COMMENTED":
        return "Commented";
      case "DISMISSED":
        return "Dismissed";
      case "PENDING":
        return "Pending";
      default:
        return "";
    }
  }

  function compactEventSummary(
    event: PREvent | IssueEvent,
    reviewThread: TimelineReviewThread | undefined,
  ): string {
    const hiddenState = providerHiddenState(event);
    if (hiddenState) return providerHiddenLabel(hiddenState);
    if (event.EventType === "review") {
      return compactReviewSummary(event);
    }
    if (event.EventType === "review_comment") {
      return compactMarkdownPreview(event.Body)
        || (reviewThread ? compactMarkdownPreview(reviewThread.thread.body) : "")
        || event.Summary
        || "Left a review comment";
    }
    if (event.EventType === "issue_comment") {
      return compactMarkdownPreview(event.Body) || event.Summary || "Commented";
    }
    if (event.EventType === "commit") {
      return commitTitle(event.Body) || event.Summary;
    }
    if (event.EventType === "cross_referenced") {
      return crossReferenceLabel(parseMetadata(event), event.Summary);
    }
    if (isLifecycleTransitionEvent(event.EventType)) {
      return "";
    }
    return event.Summary || compactMarkdownPreview(event.Body) || systemEventLabel(event.EventType);
  }

  function parseMetadata(event: PREvent | IssueEvent): Record<string, unknown> {
    if (!event.MetadataJSON) return {};
    try {
      const parsed = JSON.parse(event.MetadataJSON) as unknown;
      if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) return {};
      return parsed as Record<string, unknown>;
    } catch {
      return {};
    }
  }

  function metadataString(metadata: Record<string, unknown>, key: string): string | null {
    const value = metadata[key];
    return typeof value === "string" && value.length > 0 ? value : null;
  }

  function metadataNumber(metadata: Record<string, unknown>, key: string): number | null {
    const value = metadata[key];
    if (typeof value === "number" && Number.isInteger(value) && value > 0) return value;
    if (typeof value !== "string") return null;
    const parsed = parseInt(value, 10);
    return Number.isInteger(parsed) && parsed > 0 ? parsed : null;
  }

  function crossReferenceLabel(metadata: Record<string, unknown>, fallback: string): string {
    const title = metadataString(metadata, "source_title");
    const owner = metadataString(metadata, "source_owner");
    const name = metadataString(metadata, "source_repo");
    const number = metadataNumber(metadata, "source_number");
    const reference = owner && name && number !== null ? `${owner}/${name}#${number}` : null;

    if (title && reference) return `${title} ${reference}`;
    return title ?? fallback;
  }

  type ProviderHiddenState = {
    reason: string | null;
  };

  function providerHiddenState(event: PREvent | IssueEvent): ProviderHiddenState | null {
    const metadata = parseMetadata(event);
    if (metadata.provider_hidden !== true) return null;
    return { reason: metadataString(metadata, "provider_hidden_reason") };
  }

  function providerHiddenLabel(state: ProviderHiddenState): string {
    if (!state.reason) return "Hidden on GitHub";
    const reason = state.reason === "OFF_TOPIC"
      ? "off-topic"
      : state.reason.toLowerCase().replaceAll("_", " ");
    return `Hidden on GitHub: ${reason.charAt(0).toUpperCase()}${reason.slice(1)}`;
  }

  type CrossReferenceLink = {
    href: string;
    internal: boolean;
    dataAttributes?: ItemReferenceDataAttributes | undefined;
  };

  function crossReferenceLink(
    metadata: Record<string, unknown>,
    sourceUrl: string | null,
  ): CrossReferenceLink | null {
    const sourceType = metadataString(metadata, "source_type");
    const owner = metadataString(metadata, "source_owner");
    const name = metadataString(metadata, "source_repo");
    const number = metadataNumber(metadata, "source_number");
    if (
      provider &&
      owner &&
      name &&
      number !== null &&
      (sourceType === "PullRequest" || sourceType === "Issue")
    ) {
      const repoPath = `${owner}/${name}`;
      const link = buildItemReferenceLink({
        provider,
        platformHost,
        owner,
        name,
        repoPath,
        number,
        itemType: sourceType === "PullRequest" ? "pr" : "issue",
        externalUrl: sourceUrl ?? undefined,
      });
      return {
        ...link,
        internal: true,
      };
    }
    return sourceUrl ? { href: sourceUrl, internal: false } : null;
  }

  function eventDiffThread(event: PREvent | IssueEvent): ReviewThread | null {
    if (!("diff_thread" in event)) return null;
    return (event.diff_thread as ReviewThread | undefined) ?? null;
  }

  function reviewThreadFor(event: PREvent | IssueEvent): TimelineReviewThread | null {
    const thread = eventDiffThread(event);
    return thread ? { thread } : null;
  }

  function refreshAfterThreadChange(): void {
    if (!provider || !repoOwner || !repoName || !repoPath || number == null) return;
    detailStore?.loadDetail(repoOwner, repoName, number, {
      provider,
      platformHost,
      repoPath,
      sync: false,
    });
  }

  let copiedId = $state<string | null>(null);
  let copyExecution: AppExecution<void, never> | undefined;

  onDestroy(() => {
    copyExecution?.interrupt();
    progressiveMount.cancel();
  });
  let editingId = $state<number | null>(null);
  let editDraft = $state("");
  let savingEditId = $state<number | null>(null);
  let editError = $state<string | null>(null);
  let deleteTarget = $state<PREvent | IssueEvent | null>(null);
  let deletingId = $state<number | null>(null);
  let deleteError = $state<string | null>(null);
  let collapsedThreads = $state<string[]>([]);
  let expandedProviderHiddenComments = $state<number[]>([]);
  let expandedCompactRows = $state<string[]>([]);
  let expandedObsoleteGroups = $state<string[]>([]);
  let replyingThreadID = $state<string | null>(null);
  let replyingEntryKey = $state<string | null>(null);
  let replyDraft = $state("");
  let savingReplyThreadID = $state<string | null>(null);
  let replyError = $state<string | null>(null);
  let applyingSuggestionKey = $state<string | null>(null);
  let suggestionErrors = $state<Record<string, string>>({});
  let batchedSuggestions = $state<BatchedSuggestion[]>([]);
  let savingSuggestionBatch = $state(false);
  const batchedSuggestionKeys = $derived(batchedSuggestions.map((item) => item.key));
  const suggestionSubmissionBusy = $derived(
    applyingSuggestionKey !== null || savingSuggestionBatch,
  );
  // Suggestions batched before the PR head moved (or while it is unknown)
  // must not reach batch submit; the server would reject the whole batch.
  // A stale cached diff context also withholds the batch, matching the
  // per-row apply gating.
  const eligibleBatchedSuggestions = $derived(
    diffContextStale
      ? []
      : batchedSuggestions.filter(
          (item) => currentHeadSHA !== "" && item.reviewedHeadSHA === currentHeadSHA,
        ),
  );
  $effect(() => {
    if (deleteTarget === null) return;
    return untrack(() => pushModalFrame("delete-timeline-comment", []));
  });

  function canEditComment(event: PREvent | IssueEvent): boolean {
    return (
      event.EventType === "issue_comment" &&
      event.PlatformID != null &&
      repoOwner !== undefined &&
      repoName !== undefined &&
      onEditComment !== undefined
    );
  }

  function canDeleteComment(event: PREvent | IssueEvent): boolean {
    return event.EventType === "issue_comment" && event.PlatformID != null && onDeleteComment !== undefined;
  }

  function startDelete(event: PREvent | IssueEvent): void {
    if (!canDeleteComment(event) || deletingId !== null) return;
    deleteTarget = event;
    deleteError = null;
  }

  function cancelDelete(): void {
    if (deletingId !== null) return;
    deleteTarget = null;
    deleteError = null;
  }

  function commentExcerpt(body: string): string {
    const compact = body.replace(/\s+/g, " ").trim();
    if (compact === "") return "Empty comment";
    return compact.length > 160 ? `${compact.slice(0, 159)}…` : compact;
  }

  function confirmDelete(): void {
    const target = deleteTarget;
    if (!target || !onDeleteComment || deletingId !== null) return;
    deletingId = target.ID;
    deleteError = null;
    onDeleteComment(target, {
      onSuccess: () => {
        deleteTarget = null;
      },
      onFailure: (message) => {
        deleteError = message;
      },
      onSettled: () => {
        deletingId = null;
      },
    });
  }

  function startEdit(event: PREvent | IssueEvent): void {
    if (providerHiddenState(event) && !expandedProviderHiddenComments.includes(event.ID)) {
      expandedProviderHiddenComments = [...expandedProviderHiddenComments, event.ID];
    }
    editingId = event.ID;
    editDraft = event.Body;
    editError = null;
  }

  function startCompactEdit(entry: TimelineEntry): void {
    if (!expandedCompactRows.includes(entry.key)) {
      expandedCompactRows = [...expandedCompactRows, entry.key];
    }
    startEdit(entry.event);
  }

  function cancelEdit(): void {
    editingId = null;
    editDraft = "";
    editError = null;
  }

  function entryThreadID(entry: TimelineEntry): string {
    return entry.threadID ?? String(entry.event.ID);
  }

  function replyTargetID(entry: TimelineEntry): string | null {
    return entry.reviewThread?.thread.id ?? null;
  }

  function canReplyToThread(entry: TimelineEntry): boolean {
    return (
      canReplyToThreads &&
      detailStore !== undefined &&
      provider !== undefined &&
      repoOwner !== undefined &&
      repoName !== undefined &&
      repoPath !== undefined &&
      number !== undefined &&
      replyTargetID(entry) !== null
    );
  }

  function isReplyingToEntry(entry: TimelineEntry): boolean {
    const targetID = replyTargetID(entry);
    if (targetID === null || replyingThreadID !== targetID) return false;
    return activityViewMode === "compact" ? replyingEntryKey === entry.key : true;
  }

  function isThreadCollapsed(entry: TimelineEntry): boolean {
    return collapsedThreads.includes(entryThreadID(entry));
  }

  function toggleThread(entry: TimelineEntry): void {
    const id = entryThreadID(entry);
    collapsedThreads = collapsedThreads.includes(id)
      ? collapsedThreads.filter((item) => item !== id)
      : [...collapsedThreads, id];
  }

  function isProviderHiddenExpanded(event: PREvent | IssueEvent): boolean {
    return expandedProviderHiddenComments.includes(event.ID);
  }

  function toggleProviderHidden(event: PREvent | IssueEvent): void {
    expandedProviderHiddenComments = expandedProviderHiddenComments.includes(event.ID)
      ? expandedProviderHiddenComments.filter((id) => id !== event.ID)
      : [...expandedProviderHiddenComments, event.ID];
  }

  function isObsoleteGroupExpanded(entry: TimelineEntry): boolean {
    return expandedObsoleteGroups.includes(entry.key);
  }

  function toggleObsoleteGroup(entry: TimelineEntry): void {
    expandedObsoleteGroups = expandedObsoleteGroups.includes(entry.key)
      ? expandedObsoleteGroups.filter((item) => item !== entry.key)
      : [...expandedObsoleteGroups, entry.key];
  }

  function obsoleteGroupSummary(commits: Array<PREvent | IssueEvent>): string {
    return commits.length === 1
      ? "1 commit replaced by a later force push"
      : `${commits.length} commits replaced by a later force push`;
  }

  function compactEntryCanExpand(entry: TimelineEntry): boolean {
    if (entry.event.EventType === "commit") return showCommitDetails && commitDetailsBody(entry.event.Body).length > 0;
    return (
      shouldRenderMarkdown(entry.event.EventType) &&
      (entry.event.Body.trim().length > 0 || entry.reviewThread !== undefined)
    );
  }

  function isCompactEntryExpanded(entry: TimelineEntry): boolean {
    return expandedCompactRows.includes(entry.key);
  }

  function toggleCompactEntry(entry: TimelineEntry): void {
    expandedCompactRows = expandedCompactRows.includes(entry.key)
      ? expandedCompactRows.filter((item) => item !== entry.key)
      : [...expandedCompactRows, entry.key];
  }

  function startReply(entry: TimelineEntry): void {
    const targetID = replyTargetID(entry);
    if (!targetID) return;
    replyingThreadID = targetID;
    replyingEntryKey = entry.key;
    replyDraft = "";
    replyError = null;
  }

  function cancelReply(): void {
    replyingThreadID = null;
    replyingEntryKey = null;
    replyDraft = "";
    replyError = null;
  }

  function submitReply(entry: TimelineEntry): void {
    // Re-check the gate: availability can flip (rate limit, missing
    // write credential) while the composer is already open.
    if (!canReplyToThread(entry)) return;
    const targetID = replyTargetID(entry);
    const body = replyDraft.trim();
    if (!targetID || !provider || !repoOwner || !repoName || !repoPath || number === undefined) return;
    if (body === "") {
      replyError = "Reply body must not be empty";
      return;
    }
    savingReplyThreadID = targetID;
    replyError = null;
    detailStore?.replyToDiscussion(repoOwner, repoName, number, targetID, body, {
      onSuccess: cancelReply,
      onSettled: () => {
        if (savingReplyThreadID === targetID) savingReplyThreadID = null;
      },
    });
  }

  function saveEdit(event: PREvent | IssueEvent): void {
    const nextBody = editDraft.trim();
    if (nextBody === "") {
      editError = "Comment body must not be empty";
      return;
    }
    if (nextBody === event.Body.trim()) {
      cancelEdit();
      return;
    }
    if (onEditComment === undefined) return;

    savingEditId = event.ID;
    editError = null;
    onEditComment(event, nextBody, {
      onSuccess: cancelEdit,
      onSettled: () => {
        if (savingEditId === event.ID) savingEditId = null;
      },
    });
  }

  function copyText(id: string, text: string): void {
    copyExecution?.interrupt();
    copyExecution = runtime.runCommand(
      transientClipboardFeedback({
        text,
        write: copyToClipboard,
        isActive: () => true,
        onCopied: () => {
          copiedId = id;
        },
        onExpired: () => {
          copiedId = null;
        },
      }),
      {
        operation: "copy timeline content",
        safeContext: { id },
        onFailure: () => {},
      },
    );
  }

  function suggestionKey(event: PREvent | IssueEvent, block: MarkdownSuggestionBlock): string {
    return `${event.ID}:${block.key}`;
  }

  function eventSuggestionBlocks(event: PREvent | IssueEvent): MarkdownSuggestionBlock[] {
    if (!shouldRenderMarkdown(event.EventType)) return [];
    return parseMarkdownSuggestions(event.Body);
  }

  function hasSuggestionBlocks(blocks: MarkdownSuggestionBlock[]): boolean {
    return blocks.some((block) => block.type === "suggestion");
  }

  const markdownRepo = $derived(
    provider && repoOwner && repoName && repoPath
      ? { provider, platformHost, owner: repoOwner, name: repoName, repoPath }
      : undefined,
  );

  function suggestionRequest(
    thread: ReviewThread,
    replacement: string,
  ): ApplySuggestionRequest["suggestions"][number] {
    return {
      threadID: thread.id,
      replacement,
    };
  }

  function commitSuggestion(
    event: PREvent | IssueEvent,
    block: Extract<MarkdownSuggestionBlock, { type: "suggestion" }>,
    thread: ReviewThread,
  ): void {
    if (onApplySuggestion === undefined || suggestionSubmissionBusy) return;
    const key = suggestionKey(event, block);
    const { [key]: _discardedError, ...remainingErrors } = suggestionErrors;
    suggestionErrors = remainingErrors;
    applyingSuggestionKey = key;
    onApplySuggestion(
      {
        suggestions: [suggestionRequest(thread, block.replacement)],
      },
      {
        onResult: (result) => {
          const ok = typeof result === "boolean" ? result : result.ok;
          if (ok) {
            batchedSuggestions = batchedSuggestions.filter((item) => item.key !== key);
          } else if (typeof result !== "boolean" && result.error) {
            suggestionErrors = {
              ...suggestionErrors,
              [key]: result.error,
            };
          }
        },
        onSettled: () => {
          applyingSuggestionKey = null;
        },
      },
    );
  }

  function toggleSuggestionBatch(
    event: PREvent | IssueEvent,
    block: Extract<MarkdownSuggestionBlock, { type: "suggestion" }>,
    thread: ReviewThread,
  ): void {
    const key = suggestionKey(event, block);
    batchedSuggestions = batchedSuggestionKeys.includes(key)
      ? batchedSuggestions.filter((item) => item.key !== key)
      : [
          ...batchedSuggestions,
          {
            key,
            reviewedHeadSHA: thread.diff_head_sha ?? "",
            request: suggestionRequest(thread, block.replacement),
          },
        ];
  }

  function commitSuggestionBatch(): void {
    const eligible = eligibleBatchedSuggestions;
    if (onApplySuggestion === undefined || eligible.length === 0 || suggestionSubmissionBusy) return;
    const submittedKeys = eligible.map((item) => item.key);
    suggestionErrors = Object.fromEntries(
      Object.entries(suggestionErrors).filter(([key]) => !submittedKeys.includes(key)),
    );
    savingSuggestionBatch = true;
    onApplySuggestion(
      { suggestions: eligible.map((item) => item.request) },
      {
        onResult: (result) => {
          const ok = typeof result === "boolean" ? result : result.ok;
          if (ok) {
            batchedSuggestions = batchedSuggestions.filter((item) => !submittedKeys.includes(item.key));
          } else if (typeof result !== "boolean" && result.error) {
            suggestionErrors = {
              ...suggestionErrors,
              ...Object.fromEntries(submittedKeys.map((key) => [key, result.error!])),
            };
          }
        },
        onSettled: () => {
          savingSuggestionBatch = false;
        },
      },
    );
  }

  function directLinkCopyID(event: PREvent | IssueEvent): string {
    return `direct-link-${event.ID}`;
  }

  function inlineReplyButtonHtml(entry: TimelineEntry): string {
    const targetID = replyTargetID(entry);
    const expanded = isReplyingToEntry(entry);
    const disabled = savingReplyThreadID !== null;
    return `
      <span class="thread-reply-inline-float">
        <button
          class="thread-toggle thread-reply-action thread-reply-action--inline"
          type="button"
          data-thread-reply-inline="true"
          aria-expanded="${expanded ? "true" : "false"}"
          ${disabled ? "disabled" : ""}
        >
          <svg aria-hidden="true" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <polyline points="9 17 4 12 9 7"></polyline>
            <path d="M20 18v-2a4 4 0 0 0-4-4H4"></path>
          </svg>
          Reply
        </button>
      </span>
    `;
  }

  function withInlineReplyButton(html: string, entry: TimelineEntry): string {
    const template = document.createElement("template");
    template.innerHTML = html;
    const button = inlineReplyButtonHtml(entry);
    const targets = template.content.querySelectorAll("p, li, blockquote, h1, h2, h3, h4, h5, h6");
    const target = targets[targets.length - 1];
    if (target) {
      target.insertAdjacentHTML("beforeend", button);
    } else {
      template.content.append(document.createElement("p"));
      template.content.lastElementChild?.insertAdjacentHTML("beforeend", button);
    }
    return template.innerHTML;
  }

  function handleInlineReplyBodyClick(event: MouseEvent, entry: TimelineEntry | undefined): void {
    if (!entry) return;
    if (!(event.target instanceof Element)) return;
    if (!event.target.closest("[data-thread-reply-inline]")) return;
    startReply(entry);
  }

  function handleInlineReplyBodyKeydown(event: KeyboardEvent, entry: TimelineEntry | undefined): void {
    if (!entry) return;
    if (!(event.target instanceof Element)) return;
    if (!event.target.closest("[data-thread-reply-inline]")) return;
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    startReply(entry);
  }
</script>

{#snippet deleteAction(event: PREvent | IssueEvent)}
  {#if canDeleteComment(event)}
    <IconButton
      size="sm"
      tone="danger"
      onclick={() => startDelete(event)}
      ariaLabel="Delete comment"
      disabled={savingEditId !== null || deletingId !== null}
    >
      <Trash2Icon size={14} />
    </IconButton>
  {/if}
{/snippet}

{#snippet eventActions(event: PREvent | IssueEvent, onEdit: (() => void) | undefined)}
  {#if canEditComment(event)}
    <IconButton
      size="sm"
      onclick={() => onEdit ? onEdit() : startEdit(event)}
      ariaLabel="Edit comment"
      disabled={savingEditId !== null || deletingId !== null}
    >
      <PencilIcon size={14} />
    </IconButton>
  {/if}
  {@render deleteAction(event)}
  {#if event.DirectURL}
    {@const directCopyID = directLinkCopyID(event)}
    <IconButton
      size="sm"
      tone={copiedId === directCopyID ? "success" : "neutral"}
      onclick={() => copyText(directCopyID, event.DirectURL)}
      ariaLabel={copiedId === directCopyID ? "Copied" : "Copy direct link"}
    >
      {#if copiedId === directCopyID}
        <CheckIcon size={14} />
      {:else}
        <LinkIcon size={14} />
      {/if}
    </IconButton>
  {/if}
  {#if event.Body}
    <IconButton
      size="sm"
      tone={copiedId === String(event.ID) ? "success" : "neutral"}
      onclick={() => copyText(String(event.ID), event.Body)}
      ariaLabel={copiedId === String(event.ID) ? "Copied" : "Copy comment"}
    >
      {#if copiedId === String(event.ID)}
        <CheckIcon size={14} />
      {:else}
        <CopyIcon size={14} />
      {/if}
    </IconButton>
  {/if}
{/snippet}

{#snippet providerHiddenNotice(event: PREvent | IssueEvent, state: ProviderHiddenState)}
  {@const expanded = isProviderHiddenExpanded(event)}
  <div class="provider-hidden-notice">
    <EyeOffIcon size={14} aria-hidden="true" />
    <span>{providerHiddenLabel(state)}</span>
    <button
      class="provider-hidden-toggle"
      type="button"
      onclick={() => toggleProviderHidden(event)}
      aria-expanded={expanded}
    >
      {expanded ? "Collapse" : "Show comment"}
    </button>
  </div>
{/snippet}

{#snippet commitBody(body: string, animated: boolean)}
  {#if animated}
    <div
      class={["event-body", "commit-body-details", { "markdown-body": renderCommitMessagesAsMarkdown }]}
      transition:slide={{ duration: 100 }}
    >
      {#if renderCommitMessagesAsMarkdown}
        <MarkdownHtml raw={body} repo={markdownRepo} options={markdownOptions} />
      {:else}
        {body}
      {/if}
    </div>
  {:else}
    <div class={["event-body", "commit-body-details", { "markdown-body": renderCommitMessagesAsMarkdown }]}>
      {#if renderCommitMessagesAsMarkdown}
        <MarkdownHtml raw={body} repo={markdownRepo} options={markdownOptions} />
      {:else}
        {body}
      {/if}
    </div>
  {/if}
{/snippet}

{#snippet eventBody(
  event: PREvent | IssueEvent,
  nested = false,
  reviewThread: TimelineReviewThread | undefined = undefined,
  inlineReplyEntry: TimelineEntry | undefined = undefined,
)}
  {#if event.Body}
    <div
      class={[
        "event-body-wrap",
        nested && "event-body-wrap--nested",
        !nested && reviewThread && "event-body-wrap--with-thread",
      ]}
    >
      {#if !nested && reviewThread}
        <DiffReviewThreadSnippet
          thread={reviewThread.thread}
          context={diff ? reviewThreadContext(diff, reviewThread.thread) : null}
          canResolve={reviewThread.thread.can_resolve && canResolveReviewThreads && diffReviewDraft != null}
          onchanged={refreshAfterThreadChange}
          jumpToDiff={jumpToReviewThread
            ? () => jumpToReviewThread(reviewThread.thread)
            : undefined}
        />
      {/if}
      {#if editingId === event.ID && provider && repoOwner && repoName && repoPath}
        <div class="edit-panel">
          <CommentEditor
            {provider}
            {platformHost}
            owner={repoOwner}
            name={repoName}
            {repoPath}
            {itemType}
            itemNumber={number}
            value={editDraft}
            disabled={savingEditId === event.ID}
            autofocus
            oninput={(nextBody) => {
              editDraft = nextBody;
            }}
            onsubmit={() => {
              saveEdit(event);
            }}
          />
          {#if editError}
            <p class="edit-error">{editError}</p>
          {/if}
          <div class="edit-actions">
            <button
              class="edit-action edit-action--primary"
              onclick={() => saveEdit(event)}
              disabled={savingEditId === event.ID}
            >
              <CheckIcon size={14} />
              {savingEditId === event.ID ? "Saving..." : "Save"}
            </button>
            <button
              class="edit-action"
              onclick={cancelEdit}
              disabled={savingEditId === event.ID}
            >
              <XIcon size={14} />
              Cancel
            </button>
          </div>
        </div>
      {:else}
        <div
          class={[
            "event-body",
            {
              "markdown-body": shouldRenderMarkdown(event.EventType),
              "event-body--nested": nested,
              "event-body--with-inline-reply": inlineReplyEntry,
            },
          ]}
          onclick={(clickEvent) => handleInlineReplyBodyClick(clickEvent, inlineReplyEntry)}
          onkeydown={(keyEvent) => handleInlineReplyBodyKeydown(keyEvent, inlineReplyEntry)}
          role="presentation"
        >
          {#if shouldRenderMarkdown(event.EventType)}
            {@const blocks = eventSuggestionBlocks(event)}
            {#if hasSuggestionBlocks(blocks)}
              <div class="event-body-segments">
                {#each blocks as block (block.key)}
                  {#if block.type === "markdown"}
                    {#if block.text.trim().length > 0}
                      <div class="event-body-segment">
                        <MarkdownHtml raw={block.text} repo={markdownRepo} options={markdownOptions} />
                      </div>
                    {/if}
                  {:else if reviewThread}
                    {@const blockKey = suggestionKey(event, block)}
                    <ReviewSuggestionBlock
                      thread={reviewThread.thread}
                      context={suggestionDiff ? reviewThreadContext(suggestionDiff, reviewThread.thread) : reviewThreadContext(null, reviewThread.thread)}
                      replacement={block.replacement}
                      {currentHeadSHA}
                      applying={applyingSuggestionKey === blockKey}
                      submissionBusy={suggestionSubmissionBusy}
                      batched={batchedSuggestionKeys.includes(blockKey)}
                      error={suggestionErrors[blockKey] ?? null}
                      onCommit={onApplySuggestion !== undefined
                        ? () => commitSuggestion(event, block, reviewThread.thread)
                        : undefined}
                      onToggleBatch={onApplySuggestion !== undefined
                        ? () => toggleSuggestionBatch(event, block, reviewThread.thread)
                        : undefined}
                    />
                  {:else}
                    {@const fallback = "```suggestion\n" + block.replacement + "\n```"}
                    <div class="event-body-segment">
                      <MarkdownHtml raw={fallback} repo={markdownRepo} options={markdownOptions} />
                    </div>
                  {/if}
                {/each}
                {#if inlineReplyEntry}
                  <button
                    class="thread-toggle thread-reply-action thread-reply-action--inline"
                    type="button"
                    onclick={() => startReply(inlineReplyEntry)}
                    aria-expanded={isReplyingToEntry(inlineReplyEntry)}
                    disabled={savingReplyThreadID !== null}
                  >
                    <MessageSquareReplyIcon size={14} />
                    Reply
                  </button>
                {/if}
              </div>
            {:else}
              <MarkdownHtml
                raw={event.Body}
                repo={markdownRepo}
                options={markdownOptions}
                transformHtml={inlineReplyEntry
                  ? (html) => withInlineReplyButton(html, inlineReplyEntry)
                  : undefined}
              />
            {/if}
          {:else}
            {event.Body}
          {/if}
        </div>
      {/if}
    </div>
	{:else if editingId === event.ID && provider && repoOwner && repoName && repoPath}
	  <div class={nested ? "event-body-wrap event-body-wrap--nested" : "event-body-wrap"}>
      <div class="edit-panel">
        <CommentEditor
          {provider}
          {platformHost}
          owner={repoOwner}
          name={repoName}
          {repoPath}
          {itemType}
          itemNumber={number}
          value={editDraft}
          disabled={savingEditId === event.ID}
          autofocus
          oninput={(nextBody) => {
            editDraft = nextBody;
          }}
          onsubmit={() => {
            saveEdit(event);
          }}
        />
        {#if editError}
          <p class="edit-error">{editError}</p>
        {/if}
        <div class="edit-actions">
          <button
            class="edit-action edit-action--primary"
            onclick={() => saveEdit(event)}
            disabled={savingEditId === event.ID}
          >
            <CheckIcon size={14} />
            {savingEditId === event.ID ? "Saving..." : "Save"}
          </button>
          <button
            class="edit-action"
            onclick={cancelEdit}
            disabled={savingEditId === event.ID}
          >
            <XIcon size={14} />
            Cancel
          </button>
        </div>
      </div>
	  </div>
	{/if}
{/snippet}

{#snippet eventAuthorByline(event: PREvent | IssueEvent, compact = false)}
  <span class={["event-author", compact && "compact-event-author", isLifecycleTransitionEvent(event.EventType) && event.Author && "event-author--lifecycle"]}>
    {#if isLifecycleTransitionEvent(event.EventType) && event.Author}
      <span class="event-author-prefix">by</span> {event.Author}
    {:else}
      {event.Author || "Unknown"}
    {/if}
  </span>
{/snippet}

{#snippet threadReplyPanel(entry: TimelineEntry, targetID: string)}
	{#if provider && repoOwner && repoName && repoPath}
	  <div class="thread-reply-panel">
	    <CommentEditor
	      {provider}
	      {platformHost}
	      owner={repoOwner}
	      name={repoName}
	      {repoPath}
	      {itemType}
	      itemNumber={number}
	      value={replyDraft}
	      placeholder="Reply to thread... (Cmd+Enter to submit)"
	      disabled={savingReplyThreadID === targetID}
	      oninput={(nextBody) => {
	        replyDraft = nextBody;
	      }}
	      onsubmit={() => {
	        submitReply(entry);
	      }}
	    />
	    {#if replyError}
	      <p class="edit-error">{replyError}</p>
	    {/if}
	    <div class="edit-actions">
	      <button
	        class="edit-action edit-action--primary"
	        onclick={() => submitReply(entry)}
	        disabled={savingReplyThreadID === targetID || !canReplyToThread(entry)}
	      >
	        <CheckIcon size={14} />
	        {savingReplyThreadID === targetID ? "Replying..." : "Reply"}
	      </button>
	      <button
	        class="edit-action"
	        onclick={cancelReply}
	        disabled={savingReplyThreadID === targetID}
	      >
	        <XIcon size={14} />
	        Cancel
	      </button>
	    </div>
	  </div>
	{/if}
{/snippet}

{#if events.length === 0}
  <p class="empty">{filtered ? "No activity matches the current filters" : "No activity yet"}</p>
{:else}
  {#if onApplySuggestion !== undefined && eligibleBatchedSuggestions.length > 0}
    <Card level="default" padding="sm" class="suggestion-batch-bar">
      <div class="suggestion-batch-content">
        <span>{eligibleBatchedSuggestions.length} {eligibleBatchedSuggestions.length === 1 ? "suggestion" : "suggestions"} in batch</span>
        <button
          class="thread-toggle thread-reply-action"
          type="button"
          onclick={() => commitSuggestionBatch()}
          disabled={suggestionSubmissionBusy}
        >
          <CheckIcon size={14} />
          {savingSuggestionBatch ? "Committing..." : "Commit batch"}
        </button>
      </div>
    </Card>
  {/if}
  <div class="event-timeline">
    <Timeline ariaLabel="Item activity">
      {#each mountedTimelineEntries as entry (entry.key)}
      {@const event = entry.event}
      {@const hiddenState = providerHiddenState(event)}
      {@const targetID = replyTargetID(entry)}
      {@const hasReplyOnlyAction = entry.replies.length === 0 && canReplyToThread(entry)}
      {#if entry.obsoleteCommits}
        {@const obsoleteExpanded = isObsoleteGroupExpanded(entry)}
        <TimelineItem tone="muted" class="event--compact">
          <Card level="default" padding="sm" class="event-card--compact event-card--obsolete-group">
            <button
              class="obsolete-group-row compact-event-toggle"
              type="button"
              onclick={() => toggleObsoleteGroup(entry)}
              aria-expanded={obsoleteExpanded}
              title={obsoleteExpanded ? "Collapse obsolete commits" : "Expand obsolete commits"}
            >
              <span class="compact-event-expander" aria-hidden="true">
                {#if obsoleteExpanded}
                  <ChevronDownIcon size={14} />
                {:else}
                  <ChevronRightIcon size={14} />
                {/if}
              </span>
              <span class="event-type obsolete-group-type">Obsolete</span>
              <span class="compact-event-summary">{obsoleteGroupSummary(entry.obsoleteCommits)}</span>
              <span class="event-time compact-event-time">{formatRelativeTime(event.CreatedAt)}</span>
            </button>
            {#if obsoleteExpanded}
              <div class="obsolete-commit-list">
                {#each entry.obsoleteCommits as commit (commit.ID)}
                  <div class="obsolete-commit-row">
                    {#if commit.Author}
                      <span class="event-author">{commit.Author}</span>
                    {/if}
                    <span class="commit-sha">{shortCommit(commit.Summary)}</span>
                    <span class="commit-title">{commitTitle(commit.Body)}</span>
                    <span class="event-time">{formatRelativeTime(commit.CreatedAt)}</span>
                  </div>
                {/each}
              </div>
            {/if}
          </Card>
        </TimelineItem>
      {:else}
      <TimelineItem
        tone={eventTimelineTone(event.EventType)}
        class={activityViewMode === "compact" || isCompactEvent(event.EventType) ? "event--compact" : ""}
      >
        {#if activityViewMode === "compact"}
          {@const compactContext = compactEventContext(event, entry.reviewThread)}
          {@const compactSummary = compactEventSummary(event, entry.reviewThread)}
          {@const compactMetadata = event.EventType === "cross_referenced" ? parseMetadata(event) : null}
          {@const compactSourceUrl = compactMetadata ? metadataString(compactMetadata, "source_url") : null}
          {@const compactSourceLink = compactMetadata ? crossReferenceLink(compactMetadata, compactSourceUrl) : null}
          {@const canExpandCompact = compactEntryCanExpand(entry)}
          {@const compactExpanded = isCompactEntryExpanded(entry)}
          <Card
            level="default"
            padding="sm"
            class={["event-card--compact-row", hasReplyOnlyAction && compactExpanded && "event-card--reply-inline"].filter(Boolean).join(" ")}
          >
            <div class="compact-event-line">
              {#if canExpandCompact}
                <button
                  class="compact-event-row compact-event-toggle"
                  type="button"
                  onclick={() => toggleCompactEntry(entry)}
                  aria-expanded={compactExpanded}
                  title={compactExpanded ? "Collapse activity" : "Expand activity"}
                >
                  <span class="compact-event-expander" aria-hidden="true">
                    {#if compactExpanded}
                      <ChevronDownIcon size={14} />
                    {:else}
                      <ChevronRightIcon size={14} />
                    {/if}
                  </span>
                  <span class="event-type compact-event-type">
                    {compactEventLabel(event.EventType)}
                  </span>
                  {@render eventAuthorByline(event, true)}
                  <span class="compact-event-context" title={compactContext}>
                    {compactContext}
                  </span>
                  <span class="compact-event-summary" title={compactSummary}>
                    {#if hiddenState}
                      <span class="compact-hidden-summary">
                        <EyeOffIcon size={13} aria-hidden="true" />
                        <span>{compactSummary}</span>
                      </span>
                    {:else}
                      {compactSummary}
                    {/if}
                  </span>
                  <span class="event-time compact-event-time">{formatRelativeTime(event.CreatedAt)}</span>
                </button>
              {:else}
                <div class="compact-event-row">
                  <span class="compact-event-expander" aria-hidden="true"></span>
                  <span class="event-type compact-event-type">
                    {compactEventLabel(event.EventType)}
                  </span>
                  {@render eventAuthorByline(event, true)}
                  <span class="compact-event-context" title={compactContext}>
                    {compactContext}
                  </span>
                  <span class="compact-event-summary" title={compactSummary}>
                    {#if compactSourceLink}
                      <a
                        class={["system-event-link", { "item-ref": compactSourceLink.internal }]}
                        href={compactSourceLink.href}
                        target={compactSourceLink.internal ? undefined : "_blank"}
                        rel={compactSourceLink.internal ? undefined : "noopener noreferrer"}
                        {...(compactSourceLink.dataAttributes ?? {})}
                      >
                        {compactSummary}
                      </a>
                    {:else}
                      {compactSummary}
                    {/if}
                  </span>
                  <span class="event-time compact-event-time">{formatRelativeTime(event.CreatedAt)}</span>
                </div>
              {/if}
              <div class="compact-event-actions">
                {@render eventActions(event, () => startCompactEdit(entry))}
              </div>
            </div>
            {#if (canExpandCompact && compactExpanded) || editingId === event.ID}
              <div class="compact-expanded-content">
                {#if event.EventType === "commit"}
                  {@render commitBody(commitDetailsBody(event.Body), false)}
                {:else}
                  {@render eventBody(event, false, entry.reviewThread, hasReplyOnlyAction ? entry : undefined)}
                {/if}
              </div>
            {/if}
            {#if isReplyingToEntry(entry) && targetID !== null}
              {@render threadReplyPanel(entry, targetID)}
            {/if}
          </Card>
        {:else if isCompactEvent(event.EventType)}
          {@const metadata = parseMetadata(event)}
          {@const commitDetails = event.EventType === "commit" ? commitDetailsBody(event.Body) : ""}
          {#if isLifecycleTransitionEvent(event.EventType)}
            <CommentCard
              class="event-card--compact event--lifecycle"
              typeLabel={systemEventLabel(event.EventType)}
              tone={eventTimelineTone(event.EventType)}
              author={event.Author ? `by ${event.Author}` : undefined}
              time={formatRelativeTime(event.CreatedAt)}
            />
          {:else if event.EventType === "commit"}
            <Card
              level="default"
              padding="sm"
              class="event-card--compact event-card--commit"
            >
              <div class="event-header event-header--compact">
                <span
                  class="event-type"
                  style="color: var(--accent-green)"
                >
                  {systemEventLabel(event.EventType)}
                </span>
                {#if event.Author}
                  <span class="event-author">{event.Author}</span>
                {/if}
                <span class="commit-sha">{shortCommit(event.Summary)}</span>
                {#if !showCommitDetails}
                  <span class="commit-title">{commitTitle(event.Body)}</span>
                {/if}
                <span class="event-time">{formatRelativeTime(event.CreatedAt)}</span>
              </div>
              {#if showCommitDetails && commitDetails}
                {@render commitBody(commitDetails, true)}
              {/if}
            </Card>
          {:else if event.EventType === "cross_referenced"}
            {@const sourceUrl = metadataString(metadata, "source_url")}
            {@const sourceLabel = crossReferenceLabel(metadata, event.Summary)}
            {@const sourceLink = crossReferenceLink(metadata, sourceUrl)}
            <CommentCard
              class="event-card--compact"
              typeLabel="Referenced"
              tone={eventTimelineTone(event.EventType)}
              author={event.Author || undefined}
              time={formatRelativeTime(event.CreatedAt)}
            >
              {#if sourceLink}
                <a
                  class={["system-event-link", { "item-ref": sourceLink.internal }]}
                  href={sourceLink.href}
                  target={sourceLink.internal ? undefined : "_blank"}
                  rel={sourceLink.internal ? undefined : "noopener noreferrer"}
                  {...(sourceLink.dataAttributes ?? {})}
                >
                  {sourceLabel}
                </a>
              {:else}
                <span class="system-event-summary">{sourceLabel}</span>
              {/if}
            </CommentCard>
          {:else}
            <CommentCard
              class="event-card--compact"
              typeLabel={event.EventType === "comment_deleted" || event.EventType === "assigned" || event.EventType === "unassigned"
                ? undefined
                : systemEventLabel(event.EventType)}
              tone={eventTimelineTone(event.EventType)}
              author={event.Author || undefined}
              time={formatRelativeTime(event.CreatedAt)}
            >
              <span class="system-event-summary system-event-summary--sentence">{event.Summary}</span>
            </CommentCard>
          {/if}
        {:else}
          <CommentCard
            class={[
              hasReplyOnlyAction && "event-card--reply-inline",
              hiddenState && "event-card--provider-hidden",
            ].filter(Boolean).join(" ")}
            typeLabel={typeLabels[event.EventType] ?? event.EventType}
            tone={eventTimelineTone(event.EventType)}
            author={event.Author || undefined}
            time={formatRelativeTime(event.CreatedAt)}
            bodyGap="none"
          >
            {#snippet actions()}
              {@render eventActions(event, undefined)}
            {/snippet}
            {#if event.Summary && (event.EventType === "commit" || event.EventType === "force_push")}
              <p class="event-summary">{event.Summary}</p>
            {/if}
            {#if hiddenState}
              {@render providerHiddenNotice(event, hiddenState)}
            {/if}
            {#if !hiddenState || isProviderHiddenExpanded(event) || editingId === event.ID}
              {@render eventBody(event, false, entry.reviewThread, hasReplyOnlyAction ? entry : undefined)}
            {/if}
            {#if (!hiddenState || isProviderHiddenExpanded(event)) && (entry.replies.length > 0 || (canReplyToThread(entry) && !hasReplyOnlyAction))}
              <div class="thread-controls">
                {#if entry.replies.length > 0}
                  <button
                    class="thread-toggle"
                    type="button"
                    onclick={() => toggleThread(entry)}
                    aria-expanded={!isThreadCollapsed(entry)}
                  >
                    {#if isThreadCollapsed(entry)}
                      <ChevronRightIcon size={14} />
                      Show {entry.replies.length} {entry.replies.length === 1 ? "reply" : "replies"}
                    {:else}
                      <ChevronDownIcon size={14} />
                      Hide {entry.replies.length} {entry.replies.length === 1 ? "reply" : "replies"}
                    {/if}
                  </button>
                {/if}
                {#if canReplyToThread(entry)}
                  <button
                    class="thread-toggle thread-reply-action"
                    type="button"
                    onclick={() => startReply(entry)}
                    aria-expanded={isReplyingToEntry(entry)}
                    disabled={savingReplyThreadID !== null}
                  >
                    <MessageSquareReplyIcon size={14} />
                    Reply
                  </button>
                {/if}
              </div>
              {#if !isThreadCollapsed(entry)}
                <ol class="thread-replies" aria-label="Threaded replies">
                  {#each entry.replies as reply, index (reply.ID)}
                    {@const replyHiddenState = providerHiddenState(reply)}
                    <li
                      class="thread-reply"
                      class:thread-reply--first={index === 0}
                      class:thread-reply--last={index === entry.replies.length - 1}
                    >
                      <div class="thread-reply-rail" aria-hidden="true">
                        <span class="thread-reply-dot"></span>
                      </div>
                      <div class="thread-reply-content">
                        <div class="event-header thread-reply-header">
                          <span class="event-type">Reply</span>
                          {#if reply.Author}
                            <span class="event-author">{reply.Author}</span>
                          {/if}
                          <span class="event-time">{formatRelativeTime(reply.CreatedAt)}</span>
                          <span class="thread-reply-actions">
                            {@render eventActions(reply, undefined)}
                          </span>
                        </div>
                        {#if replyHiddenState}
                          {@render providerHiddenNotice(reply, replyHiddenState)}
                        {/if}
                        {#if !replyHiddenState || isProviderHiddenExpanded(reply) || editingId === reply.ID}
                          {@render eventBody(reply, true)}
                        {/if}
                      </div>
                    </li>
                  {/each}
                </ol>
              {/if}
            {/if}
            {#if isReplyingToEntry(entry) && targetID !== null}
              {@render threadReplyPanel(entry, targetID)}
            {/if}
          </CommentCard>
        {/if}
      </TimelineItem>
      {/if}
      {/each}
    </Timeline>
    {#if unmountedEntryCount > 0}
      <div class="timeline-load-more">
        {#if fullTimelineRequested}
          <span role="status">
            Loading full timeline ({unmountedEntryCount} {unmountedEntryCount === 1 ? "entry" : "entries"} remaining)
          </span>
        {:else}
          <Button size="sm" tone="neutral" surface="outline" onclick={loadFullTimeline}>
            Show {unmountedEntryCount} more
          </Button>
        {/if}
      </div>
    {/if}
  </div>
{/if}

{#if deleteTarget}
  <Modal title="Delete comment?" width="min(440px, calc(100vw - 40px))" onclose={cancelDelete}>
    <div class="delete-confirmation">
      <p class="delete-confirmation__message">
        Delete {deleteTarget.Author || "Unknown"}'s comment?
      </p>
      <blockquote>{commentExcerpt(deleteTarget.Body)}</blockquote>
      <p class="delete-confirmation__hint">This cannot be undone.</p>
      {#if deleteError}
        <p class="delete-confirmation__error" role="alert">{deleteError}</p>
      {/if}
    </div>
    {#snippet footer()}
      <Button tone="neutral" surface="outline" onclick={cancelDelete} disabled={deletingId !== null}>
        Cancel
      </Button>
      <Button tone="danger" surface="solid" onclick={() => void confirmDelete()} disabled={deletingId !== null}>
        {deletingId !== null ? "Deleting..." : "Delete"}
      </Button>
    {/snippet}
  </Modal>
{/if}

<style>
  .empty {
    font-size: var(--font-size-root);
    color: var(--text-muted);
    padding: 16px 0;
  }

  .timeline-load-more {
    display: flex;
    justify-content: center;
    padding: var(--focus-detail-space-md, 12px) 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  :global(.suggestion-batch-bar) {
    margin: 0.31rem 0 0.31rem 2.47rem;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .suggestion-batch-content {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--focus-detail-space-sm, 0.62rem);
  }

  :global(.event-card--compact) {
    --kit-card-padding-block: var(--focus-detail-space-xs, 7px);
  }

  .event-header {
    display: flex;
    align-items: center;
    gap: var(--focus-detail-space-xs, 6px);
    flex-wrap: wrap;
  }

  .event-header--compact {
    min-width: 0;
    flex-wrap: nowrap;
  }

  .event-card--compact-row {
    overflow: hidden;
  }

  .event-body-segments {
    display: flow-root;
  }

  .event-body-segment:empty {
    display: none;
  }

  .compact-event-line {
    display: grid;
    grid-template-columns: minmax(0, 1fr) max-content;
    align-items: center;
    gap: var(--focus-detail-space-xs, 4px);
    min-width: 0;
  }

  .compact-event-actions,
  .thread-reply-actions {
    display: inline-flex;
    align-items: center;
    gap: var(--focus-detail-space-xs, 4px);
  }

  .compact-event-row {
    display: grid;
    grid-template-columns: 15px minmax(84.5px, 97.5px) minmax(65px, 91px) minmax(0, 110.5px) minmax(0, 1fr) max-content;
    align-items: center;
    gap: var(--focus-detail-space-xs, 6px);
    min-width: 0;
  }

  .compact-event-toggle {
    width: 100%;
    color: inherit;
    text-align: left;
    border-radius: var(--radius-sm);
  }

  .compact-event-toggle:hover {
    background: var(--bg-surface-hover);
  }

  .compact-event-toggle:focus-visible {
    outline: 2px solid var(--focus-ring, var(--accent-blue));
    outline-offset: 2px;
  }

  .compact-event-expander {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    color: var(--text-muted);
  }

  .compact-expanded-content {
    margin-top: var(--focus-detail-space-sm, 8px);
    padding-top: var(--focus-detail-space-sm, 8px);
    border-top: 1px solid var(--border-muted);
  }

  .compact-expanded-content .event-body-wrap {
    margin-top: 0;
  }

  .compact-event-type,
  .compact-event-author,
  .compact-event-context,
  .compact-event-summary,
  .compact-event-time {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .compact-event-context {
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
  }

  .compact-event-summary {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }

  .compact-hidden-summary {
    display: inline-flex;
    align-items: center;
    gap: var(--focus-detail-space-xs, 6px);
    max-width: 100%;
    color: var(--text-muted);
  }

  .compact-hidden-summary span {
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .provider-hidden-notice {
    display: flex;
    align-items: center;
    gap: var(--focus-detail-space-xs, 6px);
    min-height: 28px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .provider-hidden-toggle {
    margin-left: auto;
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
    font-weight: 600;
  }

  .provider-hidden-toggle:hover {
    color: var(--text-primary);
    background: var(--bg-surface-hover);
  }

  .provider-hidden-toggle:focus-visible {
    outline: 2px solid var(--focus-ring, var(--accent-blue));
    outline-offset: 2px;
  }

  .compact-event-time {
    margin-left: 0;
  }

  .event-type {
    font-size: var(--font-size-xs);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .event-author {
    font-size: var(--font-size-sm);
    font-weight: 500;
    color: var(--text-primary);
  }

  .event-author-prefix {
    color: var(--text-muted);
    font-weight: 400;
  }

  .event-time {
    font-size: var(--font-size-xs);
    color: var(--text-muted);
    margin-left: auto;
  }

  .event-summary {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
    margin-top: var(--focus-detail-space-xs, 4px);
    font-family: var(--font-mono);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .commit-sha {
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }

  .event-timeline :global(.kit-comment-card:not(.event-card--commit) .kit-card__actions) {
    order: 1;
    margin-left: auto;
    opacity: 0;
    pointer-events: none;
    transition: opacity 0.15s;
  }

  .event-timeline :global(.kit-comment-card:not(.event-card--commit):has(.kit-card__actions) .kit-card__meta) {
    order: 2;
    margin-left: 0;
  }

  .event-timeline :global(.kit-comment-card:not(.event-card--commit):hover .kit-card__actions),
  .event-timeline :global(.kit-comment-card:not(.event-card--commit):focus-within .kit-card__actions) {
    opacity: 1;
    pointer-events: auto;
  }

  @media (hover: none), (pointer: coarse) {
    .event-timeline :global(.kit-comment-card:not(.event-card--commit) .kit-card__actions) {
      opacity: 1;
      pointer-events: auto;
    }
  }

  .commit-title,
  .system-event-summary,
  .system-event-link {
    display: block;
    max-width: 100%;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .commit-title {
    flex: 1;
    color: var(--text-primary);
  }

  .commit-body-details {
    margin-top: var(--focus-detail-space-xs, 7px);
    padding-right: var(--focus-detail-space-sm, 10px);
  }

  .obsolete-group-row {
    display: grid;
    grid-template-columns: 15px max-content minmax(0, 1fr) max-content;
    align-items: center;
    gap: var(--focus-detail-space-xs, 6px);
    min-width: 0;
  }

  .obsolete-group-type {
    color: var(--text-muted);
  }

  .obsolete-commit-list {
    display: flex;
    flex-direction: column;
    gap: var(--focus-detail-space-xs, 4px);
    margin-top: var(--focus-detail-space-sm, 8px);
    padding-top: var(--focus-detail-space-sm, 8px);
    border-top: 1px solid var(--border-muted);
  }

  .obsolete-commit-row {
    display: flex;
    align-items: center;
    gap: var(--focus-detail-space-sm, 8px);
    min-width: 0;
  }

  .obsolete-commit-row .commit-title {
    color: var(--text-secondary);
  }

  .system-event-summary,
  .system-event-link {
    flex: 1;
    font-size: var(--font-size-sm);
  }

  .system-event-summary {
    color: var(--text-secondary);
  }

  .system-event-summary--sentence {
    flex: 0 1 auto;
  }

  .system-event-link {
    color: var(--accent-blue);
    text-decoration: none;
  }

  .system-event-link:hover {
    text-decoration: underline;
  }

  /* Body wrap for copy button positioning */
  .event-body-wrap {
    position: relative;
    margin-top: var(--focus-detail-space-sm, 8px);
  }

  .event-body-wrap--nested {
    margin-top: 2px;
  }

  .event-body-wrap--with-thread {
    display: flow-root;
  }

  .event-body-wrap--with-thread :global(.thread-snippet) {
    margin-bottom: var(--focus-detail-space-xs, 6px);
  }

  .thread-controls {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--focus-detail-space-xs, 6px);
    margin-top: var(--focus-detail-space-sm, 8px);
  }

  .event-card--reply-inline {
    display: flow-root;
  }

  .thread-toggle {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    min-height: 23px;
    padding: 2.5px 6px 2.5px 3px;
    border-radius: var(--radius-sm);
    color: var(--accent-blue);
    font-size: var(--font-size-sm);
    font-weight: 600;
  }

  .thread-toggle:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .thread-toggle:disabled {
    opacity: var(--opacity-disabled);
    cursor: not-allowed;
  }

  .thread-reply-action {
    color: var(--text-secondary);
  }

  .thread-reply-panel {
    padding: var(--focus-detail-space-sm, 8px) 0 2px 17.5px;
  }

  .thread-replies {
    list-style: none;
    display: flex;
    flex-direction: column;
    gap: 0;
    margin-top: 2.5px;
    padding-left: 0;
  }

  .thread-reply {
    display: grid;
    grid-template-columns: 17.5px minmax(0, 1fr);
    column-gap: 0;
    min-width: 0;
    --thread-reply-header-padding-block: 2.5px;
    --thread-reply-header-line-height: 15px;
  }

  .thread-reply-rail {
    position: relative;
    min-height: 19.5px;
    --thread-dot-size: 6.5px;
    --thread-dot-center-y: calc(var(--thread-reply-header-padding-block) + 7.5px);
  }

  .thread-reply-rail::before {
    content: "";
    position: absolute;
    top: 0;
    bottom: 0;
    left: calc(var(--thread-dot-size) / 2);
    width: 2px;
    background: var(--border-default);
    transform: translateX(-50%);
  }

  .thread-reply--first .thread-reply-rail::before {
    top: var(--thread-dot-center-y);
  }

  .thread-reply--last .thread-reply-rail::before {
    bottom: calc(100% - var(--thread-dot-center-y));
  }

  .thread-reply--first.thread-reply--last .thread-reply-rail::before {
    display: none;
  }

  .thread-reply-dot {
    position: absolute;
    top: calc(var(--thread-dot-center-y) - var(--thread-dot-size) / 2);
    left: 0;
    width: var(--thread-dot-size);
    height: var(--thread-dot-size);
    border-radius: 50%;
    background: var(--accent-blue);
    box-shadow: 0 0 0 2.5px var(--bg-surface);
    z-index: 1;
  }

  .thread-reply-content {
    min-width: 0;
    padding: var(--thread-reply-header-padding-block) 0;
  }

  .thread-reply-header {
    min-width: 0;
    min-height: var(--thread-reply-header-line-height);
    align-items: center;
  }

  .thread-reply-actions {
    margin-left: auto;
  }

  .thread-reply-header .event-type {
    color: var(--accent-blue);
  }

  .thread-reply-header .event-author {
    color: var(--text-secondary);
  }

  .delete-confirmation {
    display: grid;
    gap: var(--space-3);
  }

  .delete-confirmation__message,
  .delete-confirmation__hint,
  .delete-confirmation__error {
    margin: 0;
  }

  .delete-confirmation blockquote {
    max-height: 7.5rem;
    overflow: auto;
    margin: 0;
    padding: var(--space-3);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.45;
  }

  .delete-confirmation__hint {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .delete-confirmation__error {
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }

  .event-body {
    font-size: var(--font-size-sm);
    color: var(--text-primary);
    padding: 0 calc(var(--focus-detail-hit-target, 26px) + var(--focus-detail-space-sm, 8px)) var(--focus-detail-space-sm, 8px) var(--focus-detail-space-sm, 10px);
    white-space: pre-wrap;
    word-break: break-word;
    line-height: 1.6;
  }

  .event-body-wrap--with-thread .event-body {
    padding-top: 2.5px;
    padding-right: var(--focus-detail-space-xs, 6px);
  }

  :global(.event-card--reply-inline) .event-body {
    padding-bottom: 0;
  }

  .event-body.markdown-body {
    white-space: normal;
  }

  .event-body.markdown-body > :global(:first-child) {
    margin-top: 0;
  }

  .event-body--nested {
    padding: 1.5px calc(var(--focus-detail-hit-target, 26px) + var(--focus-detail-space-sm, 8px)) 2px 0;
    line-height: 1.25;
  }

  .event-body--with-inline-reply {
    position: relative;
    display: block;
  }

  .event-body--with-inline-reply::after {
    content: "";
    display: block;
    clear: both;
  }

  .event-body--with-inline-reply :global(.thread-reply-inline-float) {
    float: right;
    clear: right;
    display: inline-flex;
    margin-left: var(--focus-detail-space-sm, 10px);
  }

  .event-body--with-inline-reply :global(.thread-reply-action--inline) {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    min-height: 23px;
    padding: 2.5px 6px 2.5px 3px;
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: 600;
    opacity: 0;
    transition: opacity 0.15s, background 0.15s, color 0.15s;
    vertical-align: text-bottom;
  }

  :global(.event-card--reply-inline:hover) .event-body--with-inline-reply :global(.thread-reply-action--inline),
  .event-body--with-inline-reply :global(.thread-reply-action--inline:focus-visible),
  .event-body--with-inline-reply :global(.thread-reply-action--inline[aria-expanded="true"]) {
    opacity: 1;
  }

  .event-body--with-inline-reply :global(.thread-reply-action--inline:hover) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .event-body--with-inline-reply :global(.thread-reply-action--inline:disabled) {
    opacity: var(--opacity-disabled);
    cursor: not-allowed;
  }

  @media (hover: none) {
    .event-body--with-inline-reply :global(.thread-reply-action--inline) {
      opacity: 1;
    }
  }

  .event-body--with-inline-reply > :global(:is(p, h1, h2, h3, h4, h5, h6):first-of-type)::before {
    content: "";
    float: right;
    width: calc(var(--focus-detail-hit-target, 26px) + var(--focus-detail-space-sm, 8px));
    height: calc(var(--focus-detail-hit-target, 26px) + var(--focus-detail-space-xs, 6px));
  }

  .edit-panel {
    padding: var(--focus-detail-space-sm, 8px) 0 2px;
  }

  .edit-actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--focus-detail-space-sm, 8px);
    margin-top: var(--focus-detail-space-sm, 8px);
  }

  .edit-action {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    min-height: var(--focus-detail-hit-target, 28px);
    padding: 5px var(--focus-detail-space-sm, 10px);
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-default);
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: 600;
  }

  .edit-action--primary {
    border-color: var(--accent-blue);
    background: var(--accent-blue);
    color: white;
  }

  .edit-action:disabled {
    opacity: var(--opacity-disabled);
    cursor: not-allowed;
  }

  .edit-action:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .edit-action--primary:hover:not(:disabled) {
    background: color-mix(in srgb, var(--accent-blue) 86%, black);
    color: white;
  }

  .edit-error {
    margin-top: var(--focus-detail-space-xs, 6px);
    font-size: var(--font-size-sm);
    color: var(--accent-red);
  }

  .empty-edit-btn {
    position: static;
    opacity: 1;
    margin-top: var(--focus-detail-space-sm, 8px);
  }
</style>

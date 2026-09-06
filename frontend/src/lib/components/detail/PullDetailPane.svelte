<script module lang="ts">
  /**
   * Diff scroll offsets, keyed by pull request and shared across every host.
   *
   * Module scope on purpose: the diff pane unmounts whenever it is not on screen
   * (a leaf renders all of its tabs, so an always-mounted diff would fetch one
   * per PR the user merely selects), and it remounts on a PR change. Without this
   * the reader loses their place on every pane switch.
   */
  const filesScrollPositions: Record<string, number> = Object.create(null) as Record<string, number>;
</script>

<script lang="ts">
  import PullDetail from "./PullDetail.svelte";
  import DiffFilesLayout from "../diff/DiffFilesLayout.svelte";
  import { reviewThreadsFromEvents } from "../diff/review-thread-context.js";
  import type { ProviderCapabilities, PullDetail as PullDetailResponse } from "../../api/types.js";
  import type { DetailSyncMode } from "../../stores/detail.svelte.js";
  import type { PullRequestRouteRef } from "../../routes.js";
  import type { InlineWorkspaceController } from "../../workspace-inline.js";

  interface Props {
    /** Which pane body to render: "conversation" or "files". */
    tabKey: string;
    /** Whether this pane is on screen; the diff mounts only when it is. */
    visible: boolean;
    /** Whether this pane owns window-level keyboard commands. */
    keyboardActive: boolean;
    pr: PullRequestRouteRef;
    /** The loaded detail, already confirmed to belong to `pr`, or null. */
    detail: PullDetailResponse | null;
    autoSync?: DetailSyncMode;
    hideStaleWhileLoading?: boolean;
    workflowApprovalSync?: boolean;
    inlineWorkspace?: InlineWorkspaceController | null;
    onStackMemberNavigate?: ((ref: PullRequestRouteRef) => boolean | void) | undefined;
    onDetailTabChange?: ((tab: "conversation" | "files") => void) | undefined;
    onOpenWorkspace?: ((workspaceId: string) => void) | undefined;
    onViewWorkspaces?: (() => void) | undefined;
    /** Phone-like routes render PR actions as one kit action grid. */
    phonePresentation?: boolean;
  }

  const {
    tabKey,
    visible,
    keyboardActive,
    pr,
    detail,
    autoSync = "background",
    hideStaleWhileLoading = false,
    workflowApprovalSync = true,
    inlineWorkspace = null,
    onStackMemberNavigate = undefined,
    onDetailTabChange,
    onOpenWorkspace,
    onViewWorkspaces,
    phonePresentation = false,
  }: Props = $props();

  // Provider capabilities are unknown until the detail lands. Assuming the
  // permissive set would render mutation controls a provider cannot honor, but
  // the diff needs something concrete to mount against, so this is deliberately
  // the GitHub-shaped default the PR detail used before panes existed.
  const defaultProviderCapabilities: ProviderCapabilities = {
    read_repositories: true,
    read_merge_requests: true,
    read_issues: true,
    read_issue_pr_references: false,
    read_comments: true,
    read_releases: true,
    read_labels: true,
    read_markdown_images: false,
    read_authenticated_user: false,
    read_ci: true,
    read_workflows: false,
    read_workflow_runs: false,
    workflow_dispatch: false,
    comment_mutation: true,
    thread_reply: false,
    thread_resolve: false,
    label_mutation: true,
    assignee_mutation: false,
    reviewer_mutation: false,
    state_mutation: true,
    merge_mutation: true,
    review_mutation: true,
    workflow_approval: true,
    ready_for_review: true,
    draft_mutation: true,
    issue_mutation: true,
    review_draft_mutation: false,
    review_thread_resolution: false,
    review_suggestion_application: false,
    read_review_threads: false,
    native_multiline_ranges: false,
    mutation_head_binding: false,
    supported_review_actions: [],
  };

  const scrollKey = $derived(
    [pr.provider, pr.platformHost ?? "", pr.repoPath, pr.number].join("\0"),
  );

  function rememberFilesScroll(scrollTop: number): void {
    filesScrollPositions[scrollKey] = scrollTop;
  }
</script>

{#if tabKey === "conversation"}
  <PullDetail
    owner={pr.owner}
    name={pr.name}
    number={pr.number}
    provider={pr.provider}
    platformHost={pr.platformHost}
    repoPath={pr.repoPath}
    {autoSync}
    {workflowApprovalSync}
    hideTabs={true}
    {hideStaleWhileLoading}
    onStackMemberNavigate={onStackMemberNavigate ?? (() => undefined)}
    {onDetailTabChange}
    {onOpenWorkspace}
    {onViewWorkspaces}
    {inlineWorkspace}
    {phonePresentation}
  />
{:else if tabKey === "files" && visible}
  <!-- Keyed on the PR because DiffFilesLayout holds per-file state that must not
       leak across pull requests. -->
  {#key scrollKey}
    <DiffFilesLayout
      owner={pr.owner}
      name={pr.name}
      number={pr.number}
      provider={pr.provider}
      platformHost={pr.platformHost}
      repoPath={pr.repoPath}
      diffHeadSHA={detail?.diff_head_sha}
      capabilities={detail?.repo?.capabilities ?? defaultProviderCapabilities}
      operations={detail?.repo?.operations}
      reviewThreads={reviewThreadsFromEvents(detail?.events)}
      initialScrollTop={filesScrollPositions[scrollKey] ?? 0}
      onScrollTopChange={rememberFilesScroll}
      {keyboardActive}
    />
  {/key}
{/if}

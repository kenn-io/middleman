# Inline Review Comments

Use this document for inline pull-request diff comments, local review drafts,
published review-thread ingestion, or review controls in shared diff UI.

- Staged review comments are hub-owned kenn-forge data. Spokes forward
  draft reads, writes, and publication so one fleet has one draft and conflict
  domain; do not depend on provider-native pending drafts
  (`internal/db/queries_review.go::DB.GetOrCreateMRReviewDraft`,
  `internal/server/provider_route_policy.go::providerRouteDeclarations`).
- Before a standalone daemon becomes a spoke, its draft body, review action, and
  ordered inline anchors are handed to the hub by stable repository ID
  and pull number. Local row IDs and timestamps are excluded from the canonical
  digest. Identical retries return the same receipt; divergent hub state
  is a blocking conflict and neither side is overwritten
  (`internal/db/queries_provider_state_handoff.go::DB.ImportProviderReviewDraft`).
- Unsupported draft mutations, thread resolutions, and review actions return
  typed capability errors; thread ingestion is skipped when reads are unavailable
  (`internal/server/pullapi/diff_review_handlers.go::Handler.publishDiffReviewDraft`).
- Bind drafts to the full provider reference, pull request number, and diff head
  SHA. Reject publish when the saved diff head is stale
  (`internal/server/pullapi/diff_review_handlers.go::Handler.publishDiffReviewDraft`).
- Known-stale published line threads stay visible after the rendered file and never attach by numeric coincidence; current or head-unknown representable threads stay inline, while true file-level comments remain at the file header (`frontend/src/lib/components/diff/review-thread-context.ts::reviewThreadSnapshotState`, `frontend/src/lib/components/diff/DiffFile.svelte`, `frontend/src/lib/components/diff/DiffRichPreview.svelte`).
- Review ranges stay within one file and one diff side and are contiguous in
  rendered order. Full pull-request head diffs are reviewable; single-commit and
  arbitrary range diffs remain disabled until their coordinate mapping is
  explicitly supported
  (`internal/server/pullapi/diff_review_handlers.go::dbReviewLineRange`).
- Line-number pointer and keyboard selection only change the active review
  range. The gutter utility opens the composer for that range
  (`frontend/src/lib/components/diff/DiffFile.svelte::handlePierreGutterUtility`).
- Keep unsaved composer text above renderer-owned annotation mounts: context
  expansion can remount the composer without changing its review range
  (`frontend/src/lib/components/diff/DiffFile.svelte::composerBody`).
- Public routes use provider-aware pull paths and internal draft/thread IDs.
  Provider review, thread, and comment IDs remain persistence metadata unless a
  concrete public API need is introduced
  (`internal/server/pullapi/handler.go::registerReviewDraftRoutes`).
- Published review parts appear in the selected pull request timeline, not as
  standalone global activity rows
  (`internal/server/pullapi/diff_review_handlers.go::Handler.ingestDiffReviewThreads`).
- Provider moderation metadata belongs to both thread rows and their corresponding
  timeline events; ingestion commits both representations atomically so neither can
  clear or bypass it independently
  (`internal/server/pullapi/diff_review_handlers.go::Handler.ingestDiffReviewThreads`).
- Provider publication is irreversible; follow-up thread-ingestion failures preserve
  the published or partially-published response and schedule reconciliation
  (`internal/server/pullapi/diff_review_handlers.go::Handler.publishDiffReviewDraft`).
- Workspace diffs reuse the renderer with review mode disabled. They must not
  expose composers, draft trays, publish actions, or thread-resolution controls,
  and replacing a pull-request diff with workspace state clears the draft store
  (`packages/ui/src/components/workspace/WorkspaceDiffPanel.svelte::reviewMode`,
  `packages/ui/src/components/diff/DiffView.svelte::reviewEnabled`).

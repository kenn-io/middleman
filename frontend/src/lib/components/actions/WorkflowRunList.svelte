<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import ExternalLinkIcon from "@lucide/svelte/icons/external-link";
  import { isSafeExternalHTTPURL } from "../../utils/safe-external-url.js";
  import type { components } from "../../api/generated/schema.js";

  type Run = components["schemas"]["WorkflowRunResponse"];
  type Job = components["schemas"]["WorkflowRunJobResponse"];

  interface Props {
    runs: readonly Run[];
    jobs: Readonly<Record<string, readonly Job[]>>;
    loadingJobs: readonly string[];
    onexpand: (runId: string) => void;
  }

  let { runs, jobs, loadingJobs, onexpand }: Props = $props();
  let expandedRuns = $state<Record<string, boolean>>({});
  let expandedJobs = $state<Record<string, boolean>>({});

  function statusText(status: string, conclusion: string): string {
    return conclusion ? `${status} · ${conclusion}` : status;
  }
  function toggleRun(id: string): void {
    const expanded = expandedRuns[id] === true;
    expandedRuns[id] = !expanded;
    if (!expanded) onexpand(id);
  }
  function providerName(url: string): string {
    try {
      const parsed = new URL(url);
      const hostname = parsed.hostname;
      if (hostname === "github.com" || hostname.endsWith(".github.com")) return "GitHub";
      if (hostname === "gitlab.com" || hostname.endsWith(".gitlab.com")) return "GitLab";
      return parsed.host || "provider";
    } catch { return "provider"; }
  }
</script>

<div class="run-list" role="list" aria-label="Workflow runs">
  {#each runs as run (run.id)}
    {@const safeRunURL = run.web_url && isSafeExternalHTTPURL(run.web_url) ? run.web_url : undefined}
    <section class="run" role="listitem">
      <div class="run-row">
        <Button class="run-disclosure" surface="soft" ariaExpanded={expandedRuns[run.id] === true} ariaLabel={`Run ${run.run_number} ${run.name}`} onclick={() => toggleRun(run.id)}>
          <ChevronRightIcon size={14} aria-hidden="true" class={`run-chevron${expandedRuns[run.id] === true ? " expanded" : ""}`} />
          <span class="run-primary" title={`#${run.run_number} ${run.name}`}>
            <span class="number">#{run.run_number}</span><strong>{run.name}</strong>
          </span>
          <code class="run-ref" title={run.ref}>{run.ref}</code>
          <span class="run-actor" title={run.actor}>{run.actor}</span>
          <span class="status run-status" data-status={run.conclusion || run.status}>{statusText(run.status, run.conclusion)}</span>
          {#if run.created_at}
            {@const localCreatedAt = new Date(run.created_at).toLocaleString()}
            <time class="run-time" datetime={run.created_at} title={localCreatedAt}>{localCreatedAt}</time>
          {/if}
          <code class="run-sha" title={run.head_sha}>{run.head_sha.slice(0, 7)}</code>
        </Button>
        {#if safeRunURL}
          <a href={safeRunURL} target="_blank" rel="noopener" aria-label={`Open on ${providerName(safeRunURL)}`}><ExternalLinkIcon size={14} aria-hidden="true" /></a>
        {/if}
      </div>
      {#if expandedRuns[run.id]}
        <div class="jobs" aria-label={`Jobs for run ${run.run_number}`}>
          {#if loadingJobs.includes(run.id)}
            <p role="status">Loading jobs…</p>
          {:else if (jobs[run.id]?.length ?? 0) === 0}
            <p>No jobs available.</p>
          {:else}
            {#each jobs[run.id] ?? [] as job (job.id)}
              <div class="job">
                <button class="job-row" type="button" aria-expanded={expandedJobs[job.id] === true} onclick={() => { expandedJobs[job.id] = expandedJobs[job.id] !== true; }}>
                  <ChevronRightIcon size={13} aria-hidden="true" class={expandedJobs[job.id] === true ? "expanded" : undefined} />
                  <span>{job.name}</span><span class="status" data-status={job.conclusion || job.status}>{statusText(job.status, job.conclusion)}</span>
                </button>
                {#if expandedJobs[job.id] && job.steps}
                  <ol class="steps" aria-label={`${job.name} steps`}>
                    {#each job.steps as step (step.number)}<li><span>{step.name}</span><span class="status" data-status={step.conclusion || step.status}>{statusText(step.status, step.conclusion)}</span></li>{/each}
                  </ol>
                {/if}
              </div>
            {/each}
          {/if}
        </div>
      {/if}
    </section>
  {/each}
</div>

<style>
  .run-list { container-type: inline-size; display: grid; min-width: 0; overflow-x: clip; border-block-start: 1px solid var(--border-subtle); }
  .run { min-width: 0; border-block-end: 1px solid var(--border-subtle); }
  .run-row { width: 100%; min-width: 0; display: grid; grid-template-columns: minmax(0, 1fr) 30px; align-items: center; }
  :global(.run-disclosure) {
    width: 100%;
    min-width: 0;
    justify-content: start;
    display: grid;
    grid-template-columns: auto minmax(100px, 1fr) minmax(70px, 0.7fr) minmax(64px, 0.5fr) auto auto auto;
    gap: var(--space-3);
    text-align: left;
  }
  .run-primary { min-width: 0; display: flex; gap: var(--space-2); align-items: baseline; }
  .run-primary .number { flex: 0 0 auto; }
  .run-primary strong, .run-ref, .run-actor, .run-time, .run-sha {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .run-primary strong { flex: 1 1 auto; }
  :global(.run-disclosure svg), .job-row :global(svg) { transition: none; }
  :global(.run-disclosure svg.expanded), .job-row :global(svg.expanded) { transform: rotate(90deg); }
  a { display: grid; place-items: center; color: var(--text-secondary); min-height: 30px; }
  code, time, .status, .jobs { font-size: var(--font-size-xs); color: var(--text-secondary); }
  .jobs { min-width: 0; padding: 0 0 var(--space-2) var(--space-6); }
  .jobs p { margin: var(--space-2); }
  .job-row, .steps li { box-sizing: border-box; width: 100%; min-width: 0; display: grid; grid-template-columns: auto minmax(0, 1fr) auto; gap: var(--space-2); align-items: center; min-height: 28px; border: 0; background: transparent; color: var(--text-primary); text-align: left; }
  .job-row { cursor: pointer; }
  .job-row span, .steps li span { min-width: 0; overflow-wrap: anywhere; }
  .steps { margin: 0; padding-inline-start: var(--space-6); }
  .steps li { grid-template-columns: minmax(0, 1fr) auto; }
  [data-status="success"] { color: var(--status-success-text, var(--text-success)); }
  [data-status="failure"], [data-status="cancelled"] { color: var(--status-danger-text, var(--text-danger)); }

  @container (max-width: 620px) {
    :global(.run-disclosure) {
      grid-template-columns: auto minmax(0, 1fr) minmax(0, auto);
      grid-template-areas:
        "chevron primary status"
        ". ref ref"
        ". actor actor"
        ". time time"
        ". sha sha";
      column-gap: var(--space-2);
      row-gap: var(--space-1);
      align-items: start;
    }
    :global(.run-chevron) { grid-area: chevron; }
    .run-primary { grid-area: primary; }
    .run-ref { grid-area: ref; }
    .run-actor { grid-area: actor; }
    .run-status { grid-area: status; max-width: 100%; white-space: normal; text-align: end; overflow-wrap: anywhere; }
    .run-time { grid-area: time; }
    .run-sha { grid-area: sha; }
    .jobs { padding-inline-start: var(--space-4); }
  }
</style>

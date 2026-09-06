<script lang="ts">
  import { Checkbox } from "@kenn-io/kit-ui";
  import { Effect } from "effect";
  import { DEFAULT_MODE_VISIBILITY } from "../../api/types.js";
  import { getStores } from "../../context.js";
  import { showFlash } from "../../stores/flash.svelte.js";
  import type { ModeVisibility } from "../../api/types.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { isEmbedded } from "../../stores/embed-config.svelte.js";
  import { SettingsWorkflow, settingsErrorMessage } from "../../stores/settings-workflow.js";

  type ModeKey = keyof ModeVisibility;

  interface ModeOption {
    key: ModeKey;
    label: string;
  }

  interface Props {
    modes: ModeVisibility | null | undefined;
    onUpdate: (modes: ModeVisibility) => void;
    compact?: boolean;
    saveLabel?: string;
    onSavingChange?: (saving: boolean) => void;
  }

  let {
    modes,
    onUpdate,
    compact = false,
    saveLabel = "Save",
    onSavingChange,
  }: Props = $props();

  const { settings: settingsStore } = getStores();
  const runtime = getAppRuntime();
  const embedded = isEmbedded();

  const modeOptions: ModeOption[] = [
    { key: "activity", label: "Activity" },
    { key: "repos", label: "Repos" },
    { key: "docs", label: "Docs" },
    { key: "actions", label: "Actions" },
    { key: "pulls", label: "PRs" },
    { key: "issues", label: "Issues" },
    { key: "reviews", label: "Reviews" },
    { key: "workspaces", label: "Workspaces" },
  ];

  let draft = $state<ModeVisibility>({ ...DEFAULT_MODE_VISIBILITY });
  let source = $state<ModeVisibility>({ ...DEFAULT_MODE_VISIBILITY });
  let saving = $state(false);

  function normalizeModes(value: ModeVisibility | null | undefined): ModeVisibility {
    return {
      ...DEFAULT_MODE_VISIBILITY,
      ...(value ?? {}),
    };
  }

  function sameModes(left: ModeVisibility, right: ModeVisibility): boolean {
    return modeOptions.every((option) => left[option.key] === right[option.key]);
  }

  $effect(() => {
    const next = normalizeModes(modes);
    if (sameModes(next, source)) return;
    source = next;
    draft = next;
  });

  const canSave = $derived(!saving && !sameModes(draft, source));

  function toggleMode(key: ModeKey): void {
    draft = {
      ...draft,
      [key]: !draft[key],
    };
  }

  function save(): void {
    if (embedded) return;
    if (!canSave) return;

    saving = true;
    onSavingChange?.(true);
    const pendingModes = normalizeModes(draft);
    runtime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* SettingsWorkflow;
        return yield* workflow.persist(() => ({ modes: pendingModes }));
      }).pipe(
        Effect.matchEffect({
          onFailure: (failure) =>
            Effect.sync(() => {
              draft = source;
              showFlash(settingsErrorMessage(failure), { tone: "danger" });
            }),
          onSuccess: (settings) =>
            Effect.sync(() => {
              const updated = normalizeModes(settings.modes ?? pendingModes);
              source = updated;
              draft = updated;
              onUpdate(updated);
              settingsStore.setModeVisibility(updated);
            }),
        }),
        Effect.ensuring(Effect.sync(() => {
          saving = false;
          onSavingChange?.(false);
        })),
      ),
      {
        operation: "save visible modes",
        safeContext: {},
        onFailure: () => {},
      },
    );
  }
</script>

<div class={["mode-visibility-settings", compact && "compact"].filter(Boolean).join(" ")}>
  <div class="mode-grid">
    {#each modeOptions as option (option.key)}
      <Checkbox
        class="mode-toggle"
        checked={draft[option.key]}
        disabled={saving}
        label={option.label}
        onchange={(checked) => {
          if (checked !== draft[option.key]) toggleMode(option.key);
        }}
      />
    {/each}
  </div>

  <div class="actions">
    <button
      class="save-btn"
      type="button"
      disabled={!canSave}
      onclick={save}
    >
      {saving ? "Saving..." : saveLabel}
    </button>
  </div>
</div>

<style>
  .mode-visibility-settings {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .mode-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-4) var(--space-5);
  }

  .mode-visibility-settings :global(.mode-toggle) {
    min-width: 0;
    line-height: 1.2;
  }

  .mode-visibility-settings :global(.mode-toggle .kit-checkbox__label) {
    overflow-wrap: anywhere;
  }

  .actions {
    display: flex;
    justify-content: flex-end;
  }

  .save-btn {
    min-height: 28px;
    padding: 5px 12px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: 600;
  }

  .save-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .save-btn:disabled {
    opacity: var(--opacity-disabled);
    cursor: not-allowed;
  }

  .compact {
    gap: var(--space-4);
  }

  .compact .mode-grid {
    gap: var(--space-3) var(--space-5);
  }
</style>

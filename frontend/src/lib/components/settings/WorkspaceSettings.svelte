<script lang="ts">
  import { SelectDropdown, type SelectDropdownOption } from "@kenn-io/kit-ui";
  import { getShowListAgentStatus, setShowListAgentStatus } from "../../stores/list-agent-status.svelte.js";
  import { Effect } from "effect";
  import type { Settings } from "../../api/types.js";

  import { getAppRuntime } from "../../app/runtime-context.js";
  import { getStores } from "../../context.js";
  import { isEmbedded } from "../../stores/embed-config.svelte.js";
  import { settingsErrorMessage } from "../../stores/settings-workflow.js";
  import { saveWorkspaceSettings } from "../../stores/workspace-settings-persistence.js";
  import { saveRoborevSettings } from "../../stores/roborev-settings-persistence.js";

  interface Props {
    onUpdate: (settings: Settings["workspaces"]) => void;
    onRoborevUpdate?: (settings: Settings["roborev"]) => void;
  }

  let { onUpdate, onRoborevUpdate = () => {} }: Props = $props();
  const runtime = getAppRuntime();
  const { settings: settingsStore } = getStores();
  const workspaces = $derived(settingsStore.getWorkspaceSettings());
  const roborev = $derived(settingsStore.getRoborevSettings());
  const embedded = isEmbedded();
  let saving = $state(false);
  let savingRoborev = $state(false);
  const defaultSidebarViewOptions: SelectDropdownOption[] = [
    { value: "diff", label: "Diff" },
    { value: "item", label: "PR/Issue" },
  ];

  function toggleAutoAssign(): void {
    if (embedded || saving) return;
    const baseline = workspaces;
    const pending = {
      ...workspaces,
      auto_assign_on_create: !workspaces.auto_assign_on_create,
    };
    onUpdate(pending);
    saving = true;
    runtime.runCommand(
      Effect.gen(function* () {
        return yield* saveWorkspaceSettings({
          baseline,
          changes: { auto_assign_on_create: pending.auto_assign_on_create },
          store: settingsStore,
        });
      }).pipe(
        Effect.matchEffect({
          onFailure: (failure) =>
            Effect.sync(() => {
              onUpdate(settingsStore.getWorkspaceSettings());
              console.warn("Failed to save workspace settings:", settingsErrorMessage(failure));
            }),
          onSuccess: () => Effect.sync(() => onUpdate(settingsStore.getWorkspaceSettings())),
        }),
        Effect.ensuring(Effect.sync(() => {
          saving = false;
        })),
      ),
      {
        operation: "save workspace settings",
        safeContext: {},
        onFailure: () => {},
      },
    );
  }

  function setDefaultSidebarView(value: string): void {
    const defaultSidebarView = value as Settings["workspaces"]["default_sidebar_view"];
    if (embedded || saving || defaultSidebarView === workspaces.default_sidebar_view) return;
    const baseline = workspaces;
    const pending = { ...workspaces, default_sidebar_view: defaultSidebarView };
    onUpdate(pending);
    saving = true;
    runtime.runCommand(
      Effect.gen(function* () {
        return yield* saveWorkspaceSettings({
          baseline,
          changes: { default_sidebar_view: pending.default_sidebar_view },
          store: settingsStore,
        });
      }).pipe(
        Effect.matchEffect({
          onFailure: (failure) => Effect.sync(() => {
            onUpdate(settingsStore.getWorkspaceSettings());
            console.warn("Failed to save workspace settings:", settingsErrorMessage(failure));
          }),
          onSuccess: () => Effect.sync(() => onUpdate(settingsStore.getWorkspaceSettings())),
        }),
        Effect.ensuring(Effect.sync(() => { saving = false; })),
      ),
      { operation: "save workspace settings", safeContext: {}, onFailure: () => {} },
    );
  }

  function toggleRoborevManagedClones(): void {
    if (embedded || savingRoborev) return;
    const baseline = roborev;
    const pending = {
      ...roborev,
      init_managed_clones: !roborev.init_managed_clones,
    };
    onRoborevUpdate(pending);
    savingRoborev = true;
    runtime.runCommand(
      Effect.gen(function* () {
        return yield* saveRoborevSettings({
          baseline,
          changes: { init_managed_clones: pending.init_managed_clones },
          store: settingsStore,
        });
      }).pipe(
        Effect.matchEffect({
          onFailure: (failure) =>
            Effect.sync(() => {
              onRoborevUpdate(settingsStore.getRoborevSettings());
              console.warn("Failed to save Roborev settings:", settingsErrorMessage(failure));
            }),
          onSuccess: () => Effect.sync(() => onRoborevUpdate(settingsStore.getRoborevSettings())),
        }),
        Effect.ensuring(
          Effect.sync(() => {
            savingRoborev = false;
          }),
        ),
      ),
      { operation: "save Roborev settings", safeContext: {}, onFailure: () => {} },
    );
  }
</script>

<div class="setting-row">
  <div class="setting-copy">
    <span class="setting-label">Show agent status in lists</span>
    <span class="setting-description">Show Working, Approval, Input, and Done in PR, Activity, and Issue lists. Saved in this browser.</span>
  </div>
  <button
    class={["toggle-btn", getShowListAgentStatus() && "toggle-on"]}
    type="button"
    onclick={() => setShowListAgentStatus(!getShowListAgentStatus())}
    aria-label="Show agent status in lists"
    aria-pressed={getShowListAgentStatus()}
  >
    <span class="toggle-track"><span class="toggle-thumb"></span></span>
  </button>
</div>


<div class="settings-list">
  <div class="setting-row">
    <div class="setting-copy">
      <span class="setting-label">Assign new workspace items to me</span>
      <span class="setting-description">
        When creating a workspace from a pull request or issue, add your provider account as an assignee so teammates can see that you are working on it.
      </span>
    </div>
    <button
      class={["toggle-btn", workspaces.auto_assign_on_create && "toggle-on"]}
      type="button"
      disabled={saving}
      onclick={toggleAutoAssign}
      aria-label="Assign new workspace items to me"
      aria-pressed={workspaces.auto_assign_on_create}
    >
      <span class="toggle-track"><span class="toggle-thumb"></span></span>
    </button>
  </div>
  <div class="setting-row">
    <div class="setting-copy">
      <span class="setting-label">Initialize Roborev in managed clones</span>
      <span class="setting-description">
        Run Roborev initialization for Forge-managed workspaces so commits can trigger reviews. Requires a Roborev daemon on a loopback HTTP endpoint; Forge does not start it.
      </span>
    </div>
    <button
      class={["toggle-btn", roborev.init_managed_clones && "toggle-on"]}
      type="button"
      disabled={embedded || savingRoborev}
      onclick={toggleRoborevManagedClones}
      aria-label="Initialize Roborev in managed clones"
      aria-pressed={roborev.init_managed_clones}
    >
      <span class="toggle-track"><span class="toggle-thumb"></span></span>
    </button>
  </div>
  <div class="setting-row">
    <div class="setting-copy">
      <span class="setting-label">Default sidebar view</span>
      <span class="setting-description">Choose the initial details view for workspaces created from a pull request or issue.</span>
    </div>
    <SelectDropdown
      class="sidebar-view-select"
      title="Default sidebar view"
      value={workspaces.default_sidebar_view}
      options={defaultSidebarViewOptions}
      disabled={embedded || saving}
      onchange={setDefaultSidebarView}
    />
  </div>
</div>

<style>
  .settings-list { display: flex; flex-direction: column; gap: var(--space-4); }
  .setting-row { display: flex; align-items: center; justify-content: space-between; gap: var(--space-5); min-height: 44px; }
  .setting-copy { display: flex; flex-direction: column; gap: 4px; }
  .setting-label { color: var(--text-secondary); font-size: var(--font-size-md); }
  .setting-description { max-width: 64ch; color: var(--text-muted); font-size: var(--font-size-sm); line-height: 1.4; }
  .toggle-btn { flex: 0 0 auto; cursor: pointer; padding: 0; background: none; }
  .toggle-btn:disabled { cursor: wait; opacity: var(--opacity-disabled); }
  .toggle-track { display: block; width: 36px; height: 20px; border-radius: 10px; background: var(--bg-inset); border: 1px solid var(--border-muted); position: relative; transition: background 0.15s, border-color 0.15s; }
  .toggle-on .toggle-track { background: var(--accent-blue); border-color: var(--accent-blue); }
  .toggle-thumb { display: block; width: 14px; height: 14px; border-radius: 50%; background: white; position: absolute; top: 2px; left: 2px; transition: transform 0.15s; box-shadow: var(--shadow-sm); }
  .toggle-on .toggle-thumb { transform: translateX(16px); }
  :global(.sidebar-view-select) { min-width: 120px; }
</style>

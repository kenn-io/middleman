<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import FolderGit2Icon from "@lucide/svelte/icons/folder-git-2";
  import TrashIcon from "@lucide/svelte/icons/trash-2";
  import { Effect } from "effect";
  import { onDestroy, onMount, tick, untrack } from "svelte";
  import { getAppRuntime } from "../app/runtime-context.js";
  import type { AppExecution } from "../app/runtime.js";
  import { executeGeneratedApiRequest } from "../api/generated-api.js";
  import { canonicalRepoFilterValue, displayRepoFilterValue, normalizeRepoFilterSelection } from "../utils/repo-filter-values.js";
  import { getStores } from "../context.js";
  import type { ConfigRepo, Repo, RepoPreset } from "../api/types.js";
  import { canonicalProvider } from "../api/provider-routes.js";
  import type { RepoTreeOption } from "./repoTree.js";
  import RepoTreeNode from "./RepoTreeNode.svelte";
  import RepoPresetSaveDialog, {
    type RepoPresetSaveTarget,
  } from "./RepoPresetSaveDialog.svelte";
  import TreeCheckbox from "./TreeCheckbox.svelte";
  import ConfirmDialog from "./shared/ConfirmDialog.svelte";
  import {
    buildRepoTree,
    visibleRows,
    nodeSelectionState,
    toggleSubtree,
    type VisibleRow,
  } from "./repoTree.js";
  import { createRepoTreeExpansionStore } from "../stores/repoTreeExpansion.svelte.js";
  import { ChevronDownIcon } from "../icons.ts";
  import {
    clearGlobalRepoPresetAffinity,
    getGlobalRepoPresetAffinity,
    parseRepoFilterValue,
    serializeRepoFilterValue,
    setGlobalRepoPresetSelection,
  } from "../stores/filter.svelte.js";
  import {
    findMatchingRepoPreset,
    preferredRepoPreset,
    projectRepoPresetSelection,
    repoPresetRepositoriesForSelection,
    type RepoPresetCatalogEntry,
  } from "../stores/repo-presets.js";
  import {
    getWorkspaceRepoCatalog,
    isWorkspaceRepoCatalogReady,
  } from "../stores/workspace-repo-catalog.svelte.js";
  import {
    SettingsWorkflow,
    type SettingsError,
    type SettingsSnapshot,
    settingsErrorMessage,
  } from "../stores/settings-workflow.js";
  import { registerCheatsheetEntries } from "../stores/keyboard/registry.svelte.js";
  import { showFlash } from "../stores/flash.svelte.js";
  import { repoSelectorLabel, type RepoSelectorLabel } from "./repo-selector-label.js";

  interface Props {
    selected: string | undefined;
    onchange: (repo: string | undefined) => void;
    initialOpen?: boolean;
    allowPresetManagement?: boolean;
    mobile?: boolean;
  }

  let {
    selected,
    onchange,
    initialOpen = false,
    allowPresetManagement = true,
    mobile = false,
  }: Props = $props();

  const stores = getStores();
  const runtime = getAppRuntime();

  onMount(() => {
    if (initialOpen) open = true;

    return registerCheatsheetEntries("repo-typeahead", [
      {
        id: "repo-typeahead.next",
        label: "Next repo",
        binding: { key: "ArrowDown" },
        scope: "view-pulls",
      },
      {
        id: "repo-typeahead.prev",
        label: "Previous repo",
        binding: { key: "ArrowUp" },
        scope: "view-pulls",
      },
      {
        id: "repo-typeahead.expand",
        label: "Expand / collapse group",
        binding: { key: "ArrowRight" },
        scope: "view-pulls",
      },
      {
        id: "repo-typeahead.toggle-select",
        label: "Select / deselect",
        binding: { key: " " },
        scope: "view-pulls",
      },
    ]);
  });

  let fetchedRepos = $state<Repo[]>([]);
  let reposLoading = $state(false);
  let query = $state("");
  let open = $state(false);
  let highlightIndex = $state(0);
  let inputEl = $state<HTMLInputElement>();
  let containerEl = $state<HTMLDivElement>();
  let openExecution: AppExecution<void, never> | null = null;
  let mutationExecution: AppExecution<void, never> | null = null;
  let saveDialogOpen = $state(false);
  let deletePreset = $state<RepoPreset>();
  let mutationBusy = $state(false);
  let mutationError = $state<string>();

  onDestroy(() => {
    openExecution?.interrupt();
    mutationExecution?.interrupt();
  });

  type RepoOption = RepoTreeOption & RepoPresetCatalogEntry & { repoPath: string };

  $effect(() => {
    const configuredRepoCount = configuredRepos.length;
    const loaded = settingsLoaded;

    reposLoading = true;
    fetchedRepos = [];
    const execution = untrack(() => runtime.runCommand(
      executeGeneratedApiRequest("GET /repos", (generatedClient, signal) =>
        generatedClient.RepositoriesService.listRepos({ signal })
      ).pipe(
        Effect.matchEffect({
          onFailure: () => Effect.sync(() => {
            reposLoading = false;
          }),
          onSuccess: (repos) => Effect.sync(() => {
            reposLoading = false;
            fetchedRepos = repos ?? [];
          }),
        }),
      ),
      {
        operation: "load repository typeahead options",
        safeContext: { configuredRepoCount, settingsLoaded: loaded },
        onFailure: () => {},
      },
    ));
    return execution.interrupt;
  });

  const configuredRepos = $derived(
    stores?.settings?.getConfiguredRepos?.() ?? [],
  );
  const settingsLoaded = $derived(
    stores?.settings?.isSettingsLoaded?.() ?? false,
  );
  const repoPresets = $derived(
    stores?.settings?.getRepoPresets?.() ?? [],
  );

  function optionFromRepo(repo: Repo): RepoOption {
    const repoPath = `${repo.Owner}/${repo.Name}`;
    return {
      value: `${repo.PlatformHost}/${repoPath}`,
      owner: repo.Owner,
      name: repo.Name,
      provider: canonicalProvider(repo.Platform),
      platformHost: repo.PlatformHost,
      platform_host: repo.PlatformHost,
      platform_repo_id: repo.PlatformRepoID,
      repoPath,
      repo_path: repoPath,
    };
  }

  function optionFromConfigRepo(repo: ConfigRepo): RepoOption | null {
    if (repo.is_glob || repo.hidden_from_ui) return null;
    const path = repo.tracked_repo_path || repo.repo_path || `${repo.owner}/${repo.name}`;
    if (!repo.platform_host || !path) return null;
    return {
      value: `${repo.platform_host}/${path}`,
      owner: repo.owner,
      name: repo.name,
      provider: canonicalProvider(repo.provider),
      platformHost: repo.platform_host,
      platform_host: repo.platform_host,
      platform_repo_id: repo.platform_repo_id ?? "",
      repoPath: path,
      repo_path: path,
    };
  }

  function mergeOptions(
    configured: ConfigRepo[],
    fetched: Repo[],
    workspace: readonly RepoPresetCatalogEntry[],
  ): RepoOption[] {
    const merged: RepoOption[] = [];
    const hiddenRepos = configured.filter((repo) => !repo.is_glob && repo.hidden_from_ui);
    const isHidden = (option: RepoOption) => hiddenRepos.some((repo) => {
      if (
        canonicalProvider(repo.provider) !== option.provider
        || repo.platform_host !== option.platformHost
      ) return false;

      if (repo.platform_repo_id && option.platform_repo_id) {
        return repo.platform_repo_id === option.platform_repo_id;
      }

      return [repo.tracked_repo_path, repo.repo_path, `${repo.owner}/${repo.name}`]
        .some((path) => path === option.repoPath);
    });
    const addOption = (option: RepoOption) => {
      if (isHidden(option)) return;
      const identity = `${option.provider}|${option.platformHost}/${option.repoPath}`;
      const existingIndex = merged.findIndex(
        (candidate) => `${candidate.provider}|${candidate.platformHost}/${candidate.repoPath}` === identity,
      );
      if (existingIndex >= 0) {
        const existing = merged[existingIndex]!;
        if (!existing.platform_repo_id && option.platform_repo_id) merged[existingIndex] = option;
        return;
      }
      merged.push(option);
    };

    for (const repo of configured) {
      const option = optionFromConfigRepo(repo);
      if (option) addOption(option);
    }

    for (const repo of fetched) {
      addOption(optionFromRepo(repo));
    }
    for (const repo of workspace) {
      const [owner = "", ...nameParts] = repo.repo_path.split("/");
      addOption({
        ...repo,
        owner,
        name: nameParts.join("/"),
        platformHost: repo.platform_host,
        repoPath: repo.repo_path,
      });
    }

    const identities = merged.map((option) => ({
      provider: option.provider,
      platformHost: option.platformHost,
      repoPath: option.repoPath,
      isGlob: false,
    }));
    return merged.map((option, index) => ({
      ...option,
      value: canonicalRepoFilterValue(identities[index]!, identities) ?? option.value,
    }));
  }

  const options = $derived.by(() => {
    return mergeOptions(configuredRepos, fetchedRepos, getWorkspaceRepoCatalog());
  });

  const selectedValues = $derived(parseRepoFilterValue(selected));
  const selectedSet = $derived(new Set(selectedValues));
  const matchingPreset = $derived(
    findMatchingRepoPreset(repoPresets, selected, options, getGlobalRepoPresetAffinity()),
  );
  const preferredPreset = $derived(
    preferredRepoPreset(repoPresets, selected, getGlobalRepoPresetAffinity(), options),
  );
  const selectedPresetRepos = $derived(repoPresetRepositoriesForSelection(selected, options));
  const displayLabel = $derived.by((): RepoSelectorLabel => {
    if (selectedValues.length === 0) return { primary: "Global", full: "Global" };
    if (matchingPreset) return { primary: matchingPreset.name, full: matchingPreset.name };
    if (selectedValues.length === 1) return repoSelectorLabel(selectedValues[0]!, options);
    const label = `${selectedValues.length} repos`;
    return { primary: label, full: label };
  });
  const selectionSummary = $derived(
    selectedValues.length === 0
      ? "All repositories"
      : `${selectedValues.length} ${selectedValues.length === 1 ? "repository" : "repositories"}`,
  );

  const expansion = createRepoTreeExpansionStore();

  const tree = $derived(buildRepoTree(options));

  const rows = $derived(
    visibleRows(tree, { isCollapsed: expansion.isCollapsed, query }),
  );

  function rowAriaLabel(row: VisibleRow): string {
    return row.node.kind === "host" ? row.node.platformHost : displayRepoFilterValue(row.node.id);
  }

  function toggleRowSelect(row: VisibleRow) {
    onchange(serializeRepoFilterValue(toggleSubtree(row.node, selectedValues)));
  }

  function selectPreset(preset: RepoPreset): void {
    const next = projectRepoPresetSelection(
      preset,
      options,
    );
    setGlobalRepoPresetSelection(preset.name, next);
    onchange(next);
  }

  function toggleRowExpand(row: VisibleRow) {
    if (row.hasChildren) expansion.toggle(row.node.id);
  }

  $effect(() => {
    if (selectedValues.length === 0 || reposLoading) return;
    if (globalThis.location?.pathname.startsWith("/workspaces") && !isWorkspaceRepoCatalogReady()) return;
    const normalized = normalizeRepoFilterSelection(
      selected,
      options.map((option) => ({
        provider: option.provider,
        platformHost: option.platformHost,
        repoPath: option.repoPath,
        isGlob: false,
      })),
    );
    if (normalized !== selected) {
      onchange(normalized);
      return;
    }
    const validValues = new Set(options.map((option) => option.value));
    const next = selectedValues.filter((value) => validValues.has(value) || (value.includes("|") && !settingsLoaded));
    if (next.length === selectedValues.length) return;
    onchange(serializeRepoFilterValue(next));
  });

  function openDropdown(): void {
    openExecution?.interrupt();
    query = "";
    open = true;
    highlightIndex = 0;
    openExecution = runtime.runCommand(
      Effect.promise(() => tick()).pipe(
        Effect.andThen(Effect.sync(() => inputEl?.focus())),
      ),
      {
        operation: "focus repository typeahead",
        safeContext: {},
        onFailure: () => {},
      },
    );
  }

  function closeDropdown(): void {
    openExecution?.interrupt();
    openExecution = null;
    open = false;
    query = "";
  }

  function toggleDropdown(): void {
    if (open) closeDropdown();
    else openDropdown();
  }

  function clearSelection() {
    setGlobalRepoPresetSelection(undefined, undefined);
    onchange(undefined);
  }

  function openSaveDialog(): void {
    if (selectedValues.length === 0 || mutationBusy) return;
    mutationError = undefined;
    saveDialogOpen = true;
  }

  function persistRepoPresetMutation(
    mutate: (
      workflow: (typeof SettingsWorkflow)["Service"],
    ) => Effect.Effect<SettingsSnapshot, SettingsError>,
    onSuccess: (saved: RepoPreset[]) => void,
  ): void {
    mutationExecution?.interrupt();
    mutationBusy = true;
    mutationError = undefined;
    mutationExecution = runtime.runCommand(
      Effect.gen(function* () {
        const workflow = yield* SettingsWorkflow;
        return yield* mutate(workflow);
      }).pipe(
        Effect.match({
          onFailure: (failure) => {
            mutationBusy = false;
            const message = settingsErrorMessage(failure);
            mutationError = message;
            if (deletePreset) showFlash(message, { tone: "danger" });
          },
          onSuccess: (settings) => {
            mutationBusy = false;
            const saved = settings.repo_presets ?? [];
            stores?.settings?.setRepoPresets?.(saved);
            onSuccess(saved);
          },
        }),
      ),
      {
        operation: "save repository presets",
        safeContext: { presetCount: repoPresets.length },
        onFailure: () => {},
      },
    );
  }

  function savePreset(target: RepoPresetSaveTarget): void {
    const repos = selectedPresetRepos;
    if (!repos) return;
    persistRepoPresetMutation(
      (workflow) => target.kind === "overwrite"
        ? workflow.updateRepoPreset(target.name, repos)
        : workflow.createRepoPreset({ name: target.name, repos }),
      () => {
      setGlobalRepoPresetSelection(target.name, selected);
      saveDialogOpen = false;
      mutationError = undefined;
      },
    );
  }

  function confirmDeletePreset(): void {
    const preset = deletePreset;
    if (!preset) return;
    persistRepoPresetMutation((workflow) => workflow.deleteRepoPreset(preset.name), () => {
      clearGlobalRepoPresetAffinity(preset.name);
      deletePreset = undefined;
      mutationError = undefined;
    });
  }

  function handleKeydown(e: KeyboardEvent) {
    const fixedRowCount = repoPresets.length + 1;
    const total = rows.length + fixedRowCount;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      highlightIndex = Math.min(highlightIndex + 1, total - 1);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      highlightIndex = Math.max(highlightIndex - 1, 0);
    } else if (e.key === "ArrowRight") {
      e.preventDefault();
      const row = rows[highlightIndex - fixedRowCount];
      if (row?.hasChildren && !row.expanded) expansion.toggle(row.node.id);
    } else if (e.key === "ArrowLeft") {
      e.preventDefault();
      const idx = highlightIndex - fixedRowCount;
      const row = rows[idx];
      if (row?.hasChildren && row.expanded) {
        expansion.toggle(row.node.id);
      } else if (row) {
        // On a leaf (or an already-collapsed group), move focus to the parent:
        // the nearest preceding visible row at a shallower depth.
        for (let i = idx - 1; i >= 0; i -= 1) {
          const candidate = rows[i];
          if (candidate && candidate.depth < row.depth) {
            highlightIndex = i + fixedRowCount;
            break;
          }
        }
      }
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (highlightIndex === 0) {
        clearSelection();
        return;
      }
      const preset = repoPresets[highlightIndex - 1];
      if (preset) {
        if (projectRepoPresetSelection(preset, options) !== undefined) selectPreset(preset);
        return;
      }
      const row = rows[highlightIndex - fixedRowCount];
      if (!row) return;
      if (row.hasChildren) expansion.toggle(row.node.id);
      else toggleRowSelect(row);
    } else if (e.key === " ") {
      e.preventDefault();
      if (highlightIndex === 0) {
        clearSelection();
        return;
      }
      const preset = repoPresets[highlightIndex - 1];
      if (preset) {
        if (projectRepoPresetSelection(preset, options) !== undefined) selectPreset(preset);
        return;
      }
      const row = rows[highlightIndex - fixedRowCount];
      if (row) toggleRowSelect(row);
    } else if (e.key === "Escape") {
      closeDropdown();
    }
  }

  function handleInput() {
    highlightIndex = 0;
  }

  function highlightSegments(
    text: string, q: string,
  ): { text: string; match: boolean }[] {
    if (!q) return [{ text, match: false }];
    const idx = text.toLowerCase().indexOf(q.toLowerCase());
    if (idx === -1) return [{ text, match: false }];
    return [
      ...(idx > 0
        ? [{ text: text.slice(0, idx), match: false }]
        : []),
      { text: text.slice(idx, idx + q.length), match: true },
      ...(idx + q.length < text.length
        ? [{ text: text.slice(idx + q.length), match: false }]
        : []),
    ];
  }

  function handleBlur(e: FocusEvent) {
    if (saveDialogOpen || deletePreset) return;
    const related = e.relatedTarget as Node | null;
    if (containerEl && related && containerEl.contains(related)) {
      return;
    }
    closeDropdown();
  }

  function preventBlur(e: MouseEvent) {
    e.preventDefault();
  }
</script>

<div class="typeahead" class:typeahead--mobile={mobile} bind:this={containerEl}>
  {#if !open || mobile}
    <Button
      class="typeahead-trigger"
      size="sm"
      ariaLabel={`Select repository: ${displayLabel.full}`}
      ariaExpanded={open}
      onclick={mobile ? toggleDropdown : openDropdown}
    >
      {#if mobile}
        <span class="typeahead-mobile-icon">
          <FolderGit2Icon size="16" strokeWidth="2" aria-hidden="true" />
        </span>
        <span class="typeahead-mobile-copy">
          <span class="typeahead-value" title={displayLabel.full}>
            {#if displayLabel.name}
              <span class="typeahead-repo-owner">{displayLabel.owner}/</span><span class="typeahead-repo-name">{displayLabel.name}</span>
            {:else}
              <span class="typeahead-label-text">{displayLabel.primary}</span>
            {/if}
            {#if displayLabel.qualifier}
              <span class="typeahead-label-qualifier"> · {displayLabel.qualifier}</span>
            {/if}
          </span>
          <span class="typeahead-selection-summary">{selectionSummary}</span>
        </span>
        <ChevronRightIcon
          class="typeahead-chevron"
          size="16"
          strokeWidth="2"
          aria-hidden="true"
        />
      {:else}
        <span class="typeahead-value" title={displayLabel.full}>
          {#if displayLabel.name}
            <span class="typeahead-repo-owner">{displayLabel.owner}/</span><span class="typeahead-repo-name">{displayLabel.name}</span>
          {:else}
            <span class="typeahead-label-text">{displayLabel.primary}</span>
          {/if}
          {#if displayLabel.qualifier}
            <span class="typeahead-label-qualifier"> · {displayLabel.qualifier}</span>
          {/if}
        </span>
        <ChevronDownIcon
          class="typeahead-chevron"
          size="10"
          strokeWidth="2"
          aria-hidden="true"
        />
      {/if}
    </Button>
  {/if}
  {#if open}
    <div
      class="typeahead-menu-shell"
      class:typeahead-menu-shell--mobile={mobile}
      class:kit-popover-card={mobile}
    >
      <input
        bind:this={inputEl}
        class="typeahead-input"
        type="text"
        bind:value={query}
        oninput={handleInput}
        onkeydown={handleKeydown}
        onblur={handleBlur}
        placeholder="Filter repos..."
        aria-label="Filter repos"
        autocomplete="off"
      />
      <!-- kit-ui-check-ignore: checkable provider tree owns tri-state multi-selection, named presets, persistent expansion, and provider-host-qualified identity; kit Typeahead is single-select -->
      <div
        class="typeahead-popover"
        class:kit-popover-card={!mobile}
        role="presentation"
        onmousedown={preventBlur}
      >
      <!-- kit-ui-check-ignore: preset rows share keyboard highlighting and selection with the tri-state repository tree below -->
      <ul class="typeahead-presets" role="listbox" aria-label="Repository presets">
        <li
          class="typeahead-option preset-option"
          class:highlighted={highlightIndex === 0}
          class:selected={selectedValues.length === 0}
          role="option"
          aria-selected={selectedValues.length === 0}
          onmousedown={clearSelection}
          onmouseenter={() => (highlightIndex = 0)}
        >
          <TreeCheckbox
            value={selectedValues.length === 0 ? "checked" : "unchecked"}
            decorative
          />
          <span>Global</span>
        </li>
        {#each repoPresets as preset, i (preset.name)}
          {@const projected = projectRepoPresetSelection(preset, options)}
          <li class="preset-row">
            <div
              class="typeahead-option preset-option preset-select-option"
              class:highlighted={i + 1 === highlightIndex}
              class:selected={matchingPreset?.name === preset.name}
              class:unavailable={projected === undefined}
              role="option"
              tabindex="-1"
              aria-selected={matchingPreset?.name === preset.name}
              aria-disabled={projected === undefined}
              onmousedown={() => {
                if (projected !== undefined) selectPreset(preset);
              }}
              onmouseenter={() => (highlightIndex = i + 1)}
            >
              <TreeCheckbox
                value={matchingPreset?.name === preset.name ? "checked" : "unchecked"}
                decorative
              />
              <span class="preset-name">{preset.name}</span>
            </div>
            {#if allowPresetManagement}
              <button
                type="button"
                class="preset-delete"
                aria-label={`Delete preset ${preset.name}`}
                disabled={mutationBusy}
                onclick={() => {
                  mutationError = undefined;
                  deletePreset = preset;
                }}
              >
                <TrashIcon size="13" strokeWidth="2" aria-hidden="true" />
              </button>
            {/if}
          </li>
        {/each}
      </ul>

      <!-- kit-ui-check-ignore: checkable provider tree requires hierarchical tri-state rows rather than the kit's flat single-select options -->
      <ul class="typeahead-repo-list" role="listbox" aria-label="Repositories">
        {#each rows as row, i (row.node.id)}
          <RepoTreeNode
            kind={row.node.kind}
            label={row.displayLabel ?? row.node.label}
            ariaLabel={rowAriaLabel(row)}
            provider={row.node.kind === "host" ? row.node.provider : undefined}
            depth={row.depth}
            hasChildren={row.hasChildren}
            expanded={row.expanded}
            selectionState={nodeSelectionState(row.node, selectedSet)}
            highlighted={i + repoPresets.length + 1 === highlightIndex}
            segments={query !== "" && row.node.kind === "repo"
              ? highlightSegments(row.displayLabel ?? row.node.label, query)
              : undefined}
            onToggleExpand={() => toggleRowExpand(row)}
            onToggleSelect={() => toggleRowSelect(row)}
            onHover={() => (highlightIndex = i + repoPresets.length + 1)}
          />
        {:else}
          <li class="typeahead-empty">No matching repos</li>
        {/each}
      </ul>

      {#if allowPresetManagement}
        <div class="typeahead-footer">
          {#if mutationError && !saveDialogOpen && !deletePreset}
            <p class="typeahead-error" role="alert">{mutationError}</p>
          {/if}
          <Button
            class="save-preset-button"
            size="sm"
            disabled={selectedValues.length === 0 || selectedPresetRepos === undefined || mutationBusy}
            onclick={openSaveDialog}
          >
            Save preset
          </Button>
        </div>
      {/if}
      </div>
    </div>
  {/if}
</div>

{#if allowPresetManagement && saveDialogOpen}
  <RepoPresetSaveDialog
    open
    presets={repoPresets}
    defaultPreset={preferredPreset}
    busy={mutationBusy}
    error={mutationError}
    onClose={() => {
      if (mutationBusy) return;
      saveDialogOpen = false;
      mutationError = undefined;
    }}
    onSave={savePreset}
  />
{/if}

{#if allowPresetManagement}
  <ConfirmDialog
    open={deletePreset !== undefined}
    title="Delete repository preset?"
    message={`Delete the preset ‘${deletePreset?.name ?? ""}’?`}
    hint="The repositories remain selected as a custom filter."
    confirmLabel="Delete preset"
    pendingLabel="Deleting…"
    busy={mutationBusy}
    tone="danger"
    onCancel={() => {
      if (mutationBusy) return;
      deletePreset = undefined;
      mutationError = undefined;
    }}
    onConfirm={confirmDeletePreset}
  />
{/if}

<style>
  .typeahead {
    position: relative;
    min-width: 160px;
    max-width: 260px;
  }

  .typeahead-menu-shell {
    display: contents;
  }

  :global(.typeahead-trigger.kit-button) {
    height: 26px;
    width: 100%;
    justify-content: flex-start;
    gap: var(--space-2);
    padding: 0 var(--space-4);
    font-size: var(--font-size-xs);
  }

  .typeahead-value {
    display: flex;
    flex: 1;
    min-width: 0;
    overflow: hidden;
    white-space: nowrap;
  }

  .typeahead-repo-owner,
  .typeahead-label-text,
  .typeahead-label-qualifier {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .typeahead-repo-name {
    flex: 0 1 auto;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .typeahead-label-qualifier {
    flex: 0 2 auto;
    color: var(--text-muted);
  }

  .typeahead-repo-owner {
    flex: 0 10 auto;
  }

  :global(.typeahead-chevron) {
    flex-shrink: 0;
    opacity: 0.5;
  }

  .typeahead-input {
    height: 26px;
    width: 100%;
    padding: 0 8px;
    background: var(--bg-inset);
    border: 1px solid var(--accent-blue);
    border-radius: var(--radius-sm);
    font-size: var(--font-size-xs);
    color: var(--text-primary);
    outline: none;
    box-sizing: border-box;
  }

  .typeahead-input::placeholder {
    color: var(--text-muted);
  }

  .typeahead-popover {
    position: absolute;
    top: 100%;
    left: 0;
    right: auto;
    min-width: 100%;
    width: max(300px, max-content);
    max-width: min(520px, 90vw);
    margin-top: 2px;
    max-height: 50vh;
    z-index: 90;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    padding: 0;
  }

  .typeahead-presets,
  .typeahead-repo-list {
    list-style: none;
    margin: 0;
    padding: 2px;
  }

  .typeahead-presets {
    flex: 0 0 auto;
    border-bottom: 1px solid var(--border-subtle);
  }

  .typeahead-repo-list {
    flex: 1 1 auto;
    min-height: 0;
    overflow-y: auto;
  }

  /* RepoTreeNode renders rows as a child component, so the shared row, */
  /* checkbox, and match-highlight rules are scoped to descendants of */
  /* the RepoTypeahead-owned popover rather than this component */
  /* alone. The :global() escape keeps them off the rest of the app. */
  .typeahead-popover :global(.typeahead-option) {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 4px 8px;
    font-size: var(--font-size-xs);
    color: var(--text-secondary);
    cursor: pointer;
    border-radius: 3px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .typeahead-popover :global(.typeahead-option.highlighted) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .typeahead-popover :global(.typeahead-option.selected) {
    color: var(--accent-blue);
    font-weight: 600;
  }

  .typeahead-popover :global(.match) {
    background: color-mix(in srgb, var(--accent-blue) 40%, transparent);
    color: var(--accent-blue);
    font-weight: 600;
    border-radius: 1px;
  }

  .typeahead-empty {
    padding: 6px 8px;
    font-size: var(--font-size-xs);
    color: var(--text-muted);
    font-style: italic;
  }

  .preset-row {
    display: flex;
    align-items: stretch;
  }

  .preset-select-option {
    flex: 1 1 auto;
    min-width: 0;
  }

  .preset-option.unavailable {
    cursor: not-allowed;
    opacity: 0.55;
  }

  .preset-name {
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .preset-delete {
    flex: 0 0 28px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-muted);
    cursor: pointer;
  }

  .preset-delete:hover:not(:disabled),
  .preset-delete:focus-visible {
    background: var(--bg-surface-hover);
    color: var(--accent-red);
  }

  .preset-delete:disabled {
    cursor: not-allowed;
    opacity: var(--opacity-disabled);
  }

  .typeahead-footer {
    flex: 0 0 auto;
    display: grid;
    gap: var(--space-2);
    padding: var(--space-2);
    border-top: 1px solid var(--border-subtle);
    background: var(--bg-surface);
  }

  :global(.save-preset-button.kit-button) {
    width: 100%;
    justify-content: center;
  }

  .typeahead-error {
    margin: 0;
    color: var(--accent-red);
    font-size: var(--font-size-xs);
    line-height: 1.35;
    white-space: normal;
  }

  .typeahead--mobile {
    width: 100%;
    min-width: 0;
    max-width: none;
  }

  .typeahead--mobile :global(.typeahead-trigger.kit-button),
  .typeahead--mobile .typeahead-input {
    min-height: 44px;
    border-radius: var(--radius-md);
    font-size: var(--font-size-md);
  }

  .typeahead--mobile :global(.typeahead-trigger.kit-button) {
    height: auto;
    min-height: 48px;
    gap: var(--space-4);
    padding: var(--space-1) var(--space-2);
    border: 0;
    border-top: thin solid var(--border-muted);
    border-bottom: thin solid var(--border-muted);
    border-radius: 0;
    background: transparent;
  }

  .typeahead--mobile :global(.typeahead-trigger.kit-button[aria-expanded="true"]) {
    color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
  }

  .typeahead-mobile-icon {
    width: 28px;
    height: 28px;
    flex: 0 0 28px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 13%, transparent);
  }

  .typeahead-mobile-copy {
    min-width: 0;
    flex: 1;
    display: grid;
    gap: var(--space-1);
    text-align: left;
  }

  .typeahead-mobile-copy .typeahead-value {
    flex: none;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: 700;
    line-height: 1.15;
  }

  .typeahead-selection-summary {
    overflow: hidden;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    line-height: 1.15;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .typeahead--mobile .typeahead-menu-shell--mobile {
    position: absolute;
    top: 100%;
    left: 0;
    right: 0;
    z-index: 90;
    width: min(360px, calc(100vw - 26px));
    max-width: calc(100vw - 26px);
    max-height: min(60vh, 520px);
    margin-top: var(--space-1);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    overflow: hidden;
    padding: var(--space-2);
  }

  .typeahead--mobile .typeahead-menu-shell--mobile .typeahead-input {
    width: 100%;
    flex: 0 0 auto;
  }

  .typeahead--mobile .typeahead-popover {
    position: static;
    width: 100%;
    min-width: 0;
    max-width: none;
    max-height: none;
    margin: 0;
    border: 0;
    border-radius: 0;
    background: transparent;
    box-shadow: none;
    z-index: auto;
  }

  .typeahead--mobile .typeahead-popover :global(.typeahead-option) {
    min-height: 44px;
    box-sizing: border-box;
    padding-top: 8px;
    padding-bottom: 8px;
    font-size: var(--font-size-md);
  }

  .typeahead--mobile .typeahead-empty {
    min-height: 44px;
    display: flex;
    align-items: center;
    font-size: var(--font-size-md);
  }
</style>

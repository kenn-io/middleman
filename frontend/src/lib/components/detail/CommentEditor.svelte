<script lang="ts">
  import { Effect, Exit } from "effect";
  import { onMount } from "svelte";
  import Document from "@tiptap/extension-document";
  import HardBreak from "@tiptap/extension-hard-break";
  import Paragraph from "@tiptap/extension-paragraph";
  import Placeholder from "@tiptap/extension-placeholder";
  import Text from "@tiptap/extension-text";
  import {
    Extension,
    type Editor as CoreEditor,
    type Range,
  } from "@tiptap/core";
  import { PluginKey } from "prosemirror-state";
  import Suggestion, {
    exitSuggestion,
    type SuggestionKeyDownProps,
    type SuggestionProps,
  } from "@tiptap/suggestion";
  import { Editor, EditorContent } from "svelte-tiptap";

  import {
    canonicalProvider,
    providerHostRouteParams,
    providerRouteParams,
    providerUsesHostRoute,
  } from "../../api/provider-routes.js";
  import { executeGeneratedApiRequest } from "../../api/generated-api.js";
  import type {
    CommentAutocompleteReference as GeneratedCommentAutocompleteReference,
    CommentAutocompleteResponse as GeneratedCommentAutocompleteResponse,
    GetCommentAutocompleteParams as GeneratedGetCommentAutocompleteParams,
  } from "../../api/generated/models/index.js";
  import { retryIdempotentRead } from "../../api/retry-policy.js";
  import type { AppExecution } from "../../app/runtime.js";
  import { getAppRuntime } from "../../app/runtime-context.js";
  import { computeCommentEditorMenuPosition } from "./commentEditorMenuPosition";

  interface Props {
    owner: string;
    name: string;
    provider: string;
    platformHost?: string | undefined;
    repoPath: string;
    /** Item the comment targets; its participants lead @ mention suggestions. */
    itemType?: "pull" | "issue" | undefined;
    itemNumber?: number | undefined;
    value: string;
    disabled?: boolean;
    autofocus?: boolean;
    placeholder?: string;
    oninput: (value: string) => void;
    onsubmit: () => void;
  }

  interface SuggestionItem {
    id: string;
    label: string;
    detail: string;
    insertText: string;
  }

  type AutocompleteTrigger = "@" | "#" | "!";

  type CommentAutocompleteReference = GeneratedCommentAutocompleteReference;
  type CommentAutocompleteResponse = GeneratedCommentAutocompleteResponse;

  const mentionSuggestionKey = new PluginKey("comment-editor-user-suggestion");
  const referenceSuggestionKey = new PluginKey("comment-editor-reference-suggestion");
  const mergeRequestSuggestionKey = new PluginKey("comment-editor-merge-request-suggestion");
  const runtime = getAppRuntime();

  let {
    owner,
    name,
    provider,
    platformHost,
    repoPath,
    itemType = undefined,
    itemNumber = undefined,
    value,
    disabled = false,
    autofocus = false,
    placeholder = "Write a comment... (Cmd+Enter to submit)",
    oninput,
    onsubmit,
  }: Props = $props();

  let editor = $state<Editor | null>(null);
  let syncingFromProps = false;
  let suggestionExecution: AppExecution<SuggestionItem[], never> | null = null;
  const deferredEditorCommands = new Set<AppExecution<void, never>>();
  let pointerFocusPending = false;
  let isComposingInput = false;

  function runEditorMicrotask(operation: string, callback: () => void): void {
    const execution = runtime.runMicrotask(
      () => {
        try {
          callback();
        } finally {
          deferredEditorCommands.delete(execution);
        }
      },
      { operation, safeContext: { provider, owner, name } },
    );
    deferredEditorCommands.add(execution);
  }

  function escapeHTML(text: string): string {
    return text
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;")
      .replaceAll("'", "&#39;");
  }

  function htmlFromPlainText(text: string): string {
    if (text === "") return "<p></p>";
    return `<p>${escapeHTML(text).replaceAll("\n", "<br>")}</p>`;
  }

  function plainTextFromEditor(current: CoreEditor): string {
    return current.getText({ blockSeparator: "\n" }).replace(/\n$/, "");
  }

  function isGitLabRepo(): boolean {
    return canonicalProvider(provider) === "gitlab";
  }

  function referenceKindForTrigger(trigger: AutocompleteTrigger): string | undefined {
    if (!isGitLabRepo()) return undefined;
    if (trigger === "!") return "pull";
    if (trigger === "#") return "issue";
    return undefined;
  }

  function referencePrefixForTrigger(trigger: AutocompleteTrigger): "#" | "!" {
    return trigger === "!" ? "!" : "#";
  }

  function pullReferenceLabel(): "MR" | "PR" {
    return isGitLabRepo() ? "MR" : "PR";
  }

  function toSuggestionItems(
    trigger: AutocompleteTrigger,
    response: CommentAutocompleteResponse,
  ): SuggestionItem[] {
    if (trigger === "@") {
      return (response.users ?? []).map((login: string) => ({
        id: `user:${login}`,
        label: `@${login}`,
        detail: "User",
        insertText: `@${login}`,
      }));
    }

    const kindFilter = referenceKindForTrigger(trigger);
    const prefix = referencePrefixForTrigger(trigger);
    return (response.references ?? [])
      .filter((reference: CommentAutocompleteReference) =>
        kindFilter === undefined || reference.kind === kindFilter
      )
      .map((reference: CommentAutocompleteReference) => ({
        id: `${reference.kind}:${reference.number}`,
        label: `${prefix}${reference.number}`,
        detail: `${reference.kind === "pull" ? pullReferenceLabel() : "Issue"} · ${reference.title}`,
        insertText: `${prefix}${reference.number}`,
      }));
  }

  async function loadSuggestionItems(
    trigger: AutocompleteTrigger,
    query: string,
  ): Promise<SuggestionItem[]> {
    if (trigger === "!" && !isGitLabRepo()) return [];
    suggestionExecution?.interrupt();
    const ref = { provider, platformHost, owner, name, repoPath };
    const params: GeneratedGetCommentAutocompleteParams = {
      trigger,
      q: query,
      limit: 8,
      ...(trigger === "@" && itemType !== undefined && itemNumber !== undefined
        ? { item_type: itemType === "pull" ? "pr" : "issue", item_number: itemNumber }
        : {}),
    };
    const program = executeGeneratedApiRequest("GET comment autocomplete", (client, signal) =>
      providerUsesHostRoute(ref)
        ? client.RepositoriesService.getCommentAutocompleteOnHost(
            { ...providerHostRouteParams(ref) },
            params,
            { signal },
          )
        : client.RepositoriesService.getCommentAutocomplete(
            { ...providerRouteParams(ref) },
            params,
            { signal },
          ),
    ).pipe(
      retryIdempotentRead,
      Effect.map((response) => toSuggestionItems(trigger, response)),
      Effect.catch(() => Effect.succeed([])),
    );
    const execution = runtime.runCommand(program, {
      operation: "load comment autocomplete",
      safeContext: { provider: ref.provider, owner: ref.owner, name: ref.name, trigger, query },
      onFailure: () => {},
    });
    suggestionExecution = execution;
    // Tiptap's suggestion callback requires a Promise. The request itself
    // runs in the app runtime; this boundary only observes that owned fiber.
    const exit = await execution.exit;
    return Exit.isSuccess(exit) ? exit.value : [];
  }

  function insertSuggestionText(current: CoreEditor, range: Range, insertText: string): void {
    current
      .chain()
      .focus()
      .insertContentAt(range, `${insertText} `)
      .run();
  }

  function createSuggestionRenderer() {
    let menuElement: HTMLDivElement | null = null;
    let listElement: HTMLUListElement | null = null;
    let selectedIndex = 0;
    let currentProps: SuggestionProps<SuggestionItem, SuggestionItem> | null = null;
    let listenersAttached = false;

    const handleViewportChange = () => {
      repositionMenu();
    };

    function removeMenu(): void {
      if (listenersAttached) {
        window.removeEventListener("resize", handleViewportChange);
        window.removeEventListener("scroll", handleViewportChange, true);
        listenersAttached = false;
      }
      menuElement?.remove();
      menuElement = null;
      listElement = null;
      currentProps = null;
      selectedIndex = 0;
    }

    function ensureMenu(): void {
      if (menuElement && listElement) return;

      menuElement = document.createElement("div");
      menuElement.className = "comment-editor-menu";
      menuElement.setAttribute("role", "listbox");
      menuElement.setAttribute("aria-label", "Comment autocomplete suggestions");

      listElement = document.createElement("ul");
      listElement.className = "comment-editor-menu-list";
      menuElement.appendChild(listElement);
      document.body.appendChild(menuElement);

      if (!listenersAttached) {
        window.addEventListener("resize", handleViewportChange);
        window.addEventListener("scroll", handleViewportChange, true);
        listenersAttached = true;
      }
    }

    function repositionMenu(): void {
      if (!menuElement || !currentProps?.clientRect) return;
      const caretRect = currentProps.clientRect();
      if (!caretRect) return;

      const position = computeCommentEditorMenuPosition({
        caretRect: {
          left: caretRect.left,
          top: caretRect.top,
          bottom: caretRect.bottom,
          width: caretRect.width,
        },
        viewportWidth: window.innerWidth,
        viewportHeight: window.innerHeight,
        menuHeight: menuElement.offsetHeight || 180,
      });

      menuElement.style.left = `${position.left}px`;
      menuElement.style.top = `${position.top}px`;
      menuElement.style.width = `${position.width}px`;
      menuElement.style.maxWidth = `${position.maxWidth}px`;
    }

    function renderMenu(): void {
      const list = listElement;
      if (!list || !currentProps) return;
      list.replaceChildren();

      currentProps.items.forEach((item, index) => {
        const listItem = document.createElement("li");
        const button = document.createElement("button");
        button.type = "button";
        button.className = "comment-editor-option";
        if (index === selectedIndex) {
          button.classList.add("is-highlighted");
        }
        button.setAttribute("role", "option");
        button.setAttribute("aria-selected", index === selectedIndex ? "true" : "false");

        const label = document.createElement("span");
        label.className = "comment-editor-option-label";
        label.textContent = item.label;

        const detail = document.createElement("span");
        detail.className = "comment-editor-option-detail";
        detail.textContent = item.detail;

        button.append(label, detail);
        button.onmouseenter = () => {
          selectedIndex = index;
          renderMenu();
        };
        button.onpointerdown = (event) => {
          event.preventDefault();
          currentProps?.command(item);
        };

        listItem.appendChild(button);
        list.appendChild(listItem);
      });

      repositionMenu();
    }

    function updateMenu(props: SuggestionProps<SuggestionItem, SuggestionItem>): void {
      currentProps = props;
      if (props.items.length === 0 || !props.clientRect) {
        removeMenu();
        return;
      }

      ensureMenu();
      selectedIndex = Math.min(selectedIndex, props.items.length - 1);
      renderMenu();
    }

    return {
      onStart(props: SuggestionProps<SuggestionItem, SuggestionItem>) {
        selectedIndex = 0;
        updateMenu(props);
      },
      onUpdate(props: SuggestionProps<SuggestionItem, SuggestionItem>) {
        updateMenu(props);
      },
      onKeyDown(props: SuggestionKeyDownProps) {
        if (!currentProps || currentProps.items.length === 0) return false;
        if (props.event.isComposing || props.event.keyCode === 229) {
          return false;
        }

        if (props.event.key === "ArrowDown") {
          selectedIndex = (selectedIndex + 1) % currentProps.items.length;
          renderMenu();
          return true;
        }

        if (props.event.key === "ArrowUp") {
          selectedIndex = (selectedIndex - 1 + currentProps.items.length) % currentProps.items.length;
          renderMenu();
          return true;
        }

        if (props.event.key === "Enter" || props.event.key === "Tab") {
          const item = currentProps.items[selectedIndex];
          if (!item) return false;
          currentProps.command(item);
          return true;
        }

        if (props.event.key === "Escape") {
          exitSuggestion(props.view);
          removeMenu();
          return true;
        }

        return false;
      },
      onExit() {
        removeMenu();
      },
    };
  }

  function removeSuggestionMenus(): void {
    document
      .querySelectorAll(".comment-editor-menu")
      .forEach((element) => element.remove());
  }

  function handleCompositionStart(): void {
    isComposingInput = true;
    if (editor) {
      exitSuggestion(editor.view, mentionSuggestionKey);
      exitSuggestion(editor.view, referenceSuggestionKey);
      exitSuggestion(editor.view, mergeRequestSuggestionKey);
    }
    removeSuggestionMenus();
  }

  function handleCompositionEnd(): void {
    isComposingInput = false;
    runEditorMicrotask("resume comment suggestions after composition", () => {
      if (!editor || !editor.isFocused) return;
      editor.view.dispatch(editor.state.tr);
    });
  }

  function createAutocompleteExtension(trigger: AutocompleteTrigger, pluginKey: PluginKey) {
    return Extension.create({
      name: trigger === "@"
        ? "commentUserAutocomplete"
        : trigger === "!"
          ? "commentMergeRequestAutocomplete"
          : "commentReferenceAutocomplete",
      addProseMirrorPlugins() {
        return [
          Suggestion<SuggestionItem, SuggestionItem>({
            editor: this.editor,
            pluginKey,
            char: trigger,
            allowedPrefixes: [" ", "(", "["],
            allow: () => !isComposingInput && (trigger !== "!" || isGitLabRepo()),
            items: ({ query }) => loadSuggestionItems(trigger, query),
            command: ({ editor, range, props }) => {
              insertSuggestionText(editor, range, props.insertText);
            },
            render: createSuggestionRenderer,
          }),
        ];
      },
    });
  }

  function hasActiveSuggestion(): boolean {
    if (!editor) return false;
    const mentionState = mentionSuggestionKey.getState(editor.state) as { active?: boolean } | undefined;
    const referenceState = referenceSuggestionKey.getState(editor.state) as { active?: boolean } | undefined;
    const mergeRequestState = mergeRequestSuggestionKey.getState(editor.state) as { active?: boolean } | undefined;
    return mentionState?.active === true
      || referenceState?.active === true
      || mergeRequestState?.active === true;
  }

  function handleEditorKeydown(event: KeyboardEvent): boolean {
    event.stopPropagation();

    if (event.isComposing || event.keyCode === 229) {
      return false;
    }

    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      onsubmit();
      return true;
    }

    if (
      hasActiveSuggestion() &&
      ["ArrowDown", "ArrowUp", "Enter", "Tab", "Escape"].includes(event.key)
    ) {
      return false;
    }

    if (event.key === "Enter") {
      event.preventDefault();
      editor?.commands.setHardBreak();
      return true;
    }

    return false;
  }

  onMount(() => {
    const current = new Editor({
      extensions: [
        Document,
        Paragraph,
        Text,
        HardBreak,
        createAutocompleteExtension("@", mentionSuggestionKey),
        createAutocompleteExtension("#", referenceSuggestionKey),
        createAutocompleteExtension("!", mergeRequestSuggestionKey),
        Placeholder.configure({
          placeholder,
          emptyEditorClass: "is-editor-empty",
        }),
      ],
      content: htmlFromPlainText(value),
      editable: !disabled,
      editorProps: {
        attributes: {
          class: "comment-editor-input",
          role: "textbox",
          "aria-label": placeholder,
          "aria-multiline": "true",
          "data-placeholder": placeholder,
          spellcheck: "true",
        },
        handleDOMEvents: {
          mousedown: () => {
            pointerFocusPending = true;
            return false;
          },
          focus: () => {
            runEditorMicrotask("settle comment editor focus", () => {
              if (!editor || !editor.isFocused) return;
              if (!pointerFocusPending) {
                editor.commands.focus("end", { scrollIntoView: false });
              }
              pointerFocusPending = false;
              editor.view.dispatch(editor.state.tr);
            });
            return false;
          },
        },
        handleKeyDown: (_view, event) => handleEditorKeydown(event),
      },
      onUpdate: ({ editor: updated }) => {
        const nextValue = plainTextFromEditor(updated);
        if (syncingFromProps) {
          syncingFromProps = false;
          return;
        }
        if (nextValue === value) return;
        if (!updated.isFocused) return;
        oninput(nextValue);
      },
    });

    editor = current;

    if (autofocus) {
      runEditorMicrotask("autofocus comment editor", () => {
        current.commands.focus("end", { scrollIntoView: false });
      });
    }

    current.view.dom.addEventListener("compositionstart", handleCompositionStart);
    current.view.dom.addEventListener("compositionend", handleCompositionEnd);

    return () => {
      suggestionExecution?.interrupt();
      for (const execution of deferredEditorCommands) execution.interrupt();
      deferredEditorCommands.clear();
      current.view.dom.removeEventListener("compositionstart", handleCompositionStart);
      current.view.dom.removeEventListener("compositionend", handleCompositionEnd);
      current.destroy();
      editor = null;
    };
  });

  $effect(() => {
    if (!editor) return;
    if (editor.isEditable !== !disabled) {
      editor.setEditable(!disabled);
    }
  });

  $effect(() => {
    if (!editor) return;
    const currentValue = plainTextFromEditor(editor);
    if (currentValue === value) return;
    syncingFromProps = true;
    editor.commands.setContent(htmlFromPlainText(value));
  });
</script>

<div class="comment-editor">
  {#if editor}
    <EditorContent {editor} />
  {/if}
</div>

<style>
  .comment-editor {
    position: relative;
  }

  :global(.comment-editor-input) {
    width: 100%;
    min-height: 80px;
    max-height: 200px;
    overflow-y: auto;
    resize: vertical;
    font-size: var(--font-size-root);
    line-height: 1.5;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    padding: 8px 10px;
    color: var(--text-primary);
    outline: none;
    white-space: pre-wrap;
    word-break: break-word;
  }

  :global(.comment-editor-input:focus) {
    border-color: var(--accent-blue);
  }

  :global(.comment-editor-input p) {
    margin: 0;
  }

  :global(.comment-editor-input p.is-empty:first-child::before) {
    content: attr(data-placeholder);
    color: var(--text-muted);
    pointer-events: none;
    float: left;
    height: 0;
  }

  :global(.comment-editor-menu) {
    position: fixed;
    max-height: 220px;
    overflow-y: auto;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    box-shadow: var(--shadow-md);
    z-index: 20;
  }

  :global(.comment-editor-menu-list) {
    list-style: none;
    margin: 0;
    padding: 4px;
  }

  :global(.comment-editor-option) {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 6px 8px;
    border-radius: 4px;
    cursor: pointer;
    font-size: var(--font-size-sm);
    width: 100%;
    border: 0;
    background: transparent;
    text-align: left;
  }

  :global(.comment-editor-option.is-highlighted) {
    background: var(--bg-surface-hover);
  }

  :global(.comment-editor-option-label) {
    color: var(--text-primary);
    font-weight: 600;
  }

  :global(.comment-editor-option-detail) {
    color: var(--text-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>

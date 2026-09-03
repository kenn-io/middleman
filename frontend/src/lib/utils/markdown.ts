// This pipeline stays on marked+DOMPurify directly rather than kit-ui's
// createMarkdownRenderer: interactive task lists need marked renderer
// overrides (checkbox/listitem/blockquote with per-render index state) and
// the non-data draggable attribute, neither expressible through kit's
// extensions/codeFence/data-* hook surface. The fence primitives that DO
// fit (escapeHtml, codeFenceLanguage, codeHighlightPlan and its budgets,
// shikiStyleIsAllowed) are imported from kit-ui below, so highlighting
// budgets and the shiki style allowlist stay in parity by construction.
// kit-ui-check-ignore: task-list renderer overrides exceed kit markdown hooks
import { Marked } from "marked";
import { Effect, Schema } from "effect";
// kit-ui-check-ignore: task-list renderer overrides exceed kit markdown hooks
import type { RendererObject, TokenizerAndRendererExtension, Tokens } from "marked";
// kit-ui-check-ignore: sanitizer must run app-side around the custom renderer
import DOMPurify from "dompurify";
// kit-ui-check-ignore: sanitizer must run app-side around the custom renderer
import type { UponSanitizeAttributeHook } from "dompurify";
import { codeFenceLanguage, codeHighlightPlan, escapeHtml, shikiStyleIsAllowed } from "@kenn-io/kit-ui/utils/markdown";
import { mermaidCodeFence } from "@kenn-io/kit-ui/utils/markdown-mermaid";
import { getSingletonHighlighter, type BundledLanguage, type Highlighter } from "shiki";
import { canonicalProvider, providerRepoResourceURL } from "../api/provider-routes.js";
import { itemReferenceAnchorAttributes, parseProviderItemURL } from "./item-reference.js";
import type { ItemReferenceType } from "./item-reference.js";

export interface RepoContext {
  provider: string;
  platformHost?: string | undefined;
  owner: string;
  name: string;
  repoPath: string;
}

export class MarkdownRenderError extends Schema.TaggedError<MarkdownRenderError>()("MarkdownRenderError", {
  cause: Schema.Defect(),
}) {}

type ItemRefToken = Tokens.Generic & {
  type: "itemRef";
  raw: string;
  provider: string;
  platformHost?: string | undefined;
  owner: string;
  name: string;
  repoPath: string;
  number: number;
  itemType?: ItemReferenceType | undefined;
  text: string;
};

function assertItemRefToken(token: Tokens.Generic): asserts token is ItemRefToken {
  if (
    token.type !== "itemRef" ||
    typeof token.raw !== "string" ||
    typeof token.provider !== "string" ||
    (token.platformHost !== undefined && typeof token.platformHost !== "string") ||
    typeof token.owner !== "string" ||
    typeof token.name !== "string" ||
    typeof token.repoPath !== "string" ||
    typeof token.number !== "number" ||
    (token.itemType !== undefined && token.itemType !== "pr" && token.itemType !== "issue") ||
    typeof token.text !== "string"
  ) {
    throw new Error("Unexpected itemRef token shape");
  }
}

function renderItemRefToken(token: Tokens.Generic): string {
  assertItemRefToken(token);
  return `<a ${itemReferenceAnchorAttributes(token)}>${token.text}</a>`;
}

export function providerItemRefExtension(repo?: RepoContext): TokenizerAndRendererExtension {
  const supportsBangMR = canonicalProvider(repo?.provider ?? "") === "gitlab";
  return {
    name: "itemRef",
    level: "inline",
    start(src: string): number | undefined {
      const marker = supportsBangMR ? "[#!]" : "#";
      const crossIdx = src.search(new RegExp(`[\\w.-]+/[\\w./-]+${marker}\\d`));
      // Bare: look for # preceded by start or non-word
      const bareIdx = src.search(/(^|[^\w])#\d/);
      const mrBareIdx = supportsBangMR ? src.search(/(^|[^\w])!\d/) : -1;
      const adjusted = bareIdx >= 0 && src[bareIdx] !== "#" ? bareIdx + 1 : bareIdx;
      const adjustedMR = mrBareIdx >= 0 && src[mrBareIdx] !== "!" ? mrBareIdx + 1 : mrBareIdx;
      return [crossIdx, adjusted, adjustedMR].filter((idx) => idx >= 0).sort((a, b) => a - b)[0];
    },
    tokenizer(
      this: { lexer?: { state?: { inLink?: boolean; inRawBlock?: boolean } } },
      src: string,
    ): ItemRefToken | undefined {
      const state = this.lexer?.state;
      if (state?.inLink || state?.inRawBlock || !repo) return undefined;

      const crossMatch = src.match(/^([\w.-]+(?:\/[\w.-]+)+)([#!])(\d+)(?!\w)/);
      if (crossMatch) {
        const repoPath = crossMatch[1]!;
        const marker = crossMatch[2]!;
        if (marker === "!" && !supportsBangMR) return undefined;
        const parts = repoPath.split("/");
        const name = parts.pop()!;
        const owner = parts.join("/");
        return {
          type: "itemRef",
          raw: crossMatch[0],
          provider: repo.provider,
          platformHost: repo.platformHost,
          owner,
          name,
          repoPath,
          number: parseInt(crossMatch[3]!, 10),
          itemType: marker === "!" ? "pr" : supportsBangMR ? "issue" : undefined,
          text: crossMatch[0],
        };
      }

      if (supportsBangMR) {
        const mrBareMatch = src.match(/^!(\d+)(?!\w)/);
        if (mrBareMatch) {
          return {
            type: "itemRef",
            raw: mrBareMatch[0],
            provider: repo.provider,
            platformHost: repo.platformHost,
            owner: repo.owner,
            name: repo.name,
            repoPath: repo.repoPath,
            number: parseInt(mrBareMatch[1]!, 10),
            itemType: "pr",
            text: mrBareMatch[0],
          };
        }
      }

      const bareMatch = src.match(/^#(\d+)(?!\w)/);
      if (bareMatch) {
        return {
          type: "itemRef",
          raw: bareMatch[0],
          provider: repo.provider,
          platformHost: repo.platformHost,
          owner: repo.owner,
          name: repo.name,
          repoPath: repo.repoPath,
          number: parseInt(bareMatch[1]!, 10),
          itemType: supportsBangMR ? "issue" : undefined,
          text: bareMatch[0],
        };
      }
      return undefined;
    },
    renderer(token): string {
      return renderItemRefToken(token);
    },
  };
}

export interface RenderMarkdownOpts {
  // When true, GFM task-list checkboxes render as enabled <input> elements
  // tagged with data-task-index="N" (zero-based, in document order). The
  // caller is responsible for intercepting clicks and persisting state —
  // unhandled clicks toggle visually but do not save.
  interactiveTasks?: boolean;
  // When true, a single newline inside a paragraph is a CommonMark soft
  // break that joins the surrounding text; only a blank line separates
  // paragraphs. When false (the default), newlines render as hard <br>
  // breaks the way GitHub renders comments.
  collapseSingleLineBreaks?: boolean;
}

// Per-render state for the custom checkbox renderer. Marked is single-
// threaded synchronous, so a module-level variable is safe.
//
// `itemStack` is a stack of pending listitem invocation scopes. When a
// listitem fires, it pushes a fresh frame; the checkbox renderer (for
// THIS item's `[ ]`) writes its allocated index to the top frame; the
// listitem reads the same frame back on its way out and pops. Nested
// task children push their own frames on top, so a parent's frame is
// preserved while inner items emit their own checkboxes.
type ListItemFrame = { checkboxIndex: number };
let renderState: {
  taskIndex: number;
  interactiveTasks: boolean;
  highlightCode: boolean;
  highlightedCodeTokens?: WeakSet<Tokens.Code> | undefined;
  shikiNonce: string;
  itemStack: ListItemFrame[];
  // Counts blockquote nesting depth so listitem can detect when it
  // sits inside `> ...`. The source-side task helpers don't see
  // blockquoted task lines (TASK_LINE matches column-0 bullets),
  // so the renderer must skip interactivity inside blockquotes —
  // otherwise data-task-index values would drift from the source
  // and clicks would mutate the wrong line.
  blockquoteDepth: number;
  repo?: RepoContext | undefined;
} = {
  taskIndex: 0,
  interactiveTasks: false,
  highlightCode: true,
  highlightedCodeTokens: undefined,
  shikiNonce: "",
  itemStack: [],
  blockquoteDepth: 0,
  repo: undefined,
};

const htmlCache = new Map<string, Promise<string>>();
const markedCache = new Map<string, Marked>();
const MARKDOWN_ALLOWED_ATTRS = [
  "style",
  "target",
  "data-provider",
  "data-platform-host",
  "data-owner",
  "data-name",
  "data-repo-path",
  "data-number",
  "data-item-type",
  "data-external-url",
  "data-task-index",
  "data-kenn-forge-shiki",
  "draggable",
];

// Six-dot drag handle SVG used to grab a task-list item. Inlined so
// the rendered markdown is self-contained and no extra fetch is needed.
const DRAG_HANDLE_SVG =
  `<svg viewBox="0 0 12 16" width="12" height="16" aria-hidden="true">` +
  `<circle cx="3" cy="3" r="1.2"/>` +
  `<circle cx="9" cy="3" r="1.2"/>` +
  `<circle cx="3" cy="8" r="1.2"/>` +
  `<circle cx="9" cy="8" r="1.2"/>` +
  `<circle cx="3" cy="13" r="1.2"/>` +
  `<circle cx="9" cy="13" r="1.2"/>` +
  `</svg>`;

const SHIKI_LIGHT_THEME = "github-light-default";
const SHIKI_DARK_THEME = "github-dark-default";
const SHIKI_THEMES = {
  light: SHIKI_LIGHT_THEME,
  dark: SHIKI_DARK_THEME,
} as const;
const SHIKI_PLAINTEXT_LANG = "text";
const SHIKI_GENERATED_ATTR = "data-kenn-forge-shiki";
let shikiHighlighter: Highlighter | undefined;
let shikiHighlighterPromise: Promise<Highlighter> | undefined;
let shikiNonceFallbackCounter = 0;

function getShikiHighlighter(): Promise<Highlighter> {
  shikiHighlighterPromise ??= getSingletonHighlighter({
    themes: [SHIKI_LIGHT_THEME, SHIKI_DARK_THEME],
    langs: [],
  }).then((highlighter) => {
    shikiHighlighter = highlighter;
    return highlighter;
  });
  return shikiHighlighterPromise;
}

function plainCodeBlock(text: string): string {
  return `<pre><code>${escapeHtml(text)}</code></pre>`;
}

function renderHighlightedCode(token: Tokens.Code): string {
  if (!renderState.highlightCode || !shikiHighlighter) return plainCodeBlock(token.text);
  if (!renderState.highlightedCodeTokens?.has(token)) return plainCodeBlock(token.text);
  const lang = codeFenceLanguage(token.lang);
  try {
    return markTrustedShikiHtml(
      shikiHighlighter.codeToHtml(token.text, { lang, themes: SHIKI_THEMES, defaultColor: false }),
    );
  } catch {
    return markTrustedShikiHtml(
      shikiHighlighter.codeToHtml(token.text, {
        lang: SHIKI_PLAINTEXT_LANG,
        themes: SHIKI_THEMES,
        defaultColor: false,
      }),
    );
  }
}

function markTrustedShikiHtml(html: string): string {
  const marker = `${SHIKI_GENERATED_ATTR}="${renderState.shikiNonce}"`;
  return html.replace("<pre ", `<pre ${marker} `).replaceAll("<span ", `<span ${marker} `);
}

const taskListRenderer: RendererObject = {
  // A pasted provider URL for a pull request or issue on the rendering
  // repository's host becomes an in-app item reference. Marked routes
  // bare autolinked URLs and [text](url) links through this same hook, so
  // both forms stay inside kenn-forge instead of opening the provider site.
  link(token) {
    const repo = renderState.repo;
    const ref = repo ? parseProviderItemURL(token.href, repo) : null;
    if (!ref) return false;
    const text = this.parser.parseInline(token.tokens);
    return `<a ${itemReferenceAnchorAttributes(ref)}>${text}</a>`;
  },
  blockquote(token): string {
    renderState.blockquoteDepth++;
    const inner = this.parser.parse(token.tokens);
    renderState.blockquoteDepth--;
    return `<blockquote>\n${inner}</blockquote>\n`;
  },
  code(token: Tokens.Code): string | false {
    return mermaidCodeFence(token.text, codeFenceLanguage(token.lang)) ?? renderHighlightedCode(token);
  },
  // The checkbox renderer is called during the recursive parse
  // of a listitem's inner tokens. It allocates the next task
  // index and writes it onto the top frame of itemStack so the
  // enclosing listitem can pick up THIS item's index — even if
  // nested children push and pop frames of their own first.
  // Inside a blockquote, the source-side helpers can't see the
  // task line (TASK_LINE doesn't match `> -` prefixes), so
  // emit the default disabled checkbox to keep indices aligned.
  checkbox({ checked }): string {
    const inBlockquote = renderState.blockquoteDepth > 0;
    const interactive = renderState.interactiveTasks && !inBlockquote;
    const checkedAttr = checked ? ' checked=""' : "";
    if (interactive) {
      const index = renderState.taskIndex++;
      const stack = renderState.itemStack;
      if (stack.length > 0) {
        stack[stack.length - 1]!.checkboxIndex = index;
      }
      // kit-ui-check-ignore: generated sanitized HTML uses delegated task indices and cannot instantiate a Svelte Checkbox
      return `<input${checkedAttr} type="checkbox" data-task-index="${index}">`;
    }
    // kit-ui-check-ignore: generated sanitized HTML uses delegated task indices and cannot instantiate a Svelte Checkbox
    return `<input${checkedAttr} disabled="" type="checkbox">`;
  },
  listitem(token): string {
    const frame: ListItemFrame = { checkboxIndex: -1 };
    renderState.itemStack.push(frame);
    const inner = this.parser.parse(token.tokens);
    renderState.itemStack.pop();
    if (!token.task) return `<li>${inner}</li>\n`;
    const interactive = renderState.interactiveTasks && renderState.blockquoteDepth === 0;
    if (!interactive) {
      return `<li class="task-list-item">${inner}</li>\n`;
    }
    const index = frame.checkboxIndex;
    const handle =
      `<span class="task-drag-handle" ` +
      `data-task-index="${index}" ` +
      `draggable="true" ` +
      `role="button" ` +
      `tabindex="-1" ` +
      `aria-label="Drag to reorder">` +
      DRAG_HANDLE_SVG +
      `</span>`;
    return (
      `<li class="task-list-item task-list-item--interactive" ` +
      `data-task-index="${index}">` +
      `${handle}${inner}</li>\n`
    );
  },
};

function getMarked(repo?: RepoContext, opts: RenderMarkdownOpts = {}): Marked {
  const collapse = !!opts.collapseSingleLineBreaks;
  const key = `${collapse ? 1 : 0}\0${repo ? `${repo.provider}/${repo.platformHost ?? ""}/${repo.repoPath}` : ""}`;
  let instance = markedCache.get(key);
  if (!instance) {
    instance = new Marked({ breaks: !collapse, gfm: true });
    instance.use({ extensions: [providerItemRefExtension(repo)] });
    instance.use({
      renderer: taskListRenderer,
    });
    markedCache.set(key, instance);
  }
  return instance;
}

export interface RenderedMarkdownBlock {
  key: string;
  startLine: number;
  endLine: number;
  html: string;
}

function resetRenderState(
  opts: RenderMarkdownOpts,
  repo?: RepoContext,
  highlightCode = true,
  highlightedCodeTokens?: WeakSet<Tokens.Code>,
): void {
  renderState = {
    taskIndex: 0,
    interactiveTasks: !!opts.interactiveTasks,
    highlightCode,
    highlightedCodeTokens,
    shikiNonce: shikiNonce(),
    itemStack: [],
    blockquoteDepth: 0,
    repo,
  };
}

function sanitizeMarkdownHtml(html: string): string {
  DOMPurify.addHook("uponSanitizeAttribute", shikiStyleSanitizer);
  DOMPurify.addHook("uponSanitizeAttribute", markdownImageSourceSanitizer);
  try {
    const sanitized = DOMPurify.sanitize(html, {
      ADD_ATTR: MARKDOWN_ALLOWED_ATTRS,
    });
    return sanitized.replaceAll(new RegExp(`\\s${SHIKI_GENERATED_ATTR}="[^"]*"`, "g"), "");
  } finally {
    DOMPurify.removeHook("uponSanitizeAttribute", shikiStyleSanitizer);
    DOMPurify.removeHook("uponSanitizeAttribute", markdownImageSourceSanitizer);
  }
}

const markdownImageSourceSanitizer: UponSanitizeAttributeHook = (node, data) => {
  if (data.attrName !== "src" || node.tagName.toLowerCase() !== "img" || !renderState.repo) return;
  const source = proxiedMarkdownImageSource(data.attrValue, renderState.repo);
  if (source) data.attrValue = source;
};

function proxiedMarkdownImageSource(source: string, repo: RepoContext): string | null {
  const provider = canonicalProvider(repo.provider);
  if (provider !== "github" && provider !== "gitlab") return null;
  try {
    const host = repo.platformHost?.trim() || (provider === "github" ? "github.com" : "gitlab.com");
    const normalizedSource =
      provider === "gitlab" ? normalizedGitLabMarkdownImageSource(source, host, repo.repoPath) : source;
    if (!normalizedSource) return null;
    const url = new URL(normalizedSource);
    if (url.protocol !== "https:") return null;
    if (provider === "github" && !isProxiedGitHubImage(url, host, repo.repoPath)) return null;
    if (
      provider === "gitlab" &&
      (url.host.toLowerCase() !== host.toLowerCase() ||
        (!url.pathname.startsWith(`/${repo.repoPath}/uploads/`) && !/^\/-\/project\/\d+\/uploads\//.test(url.pathname)))
    )
      return null;
    return providerRepoResourceURL(repo, "/markdown-image", { source: url.toString() });
  } catch {
    return null;
  }
}

// GitHub images that need the repository credential: private user-attachments
// uploads, and files committed to this repository referenced through blob or
// raw web URLs. Other repositories' files stay on direct loading because the
// route's credential is only known to reach this repository. The server
// applies the same rules (internal/github/markdown_images.go).
function isProxiedGitHubImage(url: URL, host: string, repoPath: string): boolean {
  const urlHost = url.host.toLowerCase();
  const platformHost = host.toLowerCase();
  const segments = url.pathname.split("/").filter(Boolean);
  const inRepository = segments.slice(0, 2).join("/").toLowerCase() === repoPath.toLowerCase();
  if (urlHost === platformHost) {
    if (url.pathname.startsWith("/user-attachments/assets/")) return true;
    return inRepository && (segments[2] === "blob" || segments[2] === "raw") && segments.length >= 5;
  }
  if (urlHost === "raw.githubusercontent.com" && platformHost === "github.com") {
    return inRepository && segments.length >= 4;
  }
  return false;
}

function normalizedGitLabMarkdownImageSource(source: string, host: string, repoPath: string): string | null {
  if (/^https:\/\//i.test(source)) return source;
  const uploadPath = source.replace(/^\//, "");
  if (uploadPath.startsWith("uploads/")) return `https://${host}/${repoPath}/${uploadPath}`;
  if (uploadPath.startsWith(`${repoPath}/uploads/`)) return `https://${host}/${uploadPath}`;
  return null;
}

function shikiNonce(): string {
  const crypto = globalThis.crypto;
  if (crypto?.randomUUID) return crypto.randomUUID();
  if (crypto?.getRandomValues) {
    const bytes = new Uint8Array(16);
    crypto.getRandomValues(bytes);
    return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  }
  return `${Date.now()}-${shikiNonceFallbackCounter++}`;
}

const shikiStyleSanitizer: UponSanitizeAttributeHook = (node, data) => {
  if (data.attrName !== "style") return;
  const tagName = node.tagName.toLowerCase();
  const trustedShikiNode = node.getAttribute(SHIKI_GENERATED_ATTR) === renderState.shikiNonce;
  const isStyledShikiNode =
    trustedShikiNode &&
    ((tagName === "pre" && node.classList.contains("shiki")) || (tagName === "span" && node.closest("pre.shiki")));
  if (!isStyledShikiNode || !shikiStyleIsAllowed(data.attrValue)) {
    data.keepAttr = false;
  }
};

function visibleTokenLineCount(raw: string): number {
  if (!raw) return 0;
  const visibleRaw = raw.endsWith("\n") ? raw.slice(0, -1) : raw;
  if (!visibleRaw) return 0;
  return visibleRaw.split("\n").length;
}

function tokenLineBreakCount(raw: string): number {
  return raw.match(/\n/g)?.length ?? 0;
}

function tokenRendersVisibleBlock(token: Tokens.Generic): boolean {
  return token.type !== "space" && token.type !== "def";
}

function detailsDepthDelta(token: Tokens.Generic): number {
  if (token.type !== "html") return 0;
  let depth = 0;
  for (const match of token.raw.matchAll(/<\/?details\b[^>]*>/gi)) {
    depth += match[0].startsWith("</") ? -1 : 1;
  }
  return depth;
}

function opensDetailsBlock(token: Tokens.Generic): boolean {
  return detailsDepthDelta(token) > 0;
}

function tokenRaw(tokens: Tokens.Generic[]): string {
  return tokens.map((token) => token.raw).join("");
}

async function loadCodeFenceLanguage(lang: string, highlighter: Highlighter): Promise<void> {
  if (lang === SHIKI_PLAINTEXT_LANG) return;
  try {
    const resolvedLang = highlighter.resolveLangAlias(lang);
    if (highlighter.getLoadedLanguages().includes(resolvedLang)) return;
    await highlighter.loadLanguage(lang as BundledLanguage);
  } catch {
    // Unknown fence info strings render as escaped plain text.
  }
}

async function loadCodeFenceLanguages(languages: string[]): Promise<void> {
  if (languages.length === 0) return;
  const highlighter = shikiHighlighter ?? (await getShikiHighlighter());
  for (const lang of languages) {
    await loadCodeFenceLanguage(lang, highlighter);
  }
}

export function renderMarkdownBlocks(
  raw: string,
  repo?: RepoContext,
  opts: RenderMarkdownOpts = {},
): RenderedMarkdownBlock[] {
  if (!raw) return [];
  const marked = getMarked(repo);
  const tokens = marked.lexer(raw) as Tokens.Generic[];
  // Rich-preview block slicing is synchronous by design; keep code fences
  // plain here instead of making output depend on prior async Shiki loads.
  resetRenderState(opts, repo, false);
  const blocks: RenderedMarkdownBlock[] = [];
  let line = 1;
  for (let i = 0; i < tokens.length; i++) {
    const token = tokens[i]!;
    const startLine = line;
    if (opensDetailsBlock(token)) {
      const groupedTokens = [token];
      let depth = detailsDepthDelta(token);
      while (depth > 0 && i + 1 < tokens.length) {
        const next = tokens[++i]!;
        groupedTokens.push(next);
        depth += detailsDepthDelta(next);
      }
      const raw = tokenRaw(groupedTokens);
      const lineCount = visibleTokenLineCount(raw);
      if (lineCount > 0) {
        blocks.push({
          key: `${blocks.length}:details:${startLine}`,
          startLine,
          endLine: startLine + lineCount - 1,
          html: sanitizeMarkdownHtml(marked.parser(groupedTokens) as string),
        });
      }
      line += tokenLineBreakCount(raw);
      continue;
    }
    const lineCount = visibleTokenLineCount(token.raw);
    if (tokenRendersVisibleBlock(token) && lineCount > 0) {
      blocks.push({
        key: `${blocks.length}:${token.type}:${startLine}`,
        startLine,
        endLine: startLine + lineCount - 1,
        html: sanitizeMarkdownHtml(marked.parser([token]) as string),
      });
    }
    line += tokenLineBreakCount(token.raw);
  }
  return blocks;
}

export function extractMarkdownDefinitionLines(raw: string, repo?: RepoContext): string[] {
  if (!raw) return [];
  const marked = getMarked(repo);
  const tokens = marked.lexer(raw) as Tokens.Generic[];
  const lines: string[] = [];
  for (const token of tokens) {
    if (token.type !== "def" || !token.raw) continue;
    const raw = token.raw.endsWith("\n") ? token.raw.slice(0, -1) : token.raw;
    if (raw) lines.push(...raw.split("\n"));
  }
  return lines;
}

export function renderMarkdown(raw: string, repo?: RepoContext, opts: RenderMarkdownOpts = {}): Promise<string> {
  if (!raw) return Promise.resolve("");
  const interactiveTasks = !!opts.interactiveTasks;
  const collapse = !!opts.collapseSingleLineBreaks;
  const repoKey = repo ? `${repo.provider}/${repo.platformHost ?? ""}/${repo.repoPath}` : "";
  const key = `${repoKey}\0${interactiveTasks ? 1 : 0}\0${collapse ? 1 : 0}\0${raw}`;
  const cached = htmlCache.get(key);
  if (cached !== undefined) return cached;

  const html = renderMarkdownUncached(raw, repo, opts);
  if (htmlCache.size > 500) htmlCache.clear();
  htmlCache.set(key, html);
  html.catch(() => {
    htmlCache.delete(key);
  });
  return html;
}

export const renderMarkdownEffect = Effect.fn("Markdown.render")(function* (
  raw: string,
  repo?: RepoContext,
  opts: RenderMarkdownOpts = {},
) {
  return yield* Effect.tryPromise({
    try: () => renderMarkdown(raw, repo, opts),
    catch: (cause) => MarkdownRenderError.make({ cause }),
  });
});

async function renderMarkdownUncached(
  raw: string,
  repo: RepoContext | undefined,
  opts: RenderMarkdownOpts,
): Promise<string> {
  const marked = getMarked(repo, opts);
  const tokens = marked.lexer(raw) as Tokens.Generic[];
  // Mermaid fences render as diagram markup, so they never spend the
  // shared highlight budget.
  const highlightPlan = codeHighlightPlan(marked, tokens, (_code, lang) => lang === "mermaid");
  await loadCodeFenceLanguages(highlightPlan.languages);
  return renderMarkdownTokens(marked, tokens, opts, repo, true, highlightPlan.tokens);
}

export function renderMarkdownSync(raw: string, repo?: RepoContext, opts: RenderMarkdownOpts = {}): string {
  if (!raw) return "";
  const marked = getMarked(repo, opts);
  const tokens = marked.lexer(raw) as Tokens.Generic[];
  return renderMarkdownTokens(marked, tokens, opts, repo, false);
}

function renderMarkdownTokens(
  marked: Marked,
  tokens: Tokens.Generic[],
  opts: RenderMarkdownOpts,
  repo?: RepoContext,
  highlightCode = true,
  highlightedCodeTokens?: WeakSet<Tokens.Code>,
): string {
  resetRenderState(opts, repo, highlightCode, highlightedCodeTokens);
  return sanitizeMarkdownHtml(marked.parser(tokens) as string);
}

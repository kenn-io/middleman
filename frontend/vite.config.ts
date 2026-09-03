import { realpathSync } from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import { svelteTesting } from "@testing-library/svelte/vite";
import { defaultClientConditions, searchForWorkspaceRoot, type Plugin, type ProxyOptions, type UserConfig } from "vite";
import { defineProject, type TestProjectInlineConfiguration } from "vite-plus/test/config";
import type { InlineConfig } from "vite-plus/test/node";
import { resolveDevApiUrl } from "./src/lib/dev/apiProxyTarget.ts";
import { healthcheckPlugin } from "./src/lib/dev/healthcheckPlugin.ts";
import { nodeUnitTestFiles } from "./vitest.node-files.ts";

const require = createRequire(import.meta.url);
const testingLibrarySvelteEntry = require.resolve("@testing-library/svelte");
const kitUiMarkdownEntry = require.resolve("@kenn-io/kit-ui/utils/markdown");
const kitUiTimeEntry = require.resolve("@kenn-io/kit-ui/utils/time");
// kit-ui is consumed as source and Bun links it from its global cache, which
// sits outside the workspace root. Its asset imports (`?inline` SVGs) go
// through Vite's fs allowlist by realpath, so the resolved source root must
// be allowed explicitly.
const kitUiSourceRoot = path.resolve(realpathSync(path.dirname(kitUiMarkdownEntry)), "..");

// resolveDevApiUrl() prefers KENN_FORGE_API_URL, which dev-ephemeral sets
// to the generated backend URL before starting Vite.
const apiUrl = resolveDevApiUrl();
const devServerPort = resolveViteServerPort();
const devServerAllowedHosts = resolveViteAllowedHosts();
const devServerHmr = resolveViteHmr();
const workspaceRoot = searchForWorkspaceRoot(process.cwd());

function devApiUrlPlugin(url: string): Plugin {
  return {
    name: "kenn-forge-dev-api-url",
    apply: "serve",
    transformIndexHtml() {
      return [
        {
          tag: "script",
          children: `window.__KENN_FORGE_DEV_API_URL__ = ${JSON.stringify(url)};`,
          injectTo: "head-prepend",
        },
      ];
    },
  };
}

export function resolveViteServerPort(argv: readonly string[] = process.argv): number {
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (!arg) continue;
    if (arg === "--port" && i + 1 < argv.length) {
      const next = argv[i + 1];
      const parsed = parsePort(next);
      if (parsed !== null) return parsed;
    }
    if (arg.startsWith("--port=")) {
      const parsed = parsePort(arg.slice("--port=".length));
      if (parsed !== null) return parsed;
    }
  }
  return 5174;
}

function parsePort(value: string | undefined): number | null {
  if (!value) return null;
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed < 1 || parsed > 65535) {
    return null;
  }
  return parsed;
}

function parseHostList(value: string | undefined): string[] {
  return (value ?? "")
    .split(",")
    .map((host) => host.trim())
    .filter(Boolean);
}

export function resolveViteAllowedHosts(env: Record<string, string | undefined> = process.env): string[] | undefined {
  const hosts = parseHostList(env.KENN_FORGE_VITE_ALLOWED_HOSTS);
  return hosts.length > 0 ? hosts : undefined;
}

export function resolveViteHmr(env: Record<string, string | undefined> = process.env) {
  const configuredProtocol = env.KENN_FORGE_VITE_HMR_PROTOCOL;
  const protocol = configuredProtocol === "ws" || configuredProtocol === "wss" ? configuredProtocol : undefined;
  const host = env.KENN_FORGE_VITE_HMR_HOST?.trim() || undefined;
  const clientPort = parsePort(env.KENN_FORGE_VITE_HMR_CLIENT_PORT) ?? undefined;

  return {
    ...(protocol ? { protocol } : {}),
    ...(host ? { host } : {}),
    ...(clientPort ? { clientPort } : {}),
    path: "/__vite_hmr",
  };
}

function logWebSocketProxyRequests(): NonNullable<ProxyOptions["configure"]> {
  return (proxy) => {
    proxy.on("proxyReqWs", (_proxyReq, req, socket) => {
      const url = req.url ?? "<unknown>";
      console.info(`[vite:ws-proxy] open ${url}`);
      socket.on("error", (err) => {
        console.error(`[vite:ws-proxy] socket error ${url}: ${err.message}`);
      });
    });
    proxy.on("error", (err, req) => {
      const url = req?.url ?? "<unknown>";
      console.error(`[vite:ws-proxy] error ${url}: ${err.message}`);
    });
    proxy.on("close", (_proxyRes, _proxySocket, proxyHead) => {
      const headLength = proxyHead instanceof Buffer ? proxyHead.length : 0;
      console.info(`[vite:ws-proxy] close proxyHeadBytes=${headLength}`);
    });
  };
}

export function webSocketDebugEnabled(env: Record<string, string | undefined> = process.env): boolean {
  switch (env.KENN_FORGE_WS_DEBUG?.trim().toLowerCase()) {
    case "1":
    case "true":
    case "yes":
    case "on":
      return true;
    default:
      return false;
  }
}

export function resolveBrowserTestWorkers(env: Record<string, string | undefined> = process.env): number | undefined {
  return env.CI ? 2 : undefined;
}

export function resolveUnitTestWorkers(env: Record<string, string | undefined> = process.env): number | undefined {
  if (!env.CI) return undefined;
  const configured = Number.parseInt(env.KENN_FORGE_CI_WORKERS ?? "", 10);
  return configured > 0 ? configured : 14;
}

function terminalWebSocketProxy(url: string): ProxyOptions {
  const proxy: ProxyOptions = {
    target: url,
    changeOrigin: true,
    ws: true,
  };
  if (webSocketDebugEnabled()) {
    proxy.configure = logWebSocketProxyRequests();
  }
  return proxy;
}

// Pure logic suites avoid jsdom startup while browser-coupled unit suites keep
// jsdom plus the localStorage/elementFromPoint shims in setup.ts. The exact
// Node list was verified A/B; anything new defaults to jsdom until promoted.
const unitTestMaxWorkers = resolveUnitTestWorkers();
const commonUnitTest = {
  pool: "threads",
  execArgv: ["--no-experimental-webstorage"],
  ...(unitTestMaxWorkers ? { maxWorkers: unitTestMaxWorkers } : {}),
  setupFiles: ["./src/test/setup.ts"],
};
const commonUnitExclude = ["tests/e2e/**", "tests/e2e-full/**", "node_modules/**", "src/**/*.browser.svelte.ts"];
const nodeUnitTestProject = {
  extends: true,
  test: {
    ...commonUnitTest,
    name: "unit-node",
    environment: "node",
    include: [...nodeUnitTestFiles],
    exclude: commonUnitExclude,
  },
} satisfies TestProjectInlineConfiguration;
const jsdomUnitTestProject = {
  extends: true,
  test: {
    ...commonUnitTest,
    name: "unit-jsdom",
    environment: "jsdom",
    include: ["src/**/*.{test,spec}.?(c|m)[jt]s?(x)", "../packages/github-app-ui/src/**/*.{test,spec}.?(c|m)[jt]s?(x)"],
    exclude: [...commonUnitExclude, ...nodeUnitTestFiles],
  },
} satisfies TestProjectInlineConfiguration;

// The "browser" project runs *.browser.svelte.ts specs in a real headless
// chromium page via the Playwright provider. It intentionally omits setup.ts:
// a real page has native localStorage and elementFromPoint, so the jsdom shims
// would be wrong here.
//
// The Playwright provider is loaded with a dynamic import inside an async
// project factory rather than a top-level import on purpose: "vite-plus/test/
// browser-playwright" transitively pulls in the browser runtime (ws's
// WebSocketServer, the @vitest/browser client), which fails to evaluate under
// the jsdom unit runner. src/lib/dev/viteConfig.test.ts and
// healthcheckPlugin.test.ts import this config module directly, so a static
// browser import would crash those unit tests. The factory body only runs when
// the browser project itself is initialized, keeping the browser runtime out of
// the unit project's module graph.
//
// resolve.conditions forces the browser/client export conditions. vite-plugin-svelte
// only picks Svelte's "browser" export ("./src/index-client.js", which exposes
// mount()) when the environment is named/consumed as a client; under the browser
// test runtime it otherwise falls through to Svelte's server entry and mount()
// throws lifecycle_function_unavailable. Spreading Vite's defaultClientConditions
// keeps the standard "module"/"development|production" placeholders intact.
const browserTestProject = defineProject(async () => {
  const { playwright } = await import("vite-plus/test/browser-playwright");
  const maxWorkers = resolveBrowserTestWorkers();
  return {
    extends: true,
    resolve: {
      conditions: [...defaultClientConditions],
    },
    test: {
      name: "browser",
      include: ["src/**/*.browser.svelte.ts"],
      ...(maxWorkers ? { maxWorkers } : {}),
      browser: {
        enabled: true,
        provider: playwright() as never,
        instances: [{ browser: "chromium" }],
        headless: true,
      },
    },
  } satisfies TestProjectInlineConfiguration;
});

const config = {
  base: "/",
  // The Go server serves this build under a configurable base_path (default
  // "/", e.g. "/kenn-forge/" behind a reverse proxy) by rewriting index.html's
  // <script src>/<link href> at request time. That rewrite only reaches HTML,
  // not URLs baked inside JS bundles. An asset URL emitted as an absolute root
  // path -- new URL("/assets/x.js", import.meta.url) -- resolves against the
  // origin and drops the base path prefix, so it 404s behind a subpath proxy
  // (notably the Pierre diff web worker). Emitting JS-referenced asset URLs as
  // relative makes them resolve against the entry chunk's own already-prefixed
  // location instead. HTML keeps default absolute URLs so the server-side
  // index.html rewrite still applies. Guarded by scripts/check-asset-base-paths.mjs.
  experimental: {
    renderBuiltUrl(_filename, { hostType }) {
      return hostType === "js" ? { relative: true } : undefined;
    },
  },
  plugins: [healthcheckPlugin(), devApiUrlPlugin(apiUrl), svelte(), svelteTesting({ autoCleanup: false })],
  resolve: {
    alias: [
      {
        find: /^@testing-library\/svelte$/,
        replacement: testingLibrarySvelteEntry,
      },
      {
        find: /^@kenn-io\/kit-ui\/utils\/markdown$/,
        replacement: kitUiMarkdownEntry,
      },
      {
        find: /^@kenn-io\/kit-ui\/utils\/time$/,
        replacement: kitUiTimeEntry,
      },
    ],
  },
  optimizeDeps: {
    // The frontend imports several heavy editor and diff dependencies. Left
    // undeclared, Vite discovers them mid-run on a cold optimizer, re-bundles,
    // and reloads the page underneath an in-flight browser-tier App mount.
    // Pre-bundle that exact cold-start set up front.
    //
    // @kenn-io/kit-ui is likewise consumed as Svelte source (svelte export
    // condition); its .svelte.ts rune modules cannot go through the dep
    // optimizer's plain-JS parse.
    exclude: ["@kenn-io/kit-ui", "@kenn-io/kata-ui"],
    include: [
      "@pierre/diffs",
      "@pierre/diffs/worker",
      "@tiptap/core",
      "@tiptap/extension-document",
      "@tiptap/extension-hard-break",
      "@tiptap/extension-paragraph",
      "@tiptap/extension-placeholder",
      "@tiptap/extension-text",
      "@tiptap/suggestion",
      "prosemirror-state",
      "shiki",
      "svelte-tiptap",
      // kit-ui-owned transitive deps, reached through its excluded barrel
      // (the markdown pipeline peers plus its own icon set — the icon paths
      // below are shared with the frontend list where they overlap).
      // mermaid is reached via kit-ui's dynamic import; without pre-bundling,
      // its CJS deps (dayjs) are served raw and fail ESM default-import interop.
      "@kenn-io/kit-ui > mermaid",
      "@kenn-io/kit-ui > marked",
      "@kenn-io/kit-ui > shiki",
      "@kenn-io/kit-ui > dompurify",
      "@kenn-io/kit-ui > @lucide/svelte/icons/arrow-down",
      "@kenn-io/kit-ui > @lucide/svelte/icons/arrow-up",
      "@kenn-io/kit-ui > @lucide/svelte/icons/arrow-up-down",
      "@kenn-io/kit-ui > @lucide/svelte/icons/calendar",
      "@kenn-io/kit-ui > @lucide/svelte/icons/check",
      "@kenn-io/kit-ui > @lucide/svelte/icons/chevron-down",
      "@kenn-io/kit-ui > @lucide/svelte/icons/chevron-left",
      "@kenn-io/kit-ui > @lucide/svelte/icons/chevron-right",
      "@kenn-io/kit-ui > @lucide/svelte/icons/chevron-up",
      "@kenn-io/kit-ui > @lucide/svelte/icons/copy",
      "@kenn-io/kit-ui > @lucide/svelte/icons/ellipsis",
      "@kenn-io/kit-ui > @lucide/svelte/icons/funnel",
      "@kenn-io/kit-ui > @lucide/svelte/icons/key-round",
      "@kenn-io/kit-ui > @lucide/svelte/icons/maximize-2",
      "@kenn-io/kit-ui > @lucide/svelte/icons/monitor",
      "@kenn-io/kit-ui > @lucide/svelte/icons/moon",
      "@kenn-io/kit-ui > @lucide/svelte/icons/panel-left-close",
      "@kenn-io/kit-ui > @lucide/svelte/icons/panel-left-open",
      "@kenn-io/kit-ui > @lucide/svelte/icons/refresh-cw",
      "@kenn-io/kit-ui > @lucide/svelte/icons/rotate-ccw",
      "@kenn-io/kit-ui > @lucide/svelte/icons/search",
      "@kenn-io/kit-ui > @lucide/svelte/icons/sun",
      "@kenn-io/kit-ui > @lucide/svelte/icons/wrap-text",
      "@kenn-io/kit-ui > @lucide/svelte/icons/x",
      // Frontend-resolvable deps the barrel also pulls in.
      "openapi-fetch",
      // The complete set of @lucide/svelte icon paths imported under
      // frontend/src.
      // Pre-bundling every icon -- not just the /pulls subset -- stops the cold
      // optimizer from discovering a new icon mid-run on issues/detail routes,
      // re-bundling, and reloading the page out from under a browser-tier mount.
      "@lucide/svelte/icons/columns-3",
      "@lucide/svelte/icons/grip-vertical",
      "@lucide/svelte/icons/maximize",
      "@lucide/svelte/icons/sliders-horizontal",
      "@lucide/svelte/icons/minimize",
      "@lucide/svelte/icons/alarm-clock",
      "@lucide/svelte/icons/alert-triangle",
      "@lucide/svelte/icons/archive-restore",
      "@lucide/svelte/icons/arrow-down",
      "@lucide/svelte/icons/arrow-left",
      "@lucide/svelte/icons/arrow-right",
      "@lucide/svelte/icons/arrow-right-left",
      "@lucide/svelte/icons/arrow-up",
      "@lucide/svelte/icons/arrow-up-down",
      "@lucide/svelte/icons/arrow-up-right",
      "@lucide/svelte/icons/ban",
      "@lucide/svelte/icons/book-open",
      "@lucide/svelte/icons/box",
      "@lucide/svelte/icons/calendar",
      "@lucide/svelte/icons/calendar-clock",
      "@lucide/svelte/icons/calendar-days",
      "@lucide/svelte/icons/check",
      "@lucide/svelte/icons/check-circle",
      "@lucide/svelte/icons/check-circle-2",
      "@lucide/svelte/icons/chevron-down",
      "@lucide/svelte/icons/chevron-left",
      "@lucide/svelte/icons/chevron-right",
      "@lucide/svelte/icons/chevron-up",
      "@lucide/svelte/icons/chevrons-down",
      "@lucide/svelte/icons/chevrons-down-up",
      "@lucide/svelte/icons/chevrons-up",
      "@lucide/svelte/icons/chevrons-up-down",
      "@lucide/svelte/icons/circle",
      "@lucide/svelte/icons/circle-alert",
      "@lucide/svelte/icons/circle-check",
      "@lucide/svelte/icons/circle-check-big",
      "@lucide/svelte/icons/circle-dashed",
      "@lucide/svelte/icons/circle-help",
      "@lucide/svelte/icons/clock",
      "@lucide/svelte/icons/clock-3",
      "@lucide/svelte/icons/command",
      "@lucide/svelte/icons/columns-2",
      "@lucide/svelte/icons/copy",
      "@lucide/svelte/icons/corner-down-left",
      "@lucide/svelte/icons/dot",
      "@lucide/svelte/icons/download",
      "@lucide/svelte/icons/ellipsis",
      "@lucide/svelte/icons/eraser",
      "@lucide/svelte/icons/eye-off",
      "@lucide/svelte/icons/external-link",
      "@lucide/svelte/icons/file-search",
      "@lucide/svelte/icons/file-text",
      "@lucide/svelte/icons/flag",
      "@lucide/svelte/icons/folder",
      "@lucide/svelte/icons/folder-git-2",
      "@lucide/svelte/icons/folder-input",
      "@lucide/svelte/icons/folder-open",
      "@lucide/svelte/icons/folder-tree",
      "@lucide/svelte/icons/funnel",
      "@lucide/svelte/icons/git-branch",
      "@lucide/svelte/icons/git-commit-horizontal",
      "@lucide/svelte/icons/git-merge",
      "@lucide/svelte/icons/git-pull-request",
      "@lucide/svelte/icons/history",
      "@lucide/svelte/icons/house",
      "@lucide/svelte/icons/inbox",
      "@lucide/svelte/icons/info",
      "@lucide/svelte/icons/keyboard",
      "@lucide/svelte/icons/layers",
      "@lucide/svelte/icons/layers-2",
      "@lucide/svelte/icons/layout-panel-left",
      "@lucide/svelte/icons/layout-panel-top",
      "@lucide/svelte/icons/link",
      "@lucide/svelte/icons/list-checks",
      "@lucide/svelte/icons/list-chevrons-down-up",
      "@lucide/svelte/icons/list-chevrons-up-down",
      "@lucide/svelte/icons/loader-circle",
      "@lucide/svelte/icons/message-square",
      "@lucide/svelte/icons/message-square-reply",
      "@lucide/svelte/icons/minus",
      "@lucide/svelte/icons/monitor",
      "@lucide/svelte/icons/monitor-up",
      "@lucide/svelte/icons/moon",
      "@lucide/svelte/icons/more-horizontal",
      "@lucide/svelte/icons/move",
      "@lucide/svelte/icons/network",
      "@lucide/svelte/icons/octagon-x",
      "@lucide/svelte/icons/package-plus",
      "@lucide/svelte/icons/panel-bottom",
      "@lucide/svelte/icons/panel-bottom-close",
      "@lucide/svelte/icons/panel-left-close",
      "@lucide/svelte/icons/panel-left-open",
      "@lucide/svelte/icons/panel-right",
      "@lucide/svelte/icons/panel-right-close",
      "@lucide/svelte/icons/panel-top",
      "@lucide/svelte/icons/paperclip",
      "@lucide/svelte/icons/pencil",
      "@lucide/svelte/icons/play",
      "@lucide/svelte/icons/plus",
      "@lucide/svelte/icons/refresh-ccw",
      "@lucide/svelte/icons/refresh-cw",
      "@lucide/svelte/icons/rotate-ccw",
      "@lucide/svelte/icons/rows-2",
      "@lucide/svelte/icons/save",
      "@lucide/svelte/icons/search",
      "@lucide/svelte/icons/send",
      "@lucide/svelte/icons/send-horizontal",
      "@lucide/svelte/icons/server",
      "@lucide/svelte/icons/space",
      "@lucide/svelte/icons/settings",
      "@lucide/svelte/icons/shield-alert",
      "@lucide/svelte/icons/sparkles",
      "@lucide/svelte/icons/square",
      "@lucide/svelte/icons/star",
      "@lucide/svelte/icons/sun",
      "@lucide/svelte/icons/tag",
      "@lucide/svelte/icons/tags",
      "@lucide/svelte/icons/terminal",
      "@lucide/svelte/icons/trash-2",
      "@lucide/svelte/icons/undo-2",
      "@lucide/svelte/icons/unlink",
      "@lucide/svelte/icons/upload",
      "@lucide/svelte/icons/user-check",
      "@lucide/svelte/icons/user-round",
      "@lucide/svelte/icons/users",
      "@lucide/svelte/icons/workflow",
      "@lucide/svelte/icons/x",
    ],
  },
  server: {
    host: "127.0.0.1",
    port: devServerPort,
    strictPort: true,
    ...(devServerAllowedHosts ? { allowedHosts: devServerAllowedHosts } : {}),
    hmr: devServerHmr,
    fs: { allow: [workspaceRoot, kitUiSourceRoot] },
    proxy: {
      "/api": {
        target: apiUrl,
        changeOrigin: true,
        timeout: 0,
        proxyTimeout: 0,
      },
      "/ws": terminalWebSocketProxy(apiUrl),
    },
  },
  test: {
    onUnhandledError(error) {
      // Vitest's global state manager owns unhandled errors across projects,
      // so this must live in the root config rather than the browser project.
      // Ignore only Vite's module-runner socket rejection during teardown;
      // all application errors still fail.
      const serializedStacks = "stacks" in error ? error.stacks : undefined;
      const hasViteClientFrame =
        error.stack?.includes("@vite/client") ||
        (Array.isArray(serializedStacks) &&
          serializedStacks.some(
            (frame: unknown) =>
              typeof frame === "object" &&
              frame !== null &&
              "file" in frame &&
              typeof frame.file === "string" &&
              frame.file.includes("@vite/client"),
          ));
      if (error.message === "WebSocket closed without opened." && hasViteClientFrame) {
        return false;
      }
      return undefined;
    },
    projects: [defineProject(nodeUnitTestProject), defineProject(jsdomUnitTestProject), browserTestProject],
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
  },
} satisfies UserConfig & { test: InlineConfig };

export default config;

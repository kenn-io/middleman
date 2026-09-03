import { afterEach, assert, it, vi } from "@effect/vitest";
import { Effect, Fiber, Layer } from "effect";
import { GeneratedApiLive } from "../api/generated-api.js";
import type {
  SettingsResponse as GeneratedSettingsResponse,
  UpdateFleetSettingsInputBody,
} from "../api/generated/models/index.js";
import { DEFAULT_TERMINAL_SETTINGS } from "../api/types.js";
import { StreamingFetchLive } from "../browser/streaming-fetch.js";
import { makeStartupSnapshot } from "../../test/startupSnapshot.js";
import { StartupWorkflowLive } from "../app/startup-workflow.js";
import { SettingsWorkflow, SettingsWorkflowLive, settingsErrorMessage } from "./settings-workflow.js";

type SettingsResponse = GeneratedSettingsResponse;
type FleetSettingsUpdate = UpdateFleetSettingsInputBody;

function makeSettings(repos: SettingsResponse["repos"] = []): SettingsResponse {
  return makeStartupSnapshot({
    repos,
    terminal: { ...DEFAULT_TERMINAL_SETTINGS, retained_sessions: 0 },
  });
}

const StartupTestLayer = Layer.provideMerge(StartupWorkflowLive, Layer.mergeAll(GeneratedApiLive, StreamingFetchLive));
const SettingsTestLayer = Layer.provideMerge(SettingsWorkflowLive, StartupTestLayer);

afterEach(() => {
  vi.unstubAllGlobals();
});

it.layer(SettingsTestLayer)("ordered settings writes", (it) => {
  it.effect("serializes fleet updates behind shared settings writes", () =>
    Effect.gen(function* () {
      let releaseFirstResponse = () => {};
      const firstResponse = new Promise<void>((resolve) => {
        releaseFirstResponse = resolve;
      });
      const observedFontSizes: number[] = [];
      const observedRequests: string[] = [];
      const settings = makeSettings();
      const fleetUpdate = {
        enabled: true,
        peer_timeout: "4s",
        sessions: { include_unmanaged_details: false },
      } satisfies FleetSettingsUpdate;
      let currentFleet = settings.fleet;
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const path = new URL(request.url).pathname;
        observedRequests.push(`${request.method} ${path}`);
        if (request.method === "GET" && path.endsWith("/settings")) {
          return Promise.resolve(Response.json({ ...settings, fleet: currentFleet }));
        }
        return request
          .clone()
          .json()
          .then((body: unknown) => {
            if (path.endsWith("/settings/fleet")) {
              currentFleet = { ...settings.fleet, ...fleetUpdate, restart_required: true };
              return Response.json(currentFleet);
            }
            if (
              typeof body !== "object" ||
              body === null ||
              !("terminal" in body) ||
              typeof body.terminal !== "object" ||
              body.terminal === null ||
              !("font_size" in body.terminal) ||
              typeof body.terminal.font_size !== "number"
            ) {
              return Response.json({ detail: "invalid settings request" }, { status: 400 });
            }
            observedFontSizes.push(body.terminal.font_size);
            const response = Response.json({
              ...settings,
              terminal: { ...settings.terminal, font_size: body.terminal.font_size },
            });
            if (observedFontSizes.length === 1) {
              return firstResponse.then(() => response);
            }
            return response;
          });
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const first = yield* Effect.forkChild(
        workflow.persist(() => ({ terminal: { ...settings.terminal, font_size: 13 } })),
      );
      yield* Effect.yieldNow;
      const second = yield* Effect.forkChild(workflow.updateFleet(fleetUpdate));
      yield* Effect.yieldNow;

      assert.deepStrictEqual(observedFontSizes, [13]);
      assert.deepStrictEqual(observedRequests, ["PUT /api/v1/settings"]);
      releaseFirstResponse();
      yield* Fiber.join(first);
      const updated = yield* Fiber.join(second);
      assert.isTrue(updated.enabled);
      assert.deepStrictEqual(observedRequests, ["PUT /api/v1/settings", "PUT /api/v1/settings/fleet"]);
    }),
  );

  it.effect("does not turn a committed repository removal into a refresh failure", () =>
    Effect.gen(function* () {
      const observedRequests: string[] = [];
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const path = new URL(request.url).pathname;
        observedRequests.push(`${request.method} ${path}`);
        if (request.method === "DELETE") return Promise.resolve(new Response(null, { status: 204 }));
        return Promise.reject(new Error("settings refresh unavailable"));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      yield* workflow.removeRepo("acme", "api", { provider: "github", host: "github.com" });

      assert.deepStrictEqual(observedRequests, ["DELETE /api/v1/repo/github/acme/api"]);
    }),
  );

  it.effect("aborts the generated preview request when its fiber is interrupted", () =>
    Effect.gen(function* () {
      let requestSignal: AbortSignal | undefined;
      let markStarted = () => {};
      const started = new Promise<void>((resolve) => {
        markStarted = resolve;
      });
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        requestSignal = request.signal;
        markStarted();
        return new Promise(() => {});
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const preview = yield* Effect.forkChild(
        workflow.previewRepos("acme", "*", { provider: "github", host: "github.com" }),
      );
      yield* Effect.promise(() => started);
      yield* Fiber.interrupt(preview);

      assert.isTrue(requestSignal?.aborted);
    }),
  );

  it.effect("rolls back a newly added exact repository when its clone path is rejected", () =>
    Effect.gen(function* () {
      const observedRequests: string[] = [];
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const path = new URL(request.url).pathname;
        observedRequests.push(`${request.method} ${path}`);
        if (request.method === "POST") return Promise.resolve(Response.json({ repos: [] }));
        if (request.method === "PUT") {
          return Promise.resolve(
            Response.json(
              {
                code: "validationError",
                detail: "clone path does not exist",
                type: "about:blank",
              },
              { status: 400 },
            ),
          );
        }
        return Promise.resolve(new Response(null, { status: 204 }));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const failure = yield* Effect.flip(
        workflow.promoteRepo(
          {
            provider: "github",
            host: "github.com",
            owner: "acme",
            name: "api",
            repo_path: "acme/api",
          },
          "/missing/api",
          false,
        ),
      );

      assert.strictEqual(failure._tag, "ApiProblemError");
      assert.deepStrictEqual(observedRequests, [
        "POST /api/v1/repos/bulk",
        "PUT /api/v1/repo/github/acme/api/worktree-base",
        "DELETE /api/v1/repo/github/acme/api",
      ]);
    }),
  );

  it.effect("reports a confirmed surviving repository after rollback fails", () =>
    Effect.gen(function* () {
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (request.method === "POST") return Promise.resolve(Response.json({ repos: [] }));
        if (request.method === "GET") {
          return Promise.resolve(
            Response.json(
              makeSettings([
                {
                  provider: "github",
                  platform_host: "github.com",
                  owner: "acme",
                  name: "api",
                  repo_path: "acme/api",
                  is_glob: false,
                  matched_repo_count: 0,
                  hidden_from_ui: false,
                },
              ]),
            ),
          );
        }
        const detail = request.method === "PUT" ? "clone path does not exist" : "repository removal failed";
        return Promise.resolve(
          Response.json(
            {
              code: request.method === "PUT" ? "validationError" : "upstreamError",
              detail,
              type: "about:blank",
            },
            { status: request.method === "PUT" ? 400 : 502 },
          ),
        );
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const failure = yield* Effect.flip(
        workflow.promoteRepo(
          {
            provider: "github",
            host: "github.com",
            owner: "acme",
            name: "api",
            repo_path: "acme/api",
          },
          "/missing/api",
          false,
        ),
      );

      assert.strictEqual(failure._tag, "RepoPromotionRollbackError");
      if (failure._tag !== "RepoPromotionRollbackError") return;
      assert.strictEqual(failure.settings.repos[0]?.repo_path, "acme/api");
      assert.strictEqual(
        settingsErrorMessage(failure),
        "clone path does not exist; rollback failed: repository removal failed",
      );
    }),
  );

  it.effect("returns the original promotion failure when reconciliation confirms rollback", () =>
    Effect.gen(function* () {
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (request.method === "POST") return Promise.resolve(Response.json({ repos: [] }));
        if (request.method === "GET") return Promise.resolve(Response.json(makeSettings()));
        if (request.method === "DELETE") {
          return Promise.resolve(
            Response.json(
              { code: "upstreamError", detail: "repository removal failed", type: "about:blank" },
              { status: 502 },
            ),
          );
        }
        return Promise.resolve(
          Response.json(
            { code: "validationError", detail: "clone path does not exist", type: "about:blank" },
            { status: 400 },
          ),
        );
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const failure = yield* Effect.flip(
        workflow.promoteRepo(
          { provider: "github", host: "github.com", owner: "acme", name: "api", repo_path: "acme/api" },
          "/missing/api",
          false,
        ),
      );

      assert.strictEqual(failure._tag, "ApiProblemError");
      assert.strictEqual(settingsErrorMessage(failure), "clone path does not exist");
    }),
  );

  it.effect("reports uncertain promotion state when rollback reconciliation also fails", () =>
    Effect.gen(function* () {
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (request.method === "POST") return Promise.resolve(Response.json({ repos: [] }));
        const detail =
          request.method === "PUT"
            ? "clone path does not exist"
            : request.method === "DELETE"
              ? "repository removal failed"
              : "settings unavailable";
        return Promise.resolve(
          Response.json(
            {
              code: request.method === "PUT" ? "validationError" : "upstreamError",
              detail,
              type: "about:blank",
            },
            { status: request.method === "PUT" ? 400 : 502 },
          ),
        );
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const failure = yield* Effect.flip(
        workflow.promoteRepo(
          {
            provider: "github",
            host: "github.com",
            owner: "delta",
            name: "uncertain-api",
            repo_path: "delta/uncertain-api",
          },
          "/missing/uncertain-api",
          false,
        ),
      );

      assert.strictEqual(failure._tag, "RepoPromotionStateUncertainError");
      assert.strictEqual(
        settingsErrorMessage(failure),
        "clone path does not exist; repository state could not be confirmed. Reload settings before retrying",
      );
    }),
  );

  it.effect("reconciles a previous uncertain promotion before allowing its retry", () =>
    Effect.gen(function* () {
      const repo = {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        is_glob: false,
        matched_repo_count: 0,
        hidden_from_ui: false,
      };
      const configured = makeSettings([repo]);
      const saved = makeSettings([{ ...repo, worktree_base_path: "/worktrees/api" }]);
      const observedRequests: string[] = [];
      let firstAttempt = true;
      let reconciliationRequests = 0;
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const path = new URL(request.url).pathname;
        observedRequests.push(`${request.method} ${path}`);
        if (request.method === "POST") return Promise.resolve(Response.json({ repos: [] }));
        if (request.method === "GET") {
          reconciliationRequests += 1;
          if (firstAttempt) {
            firstAttempt = false;
            return Promise.resolve(
              Response.json(
                { code: "upstreamError", detail: "settings unavailable", type: "about:blank" },
                { status: 502 },
              ),
            );
          }
          return Promise.resolve(Response.json(configured));
        }
        if (request.method === "DELETE") {
          return Promise.resolve(
            Response.json(
              { code: "upstreamError", detail: "repository removal failed", type: "about:blank" },
              { status: 502 },
            ),
          );
        }
        if (reconciliationRequests === 0) {
          return Promise.resolve(
            Response.json(
              { code: "validationError", detail: "clone path does not exist", type: "about:blank" },
              { status: 400 },
            ),
          );
        }
        return Promise.resolve(Response.json(saved));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      yield* Effect.exit(
        workflow.promoteRepo(
          { provider: "github", owner: "acme", name: "api", repo_path: "acme/api" },
          "/worktrees/api",
          false,
        ),
      );
      const result = yield* workflow.promoteRepo(
        { provider: "github", owner: "acme", name: "api", repo_path: "acme/api" },
        "/worktrees/api",
        false,
      );

      assert.strictEqual(result.repos[0]?.worktree_base_path, "/worktrees/api");
      assert.deepStrictEqual(observedRequests, [
        "POST /api/v1/repos/bulk",
        "PUT /api/v1/repo/github/acme/api/worktree-base",
        "DELETE /api/v1/repo/github/acme/api",
        "GET /api/v1/settings",
        "GET /api/v1/settings",
        "PUT /api/v1/repo/github/acme/api/worktree-base",
      ]);
    }),
  );

  it.effect("continues promotion after a lost add response when reconciliation finds the exact repository", () =>
    Effect.gen(function* () {
      const observedRequests: string[] = [];
      const configured = makeSettings([
        {
          provider: "github",
          platform_host: "github.com",
          owner: "acme",
          name: "api",
          repo_path: "acme/api",
          is_glob: false,
          matched_repo_count: 0,
          hidden_from_ui: false,
        },
      ]);
      const saved = {
        ...configured,
        repos: configured.repos.map((repo) => ({ ...repo, worktree_base_path: "/worktrees/api" })),
      };
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const path = new URL(request.url).pathname;
        observedRequests.push(`${request.method} ${path}`);
        if (request.method === "POST") return Promise.reject(new TypeError("response lost after commit"));
        if (request.method === "GET") return Promise.resolve(Response.json(configured));
        return Promise.resolve(Response.json(saved));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const result = yield* workflow.promoteRepo(
        { provider: "github", owner: "acme", name: "api", repo_path: "acme/api" },
        "/worktrees/api",
        false,
      );

      assert.strictEqual(result.repos[0]?.worktree_base_path, "/worktrees/api");
      assert.deepStrictEqual(observedRequests, [
        "POST /api/v1/repos/bulk",
        "GET /api/v1/settings",
        "PUT /api/v1/repo/github/acme/api/worktree-base",
      ]);
    }),
  );

  it.effect("does not confuse a custom-host repository with an omitted default-host promotion", () =>
    Effect.gen(function* () {
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (request.method === "POST") return Promise.resolve(Response.json({ repos: [] }));
        if (request.method === "GET") {
          return Promise.resolve(
            Response.json(
              makeSettings([
                {
                  provider: "github",
                  platform_host: "github.enterprise.test",
                  owner: "acme",
                  name: "api",
                  repo_path: "acme/api",
                  is_glob: false,
                  matched_repo_count: 0,
                  hidden_from_ui: false,
                },
              ]),
            ),
          );
        }
        const detail = request.method === "PUT" ? "clone path does not exist" : "repository removal failed";
        return Promise.resolve(Response.json({ code: "upstreamError", detail, type: "about:blank" }, { status: 502 }));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const failure = yield* Effect.flip(
        workflow.promoteRepo(
          { provider: "gh", owner: "acme", name: "api", repo_path: "acme/api" },
          "/missing/api",
          false,
        ),
      );

      assert.strictEqual(failure._tag, "ApiProblemError");
    }),
  );

  it.effect("returns the authoritative settings after a save response is lost", () =>
    Effect.gen(function* () {
      const original = makeSettings();
      const saved = { ...original, activity: { ...original.activity, hide_bots: true } };
      let committed = false;
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (request.method === "PUT") {
          committed = true;
          return Promise.reject(new TypeError("response lost after commit"));
        }
        return Promise.resolve(Response.json(committed ? saved : original));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const result = yield* workflow.persist(() => ({ activity: saved.activity }));

      assert.isTrue(result.activity.hide_bots);
    }),
  );

  it.effect("returns authoritative detail settings after a save response is lost", () =>
    Effect.gen(function* () {
      const original = makeSettings();
      const saved = { ...original, detail: { initial_timeline_entry_limit: 80 } };
      let committed = false;
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (request.method === "PUT") {
          committed = true;
          return Promise.reject(new TypeError("response lost after commit"));
        }
        return Promise.resolve(Response.json(committed ? saved : original));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const result = yield* workflow.persist(() => ({ detail: saved.detail }));

      assert.strictEqual(result.detail.initial_timeline_entry_limit, 80);
    }),
  );

  it.effect("does not report detail settings committed when reconciliation returns the prior value", () =>
    Effect.gen(function* () {
      const original = makeSettings();
      const requested = { initial_timeline_entry_limit: 80 };
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        return request.method === "PUT"
          ? Promise.reject(new TypeError("response lost before commit"))
          : Promise.resolve(Response.json(original));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const failure = yield* Effect.flip(workflow.persist(() => ({ detail: requested })));

      assert.strictEqual(failure._tag, "TransientTransportError");
    }),
  );

  it.effect("reconciles repository preset saves after a response is lost", () =>
    Effect.gen(function* () {
      const original = makeSettings();
      const repoPresets = [
        {
          name: "Review queue",
          repos: [
            {
              provider: "github",
              platform_host: "github.com",
              platform_repo_id: "R_widgets",
              repo_path: "acme/widgets",
            },
          ],
        },
      ];
      const saved = { ...original, repo_presets: repoPresets };
      let committed = false;
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        if (request.method === "POST") {
          committed = true;
          return Promise.reject(new TypeError("response lost after commit"));
        }
        return Promise.resolve(Response.json(committed ? saved : original));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const result = yield* workflow.createRepoPreset(repoPresets[0]!);

      assert.deepStrictEqual(result.repo_presets, repoPresets);
    }),
  );

  it.effect("does not report repository presets committed when reconciliation returns the old collection", () =>
    Effect.gen(function* () {
      const original = makeSettings();
      const repoPresets = [
        {
          name: "Review queue",
          repos: [
            {
              provider: "github",
              platform_host: "github.com",
              platform_repo_id: "R_widgets",
              repo_path: "acme/widgets",
            },
          ],
        },
      ];
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        return request.method === "POST"
          ? Promise.reject(new TypeError("response lost before commit"))
          : Promise.resolve(Response.json(original));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const failure = yield* Effect.flip(workflow.createRepoPreset(repoPresets[0]!));

      assert.strictEqual(failure._tag, "TransientTransportError");
    }),
  );

  it.effect("returns the authoritative repository list after a bulk-add response is lost", () =>
    Effect.gen(function* () {
      const configured = makeSettings([
        {
          provider: "gitlab",
          platform_host: "gitlab.com",
          owner: "acme",
          name: "api",
          repo_path: "acme/api",
          is_glob: false,
          matched_repo_count: 0,
          hidden_from_ui: false,
        },
      ]);
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        return request.method === "POST"
          ? Promise.reject(new TypeError("response lost after commit"))
          : Promise.resolve(Response.json(configured));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const result = yield* workflow.bulkAddRepos([
        { provider: "gl", owner: "acme", name: "api", repo_path: "acme/api" },
      ]);

      assert.strictEqual(result.repos[0]?.provider, "gitlab");
    }),
  );

  it.effect("reconciles retained settings uncertainty before retrying the same intent", () =>
    Effect.gen(function* () {
      const original = makeSettings();
      const saved = { ...original, activity: { ...original.activity, hide_bots: true } };
      const observedRequests: string[] = [];
      let putRequests = 0;
      let settingsReads = 0;
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const path = new URL(request.url).pathname;
        observedRequests.push(`${request.method} ${path}`);
        if (request.method === "PUT") {
          putRequests += 1;
          return putRequests === 1
            ? Promise.reject(new TypeError("response lost after commit"))
            : Promise.resolve(Response.json(saved));
        }
        settingsReads += 1;
        return settingsReads === 1
          ? Promise.resolve(
              Response.json(
                { code: "upstreamError", detail: "settings unavailable", type: "about:blank" },
                { status: 502 },
              ),
            )
          : Promise.resolve(Response.json(saved));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const first = yield* workflow.persist(() => ({ activity: saved.activity })).pipe(Effect.exit);
      const second = yield* workflow.persist(() => ({ activity: saved.activity }));

      assert.strictEqual(first._tag, "Failure");
      assert.isTrue(second.activity.hide_bots);
      assert.strictEqual(putRequests, 1);
      assert.deepStrictEqual(observedRequests, [
        "PUT /api/v1/settings",
        "GET /api/v1/settings",
        "GET /api/v1/settings",
      ]);
    }),
  );

  it.effect("accepts a committed Roborev toggle after its response is lost", () =>
    Effect.gen(function* () {
      const original = makeSettings();
      const saved = { ...original, roborev: { init_managed_clones: true } };
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        return request.method === "PUT"
          ? Promise.reject(new TypeError("response lost after commit"))
          : Promise.resolve(Response.json(saved));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const result = yield* workflow.persist(() => ({ roborev: saved.roborev }));

      assert.isTrue(result.roborev.init_managed_clones);
    }),
  );

  it.effect("reports a Roborev toggle that was not committed after its response is lost", () =>
    Effect.gen(function* () {
      const original = makeSettings();
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        return request.method === "PUT"
          ? Promise.reject(new TypeError("response lost before commit"))
          : Promise.resolve(Response.json(original));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const failure = yield* Effect.flip(workflow.persist(() => ({ roborev: { init_managed_clones: true } })));

      assert.strictEqual(failure._tag, "TransientTransportError");
    }),
  );

  it.effect("shares retained repository uncertainty between bulk add and regular settings", () =>
    Effect.gen(function* () {
      const configured = makeSettings([
        {
          provider: "github",
          platform_host: "github.com",
          owner: "acme",
          name: "api",
          repo_path: "acme/api",
          is_glob: false,
          matched_repo_count: 0,
          hidden_from_ui: false,
        },
      ]);
      let bulkRequests = 0;
      let regularRequests = 0;
      let settingsReads = 0;
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const path = new URL(request.url).pathname;
        if (request.method === "POST" && path.endsWith("/repos/bulk")) {
          bulkRequests += 1;
          return Promise.reject(new TypeError("bulk add response lost after commit"));
        }
        if (request.method === "POST" && path.endsWith("/repos")) {
          regularRequests += 1;
          return Promise.resolve(Response.json(configured));
        }
        settingsReads += 1;
        return settingsReads === 1
          ? Promise.resolve(
              Response.json(
                { code: "upstreamError", detail: "settings unavailable", type: "about:blank" },
                { status: 502 },
              ),
            )
          : Promise.resolve(Response.json(configured));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const first = yield* workflow
        .bulkAddRepos([{ provider: "gh", host: "github.com", owner: "acme", name: "api", repo_path: "acme/api" }])
        .pipe(Effect.exit);
      const recovered = yield* workflow.addRepo("acme", "api", {
        provider: "github",
        host: "github.com",
      });

      assert.strictEqual(first._tag, "Failure");
      assert.strictEqual(recovered.repos[0]?.name, "api");
      assert.strictEqual(bulkRequests, 1);
      assert.strictEqual(regularRequests, 0);
    }),
  );

  it.effect("preserves a definite repository refresh rejection", () =>
    Effect.gen(function* () {
      const observedRequests: string[] = [];
      const fetch: typeof globalThis.fetch = (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const path = new URL(request.url).pathname;
        observedRequests.push(`${request.method} ${path}`);
        return request.method === "POST"
          ? Promise.resolve(
              Response.json(
                { code: "upstreamError", detail: "refresh rejected", type: "about:blank" },
                { status: 502 },
              ),
            )
          : Promise.resolve(Response.json(makeSettings()));
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const failure = yield* workflow
        .refreshRepo("acme", "api", { provider: "github", host: "github.com" })
        .pipe(Effect.flip);

      assert.strictEqual(failure._tag, "ApiProblemError");
      assert.deepStrictEqual(observedRequests, ["POST /api/v1/repo/github/acme/api/refresh"]);
    }),
  );

  it.effect("saves repository UI visibility through the provider route", () =>
    Effect.gen(function* () {
      const repo = {
        provider: "github",
        platform_host: "github.com",
        owner: "acme",
        name: "api",
        repo_path: "acme/api",
        is_glob: false,
        matched_repo_count: 1,
        hidden_from_ui: false,
      };
      const saved = makeSettings([{ ...repo, hidden_from_ui: true }]);
      const observedRequests: string[] = [];
      const observedBodies: unknown[] = [];
      const fetch: typeof globalThis.fetch = async (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init);
        observedRequests.push(`${request.method} ${new URL(request.url).pathname}`);
        observedBodies.push(JSON.parse(await request.text()));
        return Response.json(saved);
      };
      vi.stubGlobal("fetch", fetch);

      const workflow = yield* SettingsWorkflow;
      const result = yield* workflow.updateRepoUIVisibility(
        "acme",
        "api",
        { provider: "github", host: "github.com" },
        true,
      );

      assert.strictEqual(result.repos[0]?.hidden_from_ui, true);
      assert.deepStrictEqual(observedRequests, ["PUT /api/v1/repo/github/acme/api/ui-visibility"]);
      assert.deepStrictEqual(observedBodies, [{ hidden: true }]);
    }),
  );
});

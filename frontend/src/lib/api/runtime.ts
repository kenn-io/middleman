import { configuredAPIBaseURL } from "./runtime-base.js";
import type { ProblemError } from "./generated/models/problemError.js";
import { isProblem, type ProblemBody } from "./problems.js";
import { normalizedFetch, type FetchFn } from "./request.js";

import { traceHeadersForRequest } from "../instrumentation/traceContext.js";

export const apiBaseURL = configuredAPIBaseURL();

// Attaches W3C trace context to every request so server spans join the
// frontend's minted traces (see frontend/src/lib/instrumentation/traceContext.ts).
export function tracedFetch(inner: FetchFn): FetchFn {
  return (input, init) => {
    if (globalThis.crypto?.getRandomValues === undefined) return inner(input, init);
    const headers = new Headers(input instanceof Request ? input.headers : init?.headers);
    if (input instanceof Request && init?.headers) {
      new Headers(init.headers).forEach((value, key) => headers.set(key, value));
    }
    const { traceparent, baggage } = traceHeadersForRequest();
    headers.set("traceparent", traceparent);
    if (baggage !== null) headers.set("baggage", baggage);
    return inner(input, { ...init, headers });
  };
}

export interface OrvalRequestOptions extends RequestInit {
  readonly baseURL?: string;
  readonly fetch?: FetchFn;
}

export class InvalidGeneratedErrorResponse extends Error {
  constructor(readonly body: unknown) {
    super("Generated API returned an invalid problem response");
    this.name = "InvalidGeneratedErrorResponse";
  }
}

export class GeneratedProblemResponse extends Error {
  constructor(
    readonly problem: ProblemBody,
    readonly response: Response,
  ) {
    super(problem.detail ?? problem.title ?? `API ${problem.status}`);
    this.name = "GeneratedProblemResponse";
  }
}

export async function orvalRequest(url: string, options: OrvalRequestOptions = {}): Promise<Response> {
  const { baseURL = configuredAPIBaseURL(), fetch: fetchOption, ...init } = options;
  const inner = fetchOption ?? ((...args: Parameters<typeof globalThis.fetch>) => globalThis.fetch(...args));
  const requestURL = `${baseURL.replace(/\/$/, "")}/${url.replace(/^\//, "")}`;
  return normalizedFetch(tracedFetch(inner))(requestURL, init);
}

export async function orvalFetch<A>(url: string, options: OrvalRequestOptions = {}): Promise<A> {
  const response = await orvalRequest(url, options);
  const contentType = response.headers.get("Content-Type")?.split(";")[0]?.trim().toLowerCase();
  const binary = response.ok && (contentType === "application/octet-stream" || contentType?.startsWith("image/"));
  const body = [204, 205, 304].includes(response.status) ? "" : binary ? await response.blob() : await response.text();
  const value: unknown = body
    ? typeof body === "string" && contentType?.includes("json")
      ? JSON.parse(body)
      : body
    : undefined;
  if (response.ok) return value as A;
  if (!isProblem(value)) throw new InvalidGeneratedErrorResponse(value);
  throw new GeneratedProblemResponse(value, response);
}

export function apiErrorMessage(
  error: Pick<Partial<ProblemError>, "detail" | "title"> | undefined,
  fallback: string,
): string {
  return error?.detail ?? error?.title ?? fallback;
}

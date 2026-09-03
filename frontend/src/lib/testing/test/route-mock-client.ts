import * as generatedClient from "../../api/generated/index.js";
import type { GeneratedClient } from "../../api/generated-api.js";
import type { FetchFn } from "../../api/request.js";
import { makeGeneratedClient } from "../generated-client.js";

type RouteMethod = (path: string, options?: Record<string, unknown>) => Promise<unknown>;
export type RouteMockClient = Partial<Record<"GET" | "POST" | "PUT" | "PATCH" | "DELETE", RouteMethod>>;

function parameterName(name: string): string {
  return name === "platformHost" ? "platform_host" : name.replaceAll(/([a-z0-9])([A-Z])/g, "$1_$2").toLowerCase();
}

function decodedValue(value: string): string | number | boolean {
  if (value === "true") return true;
  if (value === "false") return false;
  if (/^-?\d+(?:\.\d+)?$/.test(value)) return Number(value);
  return value;
}

async function routeRequestOptions(input: RequestInfo | URL, init: RequestInit | undefined, args: readonly unknown[]) {
  const rawURL = input instanceof Request ? input.url : String(input);
  const url = new URL(rawURL, "http://forge.test");
  let path = url.pathname.replace(/^\/api\/v1(?=\/|$)/, "") || "/";
  const pathParameters: Record<string, unknown> = {};

  for (const argument of args) {
    if (typeof argument !== "object" || argument === null || argument instanceof AbortSignal) continue;
    for (const [name, value] of Object.entries(argument)) {
      if (!["string", "number", "boolean"].includes(typeof value) || value === "") continue;
      const encoded = encodeURIComponent(String(value));
      const marker = `{${parameterName(name)}}`;
      const segments = path.split("/");
      const parameterIndex = segments.findLastIndex((segment) => segment === encoded);
      if (parameterIndex >= 0) segments[parameterIndex] = marker;
      const replaced = segments.join("/");
      if (replaced !== path) {
        path = replaced;
        pathParameters[parameterName(name)] = value;
      }
    }
  }
  path = path
    .split("/")
    .map((segment) => (segment.startsWith("{") ? segment : decodeURIComponent(segment)))
    .join("/");

  const queryParameters: Record<string, unknown> = {};
  const arrayQueryKeys = new Set(
    args.flatMap((argument) =>
      typeof argument === "object" && argument !== null
        ? Object.entries(argument)
            .filter(([, value]) => Array.isArray(value))
            .map(([key]) => key)
        : [],
    ),
  );
  for (const key of new Set(url.searchParams.keys())) {
    const values = url.searchParams.getAll(key).map(decodedValue);
    queryParameters[key] = values.length === 1 && !arrayQueryKeys.has(key) ? values[0] : values;
  }

  const rawBody =
    typeof init?.body === "string"
      ? init.body
      : input instanceof Request && input.body !== null
        ? await input.clone().text()
        : "";
  const body: unknown = rawBody.length > 0 ? JSON.parse(rawBody) : undefined;
  const signal = input instanceof Request ? input.signal : init?.signal;
  return {
    path,
    options: {
      ...(Object.keys(pathParameters).length > 0 || Object.keys(queryParameters).length > 0
        ? {
            params: {
              ...(Object.keys(pathParameters).length > 0 && { path: pathParameters }),
              ...(Object.keys(queryParameters).length > 0 && { query: queryParameters }),
            },
          }
        : {}),
      ...(body !== undefined && { body }),
      ...(signal && { signal }),
    },
  };
}

function responseFromRouteMockResult(result: unknown): Response {
  if (result instanceof Response) return result;
  if (typeof result !== "object" || result === null) {
    return result === undefined ? new Response(null, { status: 204 }) : Response.json(result);
  }
  const record = result as Record<string, unknown>;
  const response = record.response instanceof Response ? record.response : undefined;
  if (record.error !== undefined && record.error !== null) {
    const status = response?.status ?? 500;
    const error =
      typeof record.error === "object" && record.error !== null
        ? { code: "internalError", status, title: "Request failed", type: "about:blank", ...record.error }
        : { code: "internalError", status, title: "Request failed", type: "about:blank", detail: String(record.error) };
    return Response.json(error, { status, ...(response && { headers: response.headers }) });
  }
  if (record.data === undefined) {
    return new Response(null, { status: response?.status ?? 204, ...(response && { headers: response.headers }) });
  }
  return Response.json(record.data, {
    status: response?.status ?? 200,
    ...(response && { headers: response.headers }),
  });
}

function bindRouteMockOperation(
  operation: (...args: never[]) => unknown,
  routeClient: RouteMockClient,
): (...args: never[]) => unknown {
  return (...args: never[]) => {
    const fetch: FetchFn = async (input, init) => {
      const method = (init?.method ?? (input instanceof Request ? input.method : "GET")).toUpperCase();
      const handler = routeClient[method as keyof RouteMockClient];
      if (handler === undefined) throw new Error(`No ${method} route mock configured`);
      const request = await routeRequestOptions(input, init, args);
      return responseFromRouteMockResult(await handler(request.path, request.options));
    };
    const callArgs: unknown[] = [...args];
    const suppliedOptionsIndex = callArgs.findLastIndex(
      (argument) =>
        typeof argument === "object" &&
        argument !== null &&
        ("signal" in argument || "baseURL" in argument || "fetch" in argument),
    );
    const optionsIndex = suppliedOptionsIndex >= 0 ? suppliedOptionsIndex : Math.max(0, operation.length - 1);
    while (callArgs.length <= optionsIndex) callArgs.push(undefined);
    const options = callArgs[optionsIndex];
    callArgs[optionsIndex] = {
      ...(typeof options === "object" && options !== null ? options : {}),
      baseURL: "http://forge.test/api/v1",
      fetch,
    };
    return operation(...(callArgs as never[]));
  };
}

export function makeGeneratedClientFromRouteMocks(client: RouteMockClient): GeneratedClient {
  const overrides: Record<string, Record<string, unknown>> = {};
  for (const [serviceName, service] of Object.entries(generatedClient)) {
    if (!serviceName.endsWith("Service") || typeof service !== "object" || service === null) continue;
    const operations: Record<string, unknown> = {};
    for (const [operationName, operation] of Object.entries(service)) {
      if (operationName.startsWith("get") && operationName.endsWith("Url")) continue;
      if (typeof operation !== "function") continue;
      operations[operationName] = bindRouteMockOperation(operation as (...args: never[]) => unknown, client);
    }
    overrides[serviceName] = operations;
  }
  return makeGeneratedClient(overrides as Parameters<typeof makeGeneratedClient>[0]);
}

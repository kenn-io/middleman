import * as generatedClient from "../api/generated/index.js";
import type { GeneratedClient } from "../api/generated-api.js";

type GeneratedClientOverrides = {
  readonly [Service in keyof GeneratedClient]?: Partial<GeneratedClient[Service]>;
};

export function makeGeneratedClient(overrides: GeneratedClientOverrides = {}): GeneratedClient {
  const client = { ...generatedClient } as Record<string, unknown>;
  for (const [name, override] of Object.entries(overrides)) {
    const original = generatedClient[name as keyof GeneratedClient];
    client[name] =
      typeof original === "object" && original !== null && typeof override === "object" && override !== null
        ? { ...original, ...override }
        : override;
  }
  return client as GeneratedClient;
}

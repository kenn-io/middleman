<script lang="ts">
  import { Effect } from "effect";
  import { onDestroy, untrack, type ComponentProps } from "svelte";
  import * as client from "../api/generated/index.js";
  import { setAppRuntime } from "../app/runtime-context.js";
  import { makeTestAppRuntime } from "../testing/effect-layers.js";
  import RepoTypeahead from "./RepoTypeahead.svelte";

  const runtime = makeTestAppRuntime(client);
  setAppRuntime(untrack(() => runtime));
  onDestroy(() => {
    Effect.runFork(runtime.disposeEffect);
  });
  const props: ComponentProps<typeof RepoTypeahead> = $props();
</script>

<RepoTypeahead {...props} />

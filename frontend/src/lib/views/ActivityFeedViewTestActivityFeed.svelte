<script lang="ts">
  interface Props {
    onSelectBranchCommit?: (item: unknown) => void;
  }

  const { onSelectBranchCommit }: Props = $props();

  const branchCommitItem = {
    repo: {
      provider: "github",
      platform_host: "github.com",
      repo_path: "acme/widgets",
      owner: "acme",
      name: "widgets",
    },
    branch_name: "main",
    commit_sha: "abcdef1234567890abcdef1234567890abcdef12",
    body_preview: "Fix the thing",
  };
</script>

<div data-testid="activity-feed">Activity feed</div>
<!-- The real feed fires this from a branch row; exposing it as a button lets a
     spec drive the commit selection without a full feed fixture. -->
<button type="button" data-testid="select-branch-commit" onclick={() => onSelectBranchCommit?.(branchCommitItem)}>
  select branch commit
</button>

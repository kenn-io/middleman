import type {
  BodySnippet as GeneratedBodySnippet,
  CreateDocsFolderInputBody,
  CrossFolderHit,
  DocsBrowseEntry,
  DocsBrowseOutputBody,
  DocsFolderResponse,
  DocsSearchAllOutputBody,
  DocsSearchOutputBody,
  GitChangesResponse as GeneratedGitChangesResponse,
  GitStatusEntry as GeneratedGitStatusEntry,
  GitStatusResponse as GeneratedGitStatusResponse,
  Hit,
  Node,
  PublishChange,
  PublishResponse,
  PullResponse,
  SnippetRange as GeneratedSnippetRange,
} from "../generated/models/index.js";

// Generated OpenAPI schemas remain the wire authority. These aliases are
// exact when the UI consumes the wire shape directly and narrow only the
// nullable collections or string enums that the Docs API adapter validates.

export type Folder = DocsFolderResponse;

export type AddFolderInput = Omit<CreateDocsFolderInputBody, "path"> & {
  path: string;
};

export type BrowseEntry = DocsBrowseEntry;

export type BrowseResponse = Omit<DocsBrowseOutputBody, "entries" | "parent"> & {
  parent: string;
  entries: BrowseEntry[];
};

type TreeNodeWire = Node;

export type TreeNode = Omit<TreeNodeWire, "children"> & {
  children?: TreeNode[];
};

export type SearchHit = Hit;

export type SearchResponse = Omit<DocsSearchOutputBody, "hits"> & {
  hits: SearchHit[];
};

export interface DocsAPIError extends Error {
  status: number;
  code?: string;
}

// These refinements mirror @pierre/trees and the server's documented Docs
// status set. The API adapter rejects an unknown generated string before it
// reaches the component tree.
export type GitFileStatus = "added" | "deleted" | "ignored" | "modified" | "renamed" | "untracked";

export type GitStatusEntry = Omit<GeneratedGitStatusEntry, "status"> & {
  status: GitFileStatus;
};

export type GitStatusResponse = Omit<GeneratedGitStatusResponse, "entries"> & {
  entries: GitStatusEntry[];
};

export type SnippetRange = GeneratedSnippetRange;

export type BodySnippet = Omit<GeneratedBodySnippet, "matches"> & {
  matches: SnippetRange[];
};

export type CrossFolderSearchHit = Omit<CrossFolderHit, "hit_type" | "snippet"> & {
  hit_type: "filename" | "body";
  snippet?: BodySnippet;
};

export type CrossFolderSearchResponse = Omit<DocsSearchAllOutputBody, "hits" | "warnings"> & {
  hits: CrossFolderSearchHit[];
  warnings?: string[];
};

export type GitPublishChangeStatus = "added" | "deleted" | "modified" | "renamed" | "untracked";

export type GitPublishChange = Omit<PublishChange, "status"> & {
  status: GitPublishChangeStatus;
};

export type GitChangesResponse = Omit<GeneratedGitChangesResponse, "changes"> & {
  changes: GitPublishChange[];
};

export type GitPublishResponse = Omit<PublishResponse, "files"> & {
  files: GitPublishChange[];
};

export type GitPullResponse = PullResponse;

package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/forge/platform"
)

const (
	maxMCPDiffFileBytes     = 10 << 20
	maxMCPDiffFileNameBytes = 180
)

type getItemDiffInput struct {
	Item         itemRefInput `json:"item"`
	EmitDiffFile bool         `json:"emit_diff_file,omitempty"`
}

type diffFileRow struct {
	Path        string `json:"path"`
	OldPath     string `json:"old_path,omitempty"`
	Status      string `json:"status"`
	IsBinary    bool   `json:"is_binary"`
	IsGenerated bool   `json:"is_generated"`
	Additions   int    `json:"additions"`
	Deletions   int    `json:"deletions"`
}

type diffFileHandle struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type getItemDiffOutput struct {
	Stale          bool            `json:"stale"`
	TotalAdditions int             `json:"total_additions"`
	TotalDeletions int             `json:"total_deletions"`
	Files          []diffFileRow   `json:"files"`
	DiffFile       *diffFileHandle `json:"diff_file,omitempty"`
}

func (s *Server) registerDiffTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "kenn_forge_get_item_diff",
		Description: "Return cached PR diff evidence. By default this is a compact file summary; " +
			"set emit_diff_file to write the full unified diff to a local temp file.",
	}, wrapTool(s.getItemDiff))
}

func (s *Server) getItemDiff(ctx context.Context, in getItemDiffInput) (getItemDiffOutput, error) {
	if err := validateItemRef(in.Item); err != nil {
		return getItemDiffOutput{}, err
	}
	if in.Item.Type != "pr" {
		return getItemDiffOutput{}, &Error{
			Kind:    "invalid_request",
			Message: "diff is only available for prs",
		}
	}

	diff, err := s.backend.GetPullDiff(ctx, itemIdentity(in.Item), in.EmitDiffFile)
	if err != nil {
		return getItemDiffOutput{}, diffRouteError(err)
	}
	if !in.EmitDiffFile {
		return diffOutputFromFiles(diff), nil
	}
	out := diffOutputFromFiles(diff)
	data, err := serializeDiffPatches(diff.Files)
	if err != nil {
		return getItemDiffOutput{}, err
	}
	store, err := s.diffStore()
	if err != nil {
		return getItemDiffOutput{}, &Error{Kind: "internal_error", Message: "create diff temp store: " + err.Error()}
	}
	path, size, err := store.write(diffFileName(in.Item), data)
	if errors.Is(err, errDiffCacheFileTooLarge) {
		return getItemDiffOutput{}, &Error{
			Kind: "diff_too_large",
			Message: "diff exceeds the configured MCP diff cache; increase mcp.diff_cache_mb" +
				" or use a local checkout",
		}
	}
	if err != nil {
		return getItemDiffOutput{}, &Error{Kind: "internal_error", Message: "write diff file: " + err.Error()}
	}
	out.DiffFile = &diffFileHandle{Path: path, Bytes: size}
	return out, nil
}

func diffOutputFromFiles(resp Diff) getItemDiffOutput {
	out := getItemDiffOutput{
		Stale: resp.Stale,
		Files: make([]diffFileRow, 0, len(resp.Files)),
	}
	for _, file := range resp.Files {
		out.TotalAdditions += file.Additions
		out.TotalDeletions += file.Deletions
		out.Files = append(out.Files, diffFileRow{
			Path:        file.Path,
			OldPath:     file.OldPath,
			Status:      file.Status,
			IsBinary:    file.IsBinary,
			IsGenerated: file.IsGenerated,
			Additions:   file.Additions,
			Deletions:   file.Deletions,
		})
	}
	return out
}

func (s *Server) diffStore() (*diffFileStore, error) {
	s.diffMu.Lock()
	defer s.diffMu.Unlock()
	if s.diffs != nil {
		return s.diffs, nil
	}
	store, err := newDiffFileStore(s.diffCacheBytes)
	if err != nil {
		return nil, err
	}
	s.diffs = store
	return store, nil
}

func serializeDiffPatches(files []DiffFile) ([]byte, error) {
	var buf bytes.Buffer
	for _, file := range files {
		patch := file.Patch
		if patch == "" {
			patch = synthesizeDiffEvidence(file)
		}
		if buf.Len()+len(patch) > maxMCPDiffFileBytes {
			return nil, &Error{
				Kind:    "diff_too_large",
				Message: "diff is too large for MCP temp-file handoff; use a local checkout",
			}
		}
		buf.WriteString(patch)
	}
	return buf.Bytes(), nil
}

func synthesizeDiffEvidence(file DiffFile) string {
	oldPath := file.OldPath
	if oldPath == "" {
		oldPath = file.Path
	}
	var buf strings.Builder
	fmt.Fprintf(
		&buf, "diff --git %s %s\n",
		diffPatchPath("a/"+oldPath), diffPatchPath("b/"+file.Path),
	)
	switch file.Status {
	case "renamed":
		fmt.Fprintf(
			&buf, "rename from %s\nrename to %s\n",
			diffPatchPath(oldPath), diffPatchPath(file.Path),
		)
	case "copied":
		fmt.Fprintf(
			&buf, "copy from %s\ncopy to %s\n",
			diffPatchPath(oldPath), diffPatchPath(file.Path),
		)
	}
	if file.IsBinary {
		binaryOldPath := "a/" + oldPath
		binaryNewPath := "b/" + file.Path
		if file.Status == "added" {
			binaryOldPath = "/dev/null"
		}
		if file.Status == "deleted" {
			binaryNewPath = "/dev/null"
		}
		fmt.Fprintf(
			&buf, "Binary files %s and %s differ\n",
			diffPatchPath(binaryOldPath), diffPatchPath(binaryNewPath),
		)
		return buf.String()
	}
	switch file.Status {
	case "added":
		fmt.Fprintf(&buf, "--- /dev/null\n+++ %s\n", diffPatchPath("b/"+file.Path))
	case "deleted":
		fmt.Fprintf(&buf, "--- %s\n+++ /dev/null\n", diffPatchPath("a/"+oldPath))
	}
	return buf.String()
}

func diffPatchPath(path string) string {
	if path == "/dev/null" {
		return path
	}
	for _, r := range path {
		if r == '"' || r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return strconv.Quote(path)
		}
	}
	return path
}

func diffRouteError(err error) error {
	var derr *Error
	if !errors.As(err, &derr) {
		return err
	}
	msg := strings.ToLower(derr.Message)
	if derr.Kind == "not_found" && isDiffIdentityNotFound(derr, msg) {
		return err
	}
	if derr.Kind == "not_found" ||
		strings.Contains(msg, "clone manager") ||
		strings.Contains(msg, "diff not available") ||
		strings.Contains(msg, "file list not available") ||
		strings.Contains(msg, "diff view not available") ||
		strings.Contains(msg, "files view not available") {
		return &Error{
			Kind:    "diff_unavailable",
			Message: derr.Message,
			Details: derr.Details,
		}
	}
	return err
}

func isDiffIdentityNotFound(derr *Error, msg string) bool {
	switch derr.Code {
	case "repoNotFound", "pullNotFound":
		return true
	}
	return strings.Contains(msg, "pull request not found") ||
		strings.Contains(msg, "repo not found") ||
		strings.Contains(msg, "repository not found")
}

func diffFileName(ref itemRefInput) string {
	ref = canonicalDiffFileRef(ref)
	identity := fmt.Sprintf(
		"%s\x00%s\x00%s\x00%s\x00%s\x00%d",
		ref.Provider,
		ref.PlatformHost,
		ref.PlatformRepoID,
		ref.Owner,
		ref.Name,
		ref.Number,
	)
	sum := sha256.Sum256([]byte(identity))
	suffix := fmt.Sprintf("-pr-%d-%x.diff", ref.Number, sum[:12])
	prefix := fmt.Sprintf("%s-%s-%s-%s",
		sanitizeDiffName(ref.Provider),
		sanitizeDiffName(ref.PlatformHost),
		sanitizeDiffName(ref.Owner),
		sanitizeDiffName(ref.Name),
	)
	prefix = truncateDiffNamePrefix(prefix, maxMCPDiffFileNameBytes-len(suffix))
	return prefix + suffix
}

func canonicalDiffFileRef(ref itemRefInput) itemRefInput {
	ref.Provider = strings.TrimSpace(ref.Provider)
	ref.PlatformHost = strings.TrimSpace(ref.PlatformHost)
	ref.Owner = strings.Trim(strings.TrimSpace(ref.Owner), "/")
	ref.Name = strings.Trim(strings.TrimSpace(ref.Name), "/")
	kind, err := platform.NormalizeKind(ref.Provider)
	if err != nil {
		return ref
	}
	ref.Provider = string(kind)
	if platform.LowercaseRepoNames(kind) {
		ref.Owner = strings.ToLower(ref.Owner)
		ref.Name = strings.ToLower(ref.Name)
	}
	if host, ok := platform.HostOrDefault(kind, ref.PlatformHost); ok {
		ref.PlatformHost = strings.ToLower(strings.TrimSpace(host))
	}
	return ref
}

func sanitizeDiffName(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r == '.' || r == '_' || r == '-' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		if unicode.IsSpace(r) {
			b.WriteByte('_')
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

func truncateDiffNamePrefix(prefix string, maxBytes int) string {
	if maxBytes <= 0 {
		return "diff"
	}
	if len(prefix) > maxBytes {
		prefix = prefix[:maxBytes]
	}
	prefix = strings.Trim(prefix, "-_")
	if prefix == "" {
		return "diff"
	}
	return prefix
}

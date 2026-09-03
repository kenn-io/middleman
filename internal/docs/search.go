package docs

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"go.kenn.io/forge/internal/config"
)

// Hit is a single search result. Score is unstable across releases but
// monotonic within a single response: higher score = better match.
type Hit struct {
	Name    string `json:"name"`
	RelPath string `json:"rel_path"`
	Score   int    `json:"score"`
}

// Search returns filename hits for a query string. Substring match,
// case-insensitive; exact filename matches and prefix matches outrank
// general substrings. Results are capped at limit; pass 0 for no cap.
func (r *Registry) Search(folderID, query string, limit int) ([]Hit, error) {
	tree, err := r.Tree(folderID)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, nil
	}

	var hits []Hit
	var visit func(Node)
	visit = func(n Node) {
		for _, child := range n.Children {
			if child.IsDir {
				visit(child)
				continue
			}
			name := strings.ToLower(child.Name)
			rel := strings.ToLower(child.RelPath)
			score := scoreMatch(name, rel, q)
			if score == 0 {
				continue
			}
			hits = append(hits, Hit{
				Name:    child.Name,
				RelPath: child.RelPath,
				Score:   score,
			})
		}
	}
	visit(tree)

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].RelPath < hits[j].RelPath
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func scoreMatch(name, rel, q string) int {
	switch {
	case strings.TrimSuffix(name, extOf(name)) == q:
		return 100
	case name == q:
		return 90
	case strings.HasPrefix(name, q):
		return 70
	case strings.Contains(name, q):
		return 50
	case strings.Contains(rel, q):
		return 30
	default:
		return 0
	}
}

func extOf(name string) string {
	idx := strings.LastIndexByte(name, '.')
	if idx < 0 {
		return ""
	}
	return name[idx:]
}

// CrossFolderHit is a single search result emitted by SearchAll. It
// merges the per-file filename score (if any) with the per-file body
// hit (if any) - one row per file, with HitType disambiguating which
// path produced it.
type CrossFolderHit struct {
	Folder     string       `json:"folder"`
	FolderName string       `json:"folder_name"`
	Name       string       `json:"name"`
	RelPath    string       `json:"rel_path"`
	Score      int          `json:"score"`
	HitType    string       `json:"hit_type"` // "filename" | "body"
	Line       int          `json:"line,omitempty,omitzero"`
	Snippet    *BodySnippet `json:"snippet,omitempty"`
}

// SearchAllResult is the orchestrator's domain return: the handler
// wraps it in the JSON wire shape with the query echoed back.
type SearchAllResult struct {
	Hits      []CrossFolderHit
	Warnings  []string
	Truncated bool
}

const searchAllWorkerLimit = 8

// SearchAll walks every registered folder, scoring each markdown file
// against query on both filename and body content. The scan is bounded
// by a per-request worker pool of searchAllWorkerLimit goroutines and
// is context-aware - cancel ctx and the next-file boundary aborts
// promptly. Returns at most limit hits; Truncated is set when more
// hits existed than limit (detected by comparing collected hit count against limit).
//
// Per-folder failures (Tree() errored, e.g. the folder vanished on disk)
// are surfaced via Warnings, not as an error. SearchAll returns an
// error only when ALL folders failed.
func (r *Registry) SearchAll(ctx context.Context, query string, limit int) (SearchAllResult, error) {
	lowerQuery := strings.ToLower(strings.TrimSpace(query))
	if lowerQuery == "" || limit <= 0 {
		return SearchAllResult{}, nil
	}

	folders := r.Folders()
	if len(folders) == 0 {
		return SearchAllResult{}, nil
	}

	var (
		mu       sync.Mutex
		allHits  []CrossFolderHit
		warnings []string
		failed   int
	)

	for _, folder := range folders {
		if ctx.Err() != nil {
			break
		}
		tree, err := r.Tree(folder.ID)
		if err != nil {
			failed++
			mu.Lock()
			warnings = append(warnings, folder.Name+": "+err.Error())
			mu.Unlock()
			continue
		}
		hits, folderWarns := scanFolder(ctx, r, folder, tree, lowerQuery)
		mu.Lock()
		allHits = append(allHits, hits...)
		warnings = append(warnings, folderWarns...)
		mu.Unlock()
	}

	if failed > 0 && failed == len(folders) {
		return SearchAllResult{}, fmt.Errorf("all %d folders failed", len(folders))
	}

	sort.SliceStable(allHits, func(i, j int) bool {
		return compareCrossFolderHits(allHits[i], allHits[j])
	})

	truncated := false
	if len(allHits) > limit {
		truncated = true
		allHits = allHits[:limit]
	}
	return SearchAllResult{Hits: allHits, Warnings: warnings, Truncated: truncated}, nil
}

// scanFolder walks the markdown files of one folder using a bounded
// worker pool. Per-file IO failures are logged at the caller level
// (the orchestrator's caller has the slogger); recoverable warnings
// (oversize, token-too-long) surface via the warnings return.
//
// Each candidate path is resolved through r.resolve() before the body
// scan so an in-folder .md symlink can't expose snippets from outside
// the folder root - the same containment ReadFile enforces.
func scanFolder(ctx context.Context, r *Registry, folder config.DocFolder, tree Node, lowerQuery string) ([]CrossFolderHit, []string) {
	files := flattenMarkdown(folder.Path, tree)

	sem := make(chan struct{}, searchAllWorkerLimit)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		hits     []CrossFolderHit
		warnings []string
	)
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(f fileEntry) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			lowerName := strings.ToLower(filepath.Base(f.relPath))
			lowerRel := strings.ToLower(f.relPath)
			filenameScore := scoreMatch(lowerName, lowerRel, lowerQuery)
			// Containment check: drop entries whose symlink target leaves
			// the folder root. Without this an in-folder .md symlink
			// could expose body snippets from outside the registered root.
			scanPath, resolveErr := r.resolve(folder.ID, f.relPath)
			if resolveErr != nil {
				return
			}
			// Symlinks can point at non-markdown files inside the same
			// folder (e.g., `leak.md` -> `notes.txt` or `config.json`).
			// Re-check the *resolved* target's extension before scanning
			// its body, otherwise body snippets from arbitrary in-root
			// files leak through search.
			if err := ensureMarkdown(scanPath); err != nil {
				return
			}
			if err := ensureRegularFile(scanPath); err != nil {
				return
			}
			body, warn, err := scanBody(scanPath, lowerQuery)
			if err != nil {
				// Body scan IO failure - still emit the filename hit
				// when the filename matched, so a readable tree entry
				// whose file can't be opened doesn't disappear from
				// search.
				if filenameScore > 0 {
					mu.Lock()
					hits = append(hits, CrossFolderHit{
						Folder:     folder.ID,
						FolderName: folder.Name,
						Name:       filepath.Base(f.relPath),
						RelPath:    f.relPath,
						HitType:    "filename",
						Score:      filenameScore,
					})
					mu.Unlock()
				}
				return
			}
			if filenameScore == 0 && body.Line == 0 {
				if warn != "" {
					mu.Lock()
					warnings = append(warnings, warn)
					mu.Unlock()
				}
				return
			}
			hit := CrossFolderHit{
				Folder:     folder.ID,
				FolderName: folder.Name,
				Name:       filepath.Base(f.relPath),
				RelPath:    f.relPath,
			}
			if filenameScore > 0 {
				hit.HitType = "filename"
				hit.Score = filenameScore
			} else {
				hit.HitType = "body"
				hit.Score = body.Score
			}
			if body.Line > 0 {
				hit.Line = body.Line
				snippet := body.Snippet
				hit.Snippet = &snippet
			}
			mu.Lock()
			hits = append(hits, hit)
			if warn != "" {
				warnings = append(warnings, warn)
			}
			mu.Unlock()
		}(f)
	}
	wg.Wait()
	return hits, warnings
}

type fileEntry struct {
	relPath string
	absPath string
}

func flattenMarkdown(root string, n Node) []fileEntry {
	var out []fileEntry
	var visit func(Node)
	visit = func(node Node) {
		if !node.IsDir {
			out = append(out, fileEntry{
				relPath: node.RelPath,
				absPath: filepath.Join(root, filepath.FromSlash(node.RelPath)),
			})
			return
		}
		for _, child := range node.Children {
			visit(child)
		}
	}
	visit(n)
	return out
}

// compareCrossFolderHits implements the composite sort key from the
// spec: bucket (filename=0, body=1) -> score desc -> folder_name -> rel_path
// -> line ascending. Filename-only hits use Line=0 so the line tiebreaker
// is stable.
func compareCrossFolderHits(a, b CrossFolderHit) bool {
	ab := bucket(a.HitType)
	bb := bucket(b.HitType)
	if ab != bb {
		return ab < bb
	}
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.FolderName != b.FolderName {
		return a.FolderName < b.FolderName
	}
	if a.RelPath != b.RelPath {
		return a.RelPath < b.RelPath
	}
	return a.Line < b.Line
}

func bucket(hitType string) int {
	if hitType == "filename" {
		return 0
	}
	return 1
}

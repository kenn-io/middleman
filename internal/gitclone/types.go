package gitclone

// DiffResult is the structured output of a git diff operation.
type DiffResult struct {
	Stale               bool       `json:"stale"`
	WhitespaceOnlyCount int        `json:"whitespace_only_count"`
	Files               []DiffFile `json:"files"`
}

// FileContent is the raw content for one file at a git revision.
type FileContent struct {
	Path string
	Data []byte
	Size int64
}

// DiffFile represents one file in a diff.
type DiffFile struct {
	Path             string `json:"path"`
	OldPath          string `json:"old_path"`
	Status           string `json:"status"` // added, modified, deleted, renamed, copied
	IsBinary         bool   `json:"is_binary"`
	IsGenerated      bool   `json:"is_generated"`
	IsWhitespaceOnly bool   `json:"is_whitespace_only"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch"`
	Hunks            []Hunk `json:"hunks"`
}

// Hunk represents one contiguous section of changes in a file.
type Hunk struct {
	OldStart int    `json:"old_start"`
	OldCount int    `json:"old_count"`
	NewStart int    `json:"new_start"`
	NewCount int    `json:"new_count"`
	Section  string `json:"section,omitempty"`
	Lines    []Line `json:"lines"`
}

// Line represents one line in a diff hunk.
type Line struct {
	Type      string `json:"type"` // context, add, delete
	Content   string `json:"content"`
	OldNum    int    `json:"old_num,omitempty,omitzero"`
	NewNum    int    `json:"new_num,omitempty,omitzero"`
	NoNewline bool   `json:"no_newline,omitempty"`
}

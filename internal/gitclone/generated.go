package gitclone

import (
	"bytes"
	"path/filepath"
	"strings"
)

var generatedBasenames = map[string]bool{
	"bun.lock":            true,
	"bun.lockb":           true,
	"cargo.lock":          true,
	"composer.lock":       true,
	"deno.lock":           true,
	"flake.lock":          true,
	"gemfile.lock":        true,
	"go.sum":              true,
	"gradle.lockfile":     true,
	"mix.lock":            true,
	"npm-shrinkwrap.json": true,
	"package-lock.json":   true,
	"pipfile.lock":        true,
	"pnpm-lock.yaml":      true,
	"poetry.lock":         true,
	"pubspec.lock":        true,
	".terraform.lock.hcl": true,
	"terraform.lock.hcl":  true,
	"uv.lock":             true,
	"yarn.lock":           true,
}

var generatedSuffixes = []string{
	".lock",
	".lock.json",
	".lock.yaml",
	".lock.yml",
}

// GeneratedAttributeInput returns NUL-delimited paths for git check-attr.
func GeneratedAttributeInput(files []DiffFile) []byte {
	var input bytes.Buffer
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		path := file.Path
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		input.WriteString(path)
		input.WriteByte(0)
	}
	return input.Bytes()
}

// ParseLinguistGeneratedAttributes parses `git check-attr -z` triples.
func ParseLinguistGeneratedAttributes(out []byte) map[string]bool {
	parts := bytes.Split(out, []byte{0})
	generated := make(map[string]bool)
	for i := 0; i+2 < len(parts); i += 3 {
		path := string(parts[i])
		attr := string(parts[i+1])
		value := string(parts[i+2])
		if path == "" || attr != "linguist-generated" {
			continue
		}
		if value == "unspecified" {
			continue
		}
		generated[path] = value == "set" || value == "true"
	}
	return generated
}

// MarkGeneratedFiles applies Linguist metadata and local generated heuristics.
func MarkGeneratedFiles(files []DiffFile, linguistGenerated map[string]bool) {
	for i := range files {
		if generated, ok := linguistGenerated[files[i].Path]; ok {
			files[i].IsGenerated = generated
			continue
		}
		files[i].IsGenerated = IsGeneratedPath(files[i].Path)
	}
}

// IsGeneratedPath recognizes generated artifacts that are useful even without
// repository-specific Linguist attributes.
func IsGeneratedPath(path string) bool {
	base := strings.ToLower(filepath.Base(filepath.ToSlash(path)))
	if generatedBasenames[base] {
		return true
	}
	for _, suffix := range generatedSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

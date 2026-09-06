// Package terminalpaste stores browser clipboard images long enough for a
// terminal application to read the pasted path.
package terminalpaste

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const (
	// MaxImageBytes is the largest browser clipboard image accepted by Forge.
	MaxImageBytes = 20 << 20

	retention     = 7 * 24 * time.Hour
	maxCacheBytes = int64(1 << 30)
)

// ErrUnsupportedImage means the payload does not have a supported image
// signature.
var ErrUnsupportedImage = errors.New("unsupported image format")

// Store owns the private cache of images pasted into web terminals.
type Store struct {
	mu       sync.Mutex
	root     string
	now      func() time.Time
	maxBytes int64
}

// NewStore creates the cache directory and applies its retention policy.
func NewStore(root string) (*Store, error) {
	return newStore(root, time.Now, maxCacheBytes)
}

func newStore(root string, now func() time.Time, maxBytes int64) (*Store, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve terminal paste image cache: %w", err)
	}
	store := &Store{root: absRoot, now: now, maxBytes: maxBytes}
	if err := store.prepareRoot(); err != nil {
		return nil, err
	}
	if err := store.sweepLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

// Save validates data by signature and writes it to a private path whose
// filename is extension-safe and contains no spaces. The returned path is
// absolute.
func (s *Store) Save(data []byte) (string, error) {
	extension, err := imageExtension(data)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.prepareRoot(); err != nil {
		return "", err
	}
	if err := s.sweepLocked(); err != nil {
		return "", err
	}

	file, err := os.CreateTemp(s.root, "paste-image-*"+extension)
	if err != nil {
		return "", fmt.Errorf("create terminal paste image: %w", err)
	}
	path := file.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("secure terminal paste image: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("write terminal paste image: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close terminal paste image: %w", err)
	}
	now := s.now()
	if err := os.Chtimes(path, now, now); err != nil {
		return "", fmt.Errorf("timestamp terminal paste image: %w", err)
	}
	remove = false
	if err := s.enforceCapLocked(); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) prepareRoot() error {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return fmt.Errorf("create terminal paste image cache: %w", err)
	}
	if err := os.Chmod(s.root, 0o700); err != nil {
		return fmt.Errorf("secure terminal paste image cache: %w", err)
	}
	return nil
}

func (s *Store) sweepLocked() error {
	entries, err := s.cacheFiles()
	if err != nil {
		return err
	}
	cutoff := s.now().Add(-retention)
	for _, entry := range entries {
		if entry.modTime.Before(cutoff) {
			if err := os.Remove(entry.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove expired terminal paste image: %w", err)
			}
		}
	}
	return s.enforceCapLocked()
}

func (s *Store) enforceCapLocked() error {
	entries, err := s.cacheFiles()
	if err != nil {
		return err
	}
	var total int64
	for _, entry := range entries {
		total += entry.size
	}
	slices.SortFunc(entries, func(a, b cacheFile) int {
		if order := a.modTime.Compare(b.modTime); order != 0 {
			return order
		}
		return bytes.Compare([]byte(a.path), []byte(b.path))
	})
	for _, entry := range entries {
		if total <= s.maxBytes {
			break
		}
		if err := os.Remove(entry.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("evict terminal paste image: %w", err)
		}
		total -= entry.size
	}
	return nil
}

type cacheFile struct {
	path    string
	size    int64
	modTime time.Time
}

func (s *Store) cacheFiles() ([]cacheFile, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("read terminal paste image cache: %w", err)
	}
	files := make([]cacheFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("inspect terminal paste image: %w", err)
		}
		files = append(files, cacheFile{
			path:    filepath.Join(s.root, entry.Name()),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
	}
	return files, nil
}

func imageExtension(data []byte) (string, error) {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")):
		return ".png", nil
	case len(data) >= 3 && bytes.Equal(data[:3], []byte("\xff\xd8\xff")):
		return ".jpg", nil
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ".webp", nil
	default:
		return "", ErrUnsupportedImage
	}
}

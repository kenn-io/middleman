package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.kenn.io/forge/platform"
	"golang.org/x/sync/singleflight"
)

const (
	markdownImageCacheTTL      = 14 * 24 * time.Hour
	markdownImageMutableTTL    = 5 * time.Minute
	markdownImageFetchTimeout  = 30 * time.Second
	markdownImageCacheMaxBytes = int64(512 << 20)
	markdownImageCacheMagic    = "kenn-forge-markdown-image-v2\n"
	markdownImageMutableFlag   = "mutable"
	markdownImageImmutableFlag = "immutable"
)

type markdownImageCache struct {
	root  string
	mu    sync.Mutex
	group singleflight.Group
}

func newMarkdownImageCache(root string) *markdownImageCache {
	return &markdownImageCache{root: root}
}

func markdownImageCacheRoot(dataDir string) string {
	if strings.TrimSpace(dataDir) == "" {
		return ""
	}
	return filepath.Join(dataDir, "cache", "markdown-images")
}

func (c *markdownImageCache) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.root, hex.EncodeToString(sum[:])+".cache")
}

func (c *markdownImageCache) get(key string) (platform.MarkdownImage, bool) {
	if c == nil || c.root == "" {
		return platform.MarkdownImage{}, false
	}
	path := c.path(key)
	info, err := os.Stat(path)
	if err != nil {
		return platform.MarkdownImage{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return platform.MarkdownImage{}, false
	}
	image, ok := decodeMarkdownImageCacheEntry(data)
	if !ok {
		_ = os.Remove(path)
		return platform.MarkdownImage{}, false
	}
	ttl := markdownImageCacheTTL
	if image.Mutable {
		ttl = markdownImageMutableTTL
	}
	if time.Since(info.ModTime()) > ttl {
		_ = os.Remove(path)
		return platform.MarkdownImage{}, false
	}
	return image, true
}

// Cache entries are the magic line, the content type line, a mutability flag
// line, then the raw bytes.
func encodeMarkdownImageCacheHeader(image platform.MarkdownImage) string {
	flag := markdownImageImmutableFlag
	if image.Mutable {
		flag = markdownImageMutableFlag
	}
	return markdownImageCacheMagic + image.ContentType + "\n" + flag + "\n"
}

func decodeMarkdownImageCacheEntry(data []byte) (platform.MarkdownImage, bool) {
	if !bytes.HasPrefix(data, []byte(markdownImageCacheMagic)) {
		return platform.MarkdownImage{}, false
	}
	payload := data[len(markdownImageCacheMagic):]
	typeEnd := bytes.IndexByte(payload, '\n')
	if typeEnd <= 0 {
		return platform.MarkdownImage{}, false
	}
	rest := payload[typeEnd+1:]
	flagEnd := bytes.IndexByte(rest, '\n')
	if flagEnd <= 0 {
		return platform.MarkdownImage{}, false
	}
	var mutable bool
	switch string(rest[:flagEnd]) {
	case markdownImageMutableFlag:
		mutable = true
	case markdownImageImmutableFlag:
		mutable = false
	default:
		return platform.MarkdownImage{}, false
	}
	return platform.MarkdownImage{
		ContentType: string(payload[:typeEnd]),
		Content:     rest[flagEnd+1:],
		Mutable:     mutable,
	}, true
}

func (c *markdownImageCache) load(
	ctx context.Context,
	key string,
	fetch func(context.Context) (platform.MarkdownImage, error),
) (platform.MarkdownImage, error) {
	if image, ok := c.get(key); ok {
		return image, nil
	}
	resultCh := c.group.DoChan(key, func() (any, error) {
		if image, ok := c.get(key); ok {
			return image, nil
		}
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markdownImageFetchTimeout)
		defer cancel()
		image, err := fetch(fetchCtx)
		if err != nil {
			return platform.MarkdownImage{}, err
		}
		if err := c.set(key, image); err != nil {
			slog.Warn("cache markdown image", "err", err)
		}
		return image, nil
	})
	select {
	case <-ctx.Done():
		return platform.MarkdownImage{}, ctx.Err()
	case result := <-resultCh:
		if result.Err != nil {
			return platform.MarkdownImage{}, result.Err
		}
		return result.Val.(platform.MarkdownImage), nil
	}
}

func (c *markdownImageCache) set(key string, image platform.MarkdownImage) error {
	if c == nil || c.root == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.root, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(c.root, ".markdown-image-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err = temp.WriteString(encodeMarkdownImageCacheHeader(image)); err == nil {
		_, err = temp.Write(image.Content)
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tempPath, c.path(key)); err != nil {
		return err
	}
	return c.evictLocked(time.Now())
}

type markdownImageCacheFile struct {
	path    string
	size    int64
	modTime time.Time
}

func (c *markdownImageCache) evictLocked(now time.Time) error {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return err
	}
	files := make([]markdownImageCacheFile, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".cache") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		path := filepath.Join(c.root, entry.Name())
		if now.Sub(info.ModTime()) > markdownImageCacheTTL {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return removeErr
			}
			continue
		}
		files = append(files, markdownImageCacheFile{path: path, size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, file := range files {
		if total <= markdownImageCacheMaxBytes {
			break
		}
		if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		total -= file.size
	}
	return nil
}

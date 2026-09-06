package terminalpaste

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreSaveChoosesExtensionFromImageSignature(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	tests := []struct {
		name string
		data []byte
		ext  string
	}{
		{name: "png", data: append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 24)...), ext: ".png"},
		{name: "jpeg", data: []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00payload"), ext: ".jpg"},
		{name: "webp", data: []byte("RIFF\x10\x00\x00\x00WEBPVP8 payload"), ext: ".webp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			path, err := store.Save(tt.data)
			require.NoError(err)
			assert.Equal(tt.ext, filepath.Ext(path))
			stored, err := os.ReadFile(path)
			require.NoError(err)
			assert.Equal(tt.data, stored)
		})
	}
}

func TestStoreSaveRejectsUnsupportedContent(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	_, err = store.Save([]byte("not an image"))

	assert.ErrorIs(t, err, ErrUnsupportedImage)
}

func TestStoreSweepsExpiredFilesAtStartup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root := t.TempDir()
	expired := filepath.Join(root, "expired.png")
	recent := filepath.Join(root, "recent.png")
	require.NoError(os.WriteFile(expired, []byte("old"), 0o600))
	require.NoError(os.WriteFile(recent, []byte("new"), 0o600))
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	require.NoError(os.Chtimes(expired, now.Add(-retention-time.Hour), now.Add(-retention-time.Hour)))
	require.NoError(os.Chtimes(recent, now.Add(-retention+time.Hour), now.Add(-retention+time.Hour)))

	store, err := newStore(root, func() time.Time { return now }, maxCacheBytes)
	require.NoError(err)

	assert.NoFileExists(expired)
	assert.FileExists(recent)
	assert.NotNil(store)
}

func TestStoreEvictsOldestFileBeforeNewlySavedImage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root := t.TempDir()
	oldest := filepath.Join(root, "oldest.png")
	newer := filepath.Join(root, "newer.png")
	require.NoError(os.WriteFile(oldest, []byte("1234"), 0o600))
	require.NoError(os.WriteFile(newer, []byte("5678"), 0o600))
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	require.NoError(os.Chtimes(oldest, now.Add(-2*time.Hour), now.Add(-2*time.Hour)))
	require.NoError(os.Chtimes(newer, now.Add(-time.Hour), now.Add(-time.Hour)))
	store, err := newStore(root, func() time.Time { return now }, 12)
	require.NoError(err)

	newPath, err := store.Save([]byte("\x89PNG\r\n\x1a\n"))
	require.NoError(err)

	assert.NoFileExists(oldest)
	assert.FileExists(newer)
	assert.FileExists(newPath)
}

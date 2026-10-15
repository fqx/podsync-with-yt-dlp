package fs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

var (
	testCtx = context.Background()
)

func TestNewLocal(t *testing.T) {
	local, err := NewLocal("", false, false)
	assert.NoError(t, err)
	assert.NotNil(t, local)
}

func TestLocal_Create(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "")
	assert.NoError(t, err)

	stor, err := NewLocal(tmpDir, false, false)
	assert.NoError(t, err)

	written, err := stor.Create(testCtx, "1/test", bytes.NewBuffer([]byte{1, 5, 7, 8, 3}))
	assert.NoError(t, err)
	assert.EqualValues(t, 5, written)

	stat, err := os.Stat(filepath.Join(tmpDir, "1", "test"))
	assert.NoError(t, err)
	assert.EqualValues(t, 5, stat.Size())
}

func TestLocal_Size(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "")
	assert.NoError(t, err)

	defer os.RemoveAll(tmpDir)

	stor, err := NewLocal(tmpDir, false, false)
	assert.NoError(t, err)

	_, err = stor.Create(testCtx, "1/test", bytes.NewBuffer([]byte{1, 5, 7, 8, 3}))
	assert.NoError(t, err)

	sz, err := stor.Size(testCtx, "1/test")
	assert.NoError(t, err)
	assert.EqualValues(t, 5, sz)
}

func TestLocal_NoSize(t *testing.T) {
	stor, err := NewLocal("", false, false)
	assert.NoError(t, err)

	_, err = stor.Size(testCtx, "1/test")
	assert.True(t, os.IsNotExist(err))
}

func TestLocal_Delete(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "")
	assert.NoError(t, err)

	stor, err := NewLocal(tmpDir, false, false)
	assert.NoError(t, err)

	_, err = stor.Create(testCtx, "1/test", bytes.NewBuffer([]byte{1, 5, 7, 8, 3}))
	assert.NoError(t, err)

	err = stor.Delete(testCtx, "1/test")
	assert.NoError(t, err)

	_, err = stor.Size(testCtx, "1/test")
	assert.True(t, os.IsNotExist(err))

	_, err = os.Stat(filepath.Join(tmpDir, "1", "test"))
	assert.True(t, os.IsNotExist(err))

	err = stor.Delete(testCtx, "1/test")
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func TestLocal_copyFile(t *testing.T) {
	reader := bytes.NewReader([]byte{1, 2, 4})
	tmpDir, err := os.MkdirTemp("", "")
	assert.NoError(t, err)

	file := filepath.Join(tmpDir, "1")

	l := &Local{}
	size, err := l.copyFile(reader, file)
	assert.NoError(t, err)
	assert.EqualValues(t, 3, size)

	stat, err := os.Stat(file)
	assert.NoError(t, err)
	assert.EqualValues(t, 3, stat.Size())
}

func TestLocal_NoListing(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "")
	assert.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create a file inside a subdirectory
	stor, err := NewLocal(tmpDir, false, true)
	assert.NoError(t, err)

	_, err = stor.Create(testCtx, "feed1/episode.mp3", bytes.NewBuffer([]byte{1, 2, 3}))
	assert.NoError(t, err)

	// Opening a file should work
	file, err := stor.Open("/feed1/episode.mp3")
	assert.NoError(t, err)
	file.Close()

	// Opening a directory should return ErrNotExist when NoListing is enabled
	_, err = stor.Open("/feed1")
	assert.True(t, os.IsNotExist(err))

	// Opening root should also return ErrNotExist
	_, err = stor.Open("/")
	assert.True(t, os.IsNotExist(err))
}

func TestLocal_NoListing_Disabled(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "")
	assert.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create storage with NoListing disabled
	stor, err := NewLocal(tmpDir, false, false)
	assert.NoError(t, err)

	_, err = stor.Create(testCtx, "feed1/episode.mp3", bytes.NewBuffer([]byte{1, 2, 3}))
	assert.NoError(t, err)

	// Opening a directory should work when NoListing is disabled
	file, err := stor.Open("/feed1")
	assert.NoError(t, err)
	file.Close()
}

func TestLocalWalkPrefixAndSymlinks(t *testing.T) {
	root := t.TempDir()
	storage, err := NewLocal(root, false, false)
	assert.NoError(t, err)
	for _, name := range []string{"artwork/version/a.jpg", "artwork/version/a.jpg.ready", "other/user.jpg"} {
		_, err = storage.Create(testCtx, name, bytes.NewBufferString("1"))
		assert.NoError(t, err)
	}
	var names []string
	err = storage.Walk(testCtx, "artwork/version", func(name string) error { names = append(names, name); return nil })
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"artwork/version/a.jpg", "artwork/version/a.jpg.ready"}, names)
	assert.NoError(t, storage.Walk(testCtx, "artwork/missing", func(string) error { t.Fatal("unexpected object"); return nil }))
	// Windows runners may lack permission to create symlinks; the prefix test above still runs.
	if err := os.Symlink(filepath.Join(root, "other"), filepath.Join(root, "artwork", "link")); err == nil {
		assert.Error(t, storage.Walk(testCtx, "artwork/link", func(string) error { t.Fatal("followed symlink"); return nil }))
	}
	assert.Error(t, storage.Walk(testCtx, "../outside", func(string) error { return nil }))
}

func TestLocalLinkMediaSafetyAndConflicts(t *testing.T) {
	root := t.TempDir()
	local, err := NewLocal(root, false, false)
	assert.NoError(t, err)
	_, err = local.Create(testCtx, "example/old.mp4", bytes.NewBufferString("video"))
	assert.NoError(t, err)
	assert.NoError(t, local.LinkMedia(testCtx, "example/old.mp4", "example/new.mp4"))
	assert.NoError(t, local.LinkMedia(testCtx, "example/old.mp4", "example/new.mp4"))
	_, err = local.Create(testCtx, "example/conflict.mp4", bytes.NewBufferString("different"))
	assert.NoError(t, err)
	assert.Error(t, local.LinkMedia(testCtx, "example/old.mp4", "example/conflict.mp4"))
	contents, err := os.ReadFile(filepath.Join(root, "example/conflict.mp4"))
	assert.NoError(t, err)
	assert.Equal(t, "different", string(contents))
	assert.Error(t, local.LinkMedia(testCtx, "../outside.mp4", "example/new.mp4"))
	assert.Error(t, local.LinkMedia(testCtx, "example/old.mp4", "../outside.mp4"))
	outside := t.TempDir()
	assert.NoError(t, os.WriteFile(filepath.Join(outside, "video.mp4"), []byte("outside"), 0600))
	assert.NoError(t, os.Symlink(filepath.Join(outside, "video.mp4"), filepath.Join(root, "example/symlink.mp4")))
	assert.Error(t, local.LinkMedia(testCtx, "example/symlink.mp4", "example/unsafe.mp4"))
	assert.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	assert.Error(t, local.LinkMedia(testCtx, "escape/video.mp4", "escape/new.mp4"))
}

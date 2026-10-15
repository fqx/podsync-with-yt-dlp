package fs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// LocalConfig is the storage configuration for local file system
type LocalConfig struct {
	DataDir string `toml:"data_dir"`
}

// Local implements local file storage
type Local struct {
	rootDir      string
	WebUIEnabled bool
	NoListing    bool
}

func NewLocal(rootDir string, webUIEnabled bool, noListing bool) (*Local, error) {
	return &Local{rootDir: rootDir, WebUIEnabled: webUIEnabled, NoListing: noListing}, nil
}

func (l *Local) Open(name string) (http.File, error) {
	if name == "/index.html" && l.WebUIEnabled {
		return os.Open("./html/index.html")
	}
	path := filepath.Join(l.rootDir, name)
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	// If NoListing is enabled, prevent directory listings by returning 404
	if l.NoListing {
		stat, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, err
		}
		if stat.IsDir() {
			file.Close()
			return nil, os.ErrNotExist
		}
	}

	return file, nil
}

func (l *Local) Delete(_ctx context.Context, name string) error {
	path := filepath.Join(l.rootDir, name)
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("failed to delete file %s: %w", path, err)
	}
	return nil
}

func (l *Local) Create(_ctx context.Context, name string, reader io.Reader) (int64, error) {
	var (
		logger = log.WithField("name", name)
		path   = filepath.Join(l.rootDir, name)
	)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return 0, errors.Wrapf(err, "failed to mkdir: %s", path)
	}

	logger.Infof("creating file: %s", path)
	written, err := l.copyFile(reader, path)
	if err != nil {
		return 0, errors.Wrap(err, "failed to copy file")
	}

	logger.Debugf("written %d bytes", written)
	return written, nil
}

func (l *Local) copyFile(source io.Reader, destinationPath string) (int64, error) {
	dest, err := os.Create(destinationPath)
	if err != nil {
		return 0, errors.Wrap(err, "failed to create destination file")
	}

	defer dest.Close()

	written, err := io.Copy(dest, source)
	if err != nil {
		return 0, errors.Wrap(err, "failed to copy data")
	}

	return written, nil
}

func (l *Local) Size(_ctx context.Context, name string) (int64, error) {
	file, err := l.Open(name)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return 0, err
	}

	return stat.Size(), nil
}

// Walk never follows symlinks in or below the requested relative prefix.
func (l *Local) Walk(ctx context.Context, prefix string, visit func(string) error) error {
	prefix = strings.TrimSuffix(prefix, "/")
	if !filepath.IsLocal(prefix) {
		return fmt.Errorf("invalid storage prefix")
	}
	current := l.rootDir
	for _, part := range strings.Split(filepath.FromSlash(prefix), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink storage prefix")
		}
	}
	return filepath.WalkDir(current, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relative, err := filepath.Rel(l.rootDir, name)
		if err != nil {
			return err
		}
		return visit(filepath.ToSlash(relative))
	})
}

// LinkMedia preserves the old URL and atomically adds the canonical name. Both
// names must be in the same directory; rooted operations cannot escape storage.
func (l *Local) LinkMedia(ctx context.Context, source, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsLocal(source) || !filepath.IsLocal(target) || filepath.Dir(source) != filepath.Dir(target) {
		return fmt.Errorf("invalid media link paths")
	}
	root, err := os.OpenRoot(l.rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	directory, err := root.OpenRoot(filepath.Dir(source))
	if err != nil {
		return err
	}
	defer directory.Close()
	sourceName, targetName := filepath.Base(source), filepath.Base(target)
	info, err := directory.Lstat(sourceName)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("media link source is not a regular file")
	}
	err = directory.Link(sourceName, targetName)
	if errors.Is(err, os.ErrExist) {
		destination, statErr := directory.Lstat(targetName)
		if statErr == nil && destination.Mode().IsRegular() && os.SameFile(info, destination) {
			return nil
		}
	}
	return err
}

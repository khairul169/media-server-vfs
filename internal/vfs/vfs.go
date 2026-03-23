// Package vfs implements a virtual filesystem that presents zip/rar archives
// as browseable directories. Files inside archives are extracted on-the-fly
// to a temp cache on first access.
package vfs

import (
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FS is the top-level virtual filesystem. It wraps a real root directory and
// intercepts paths that pass through an archive, transparently mounting the
// archive as a subdirectory.
type FS struct {
	root  string
	cache *Cache
	mu    sync.RWMutex
}

// New creates a new virtual FS rooted at rootDir, using cacheDir for
// on-the-fly extraction temp files.
func New(rootDir, cacheDir string) (*FS, error) {
	rootDir = filepath.Clean(rootDir)
	if _, err := os.Stat(rootDir); err != nil {
		return nil, err
	}
	cache, err := NewCache(cacheDir)
	if err != nil {
		return nil, err
	}
	return &FS{root: rootDir, cache: cache}, nil
}

// Root returns the physical root directory.
func (v *FS) Root() string { return v.root }

// Stat returns FileInfo for a virtual path. The path is relative to root.
// If the path passes through an archive, the archive is treated as a directory.
func (v *FS) Stat(vpath string) (fs.FileInfo, error) {
	vpath = cleanVPath(vpath)

	// Walk the path segments to find where an archive boundary is.
	archivePath, innerPath, found := v.splitAtArchive(vpath)
	if !found {
		// Purely real path.
		return os.Stat(filepath.Join(v.root, vpath))
	}

	if innerPath == "" {
		// Path IS the archive itself — present it as a directory.
		fi, err := os.Stat(archivePath)
		if err != nil {
			return nil, err
		}
		return &archiveDirInfo{fi}, nil
	}

	// Path is inside an archive.
	return v.cache.StatEntry(archivePath, innerPath)
}

// ReadDir returns directory entries for a virtual path.
func (v *FS) ReadDir(vpath string) ([]fs.FileInfo, error) {
	vpath = cleanVPath(vpath)

	archivePath, innerPath, found := v.splitAtArchive(vpath)
	if !found {
		realPath := filepath.Join(v.root, vpath)
		entries, err := os.ReadDir(realPath)
		if err != nil {
			return nil, err
		}
		var infos []fs.FileInfo
		for _, e := range entries {
			fi, err := e.Info()
			if err != nil {
				continue
			}
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if isArchive(e.Name()) {
				fi = &archiveDirInfo{fi}
			}
			infos = append(infos, fi)
		}
		return infos, nil
	}

	// List contents inside an archive.
	return v.cache.ListDir(archivePath, innerPath)
}

// Open opens a file at the virtual path for reading.
// For files inside archives, the file is extracted to cache first.
func (v *FS) Open(vpath string) (io.ReadCloser, error) {
	vpath = cleanVPath(vpath)

	archivePath, innerPath, found := v.splitAtArchive(vpath)
	if !found {
		return os.Open(filepath.Join(v.root, vpath))
	}
	if innerPath == "" {
		return nil, &fs.PathError{Op: "open", Path: vpath, Err: fs.ErrInvalid}
	}

	// Extract the file from the archive to cache, return reader.
	return v.cache.OpenEntry(archivePath, innerPath)
}

// splitAtArchive walks path segments and returns:
//   - archivePath: the real filesystem path to the archive file
//   - innerPath: the remaining path inside the archive (may be empty)
//   - found: true if an archive boundary was crossed
func (v *FS) splitAtArchive(vpath string) (archivePath, innerPath string, found bool) {
	parts := strings.Split(vpath, "/")
	current := v.root

	for i, part := range parts {
		if part == "" {
			continue
		}
		next := filepath.Join(current, part)

		fi, err := os.Stat(next)
		if err != nil {
			// Could be a virtual path inside an already-identified archive;
			// if we haven't found an archive yet, the path simply doesn't exist.
			return "", "", false
		}

		if !fi.IsDir() && isArchive(part) {
			// Found the archive boundary.
			inner := strings.Join(parts[i+1:], "/")
			return next, inner, true
		}
		current = next
	}
	return "", "", false
}

// ---- helpers ----------------------------------------------------------------

func cleanVPath(p string) string {
	p = filepath.ToSlash(filepath.Clean("/" + p))
	p = strings.TrimPrefix(p, "/")
	return p
}

// isArchive returns true for archive extensions we handle.
func isArchive(name string) bool {
	low := strings.ToLower(name)
	return strings.HasSuffix(low, ".zip") || strings.HasSuffix(low, ".rar")
}

// archiveDirInfo is defined in cache.go

// ---- Cache eviction background job -----------------------------------------

// StartEviction runs a goroutine that evicts cache entries older than maxAge.
func (v *FS) StartEviction(maxAge time.Duration, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			n, err := v.cache.Evict(maxAge)
			if err != nil {
				log.Printf("[vfs] cache eviction error: %v", err)
			} else if n > 0 {
				log.Printf("[vfs] evicted %d cache entries older than %v", n, maxAge)
			}
		}
	}()
}

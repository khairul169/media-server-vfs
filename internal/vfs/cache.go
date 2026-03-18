package vfs

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nwaples/rardecode"
)

// Cache manages on-the-fly extraction of archive entries to a temp directory.
// Each extracted file is stored at:
//
//	<cacheDir>/<archive-hash>/<inner-path>
//
// Concurrent requests for the same entry are coalesced — only one goroutine
// does the extraction while others wait.
type Cache struct {
	dir string

	mu      sync.Mutex
	pending map[string]*extractTask // key = cacheKey(archive, inner)
}

type extractTask struct {
	done chan struct{}
	err  error
}

// NewCache creates (or reuses) a cache directory.
func NewCache(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cache dir: %w", err)
	}
	return &Cache{dir: dir, pending: make(map[string]*extractTask)}, nil
}

// OpenEntry returns a ReadCloser for the given innerPath inside archivePath.
// The file is extracted to the cache on first access; subsequent accesses
// read directly from the cached file.
func (c *Cache) OpenEntry(archivePath, innerPath string) (io.ReadCloser, error) {
	dest := c.destPath(archivePath, innerPath)

	// Fast path: already cached.
	if _, err := os.Stat(dest); err == nil {
		// Touch access time for eviction purposes.
		_ = os.Chtimes(dest, time.Now(), time.Now())
		return os.Open(dest)
	}

	// Slow path: extract.
	if err := c.extract(archivePath, innerPath, dest); err != nil {
		return nil, err
	}
	return os.Open(dest)
}

// StatEntry returns FileInfo for innerPath inside the archive.
func (c *Cache) StatEntry(archivePath, innerPath string) (fs.FileInfo, error) {
	low := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(low, ".zip"):
		return c.statZip(archivePath, innerPath)
	case strings.HasSuffix(low, ".rar"):
		return c.statRar(archivePath, innerPath)
	default:
		return nil, fmt.Errorf("unsupported archive: %s", archivePath)
	}
}

// ListDir returns FileInfo for all direct children of dirPath inside the archive.
// dirPath == "" means the root of the archive.
func (c *Cache) ListDir(archivePath, dirPath string) ([]fs.FileInfo, error) {
	low := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(low, ".zip"):
		return c.listZip(archivePath, dirPath)
	case strings.HasSuffix(low, ".rar"):
		return c.listRar(archivePath, dirPath)
	default:
		return nil, fmt.Errorf("unsupported archive: %s", archivePath)
	}
}

// Evict removes cached files not accessed within maxAge. Returns the count.
func (c *Cache) Evict(maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	var count int
	err := filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.ModTime().Before(cutoff) {
			if e := os.Remove(p); e == nil {
				count++
			}
		}
		return nil
	})
	// Clean empty subdirs.
	_ = removeEmptyDirs(c.dir)
	return count, err
}

// ---- extraction -------------------------------------------------------------

func (c *Cache) extract(archivePath, innerPath, dest string) error {
	key := archivePath + "|" + innerPath

	c.mu.Lock()
	if task, ok := c.pending[key]; ok {
		c.mu.Unlock()
		<-task.done
		return task.err
	}
	task := &extractTask{done: make(chan struct{})}
	c.pending[key] = task
	c.mu.Unlock()

	go func() {
		task.err = c.doExtract(archivePath, innerPath, dest)
		if task.err != nil {
			log.Printf("[cache] extract error %s|%s: %v", archivePath, innerPath, task.err)
		}
		close(task.done)

		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	<-task.done
	return task.err
}

func (c *Cache) doExtract(archivePath, innerPath, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	low := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(low, ".zip"):
		return extractZipEntry(archivePath, innerPath, dest)
	case strings.HasSuffix(low, ".rar"):
		return extractRarEntry(archivePath, innerPath, dest)
	default:
		return fmt.Errorf("unsupported archive: %s", archivePath)
	}
}

// ---- ZIP support ------------------------------------------------------------

func extractZipEntry(archivePath, innerPath, dest string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if normZipName(f.Name) == innerPath {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeFile(rc, dest)
		}
	}
	return fmt.Errorf("entry not found in zip: %s", innerPath)
}

func (c *Cache) statZip(archivePath, innerPath string) (fs.FileInfo, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	for _, f := range r.File {
		if normZipName(f.Name) == innerPath {
			return f.FileInfo(), nil
		}
		// Also match as a directory prefix.
		if strings.HasPrefix(normZipName(f.Name)+"/", innerPath+"/") {
			return &syntheticDirInfo{name: filepath.Base(innerPath), mod: f.Modified}, nil
		}
	}
	return nil, &fs.PathError{Op: "stat", Path: innerPath, Err: fs.ErrNotExist}
}

func (c *Cache) listZip(archivePath, dirPath string) ([]fs.FileInfo, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	return listEntriesFromNames(dirPath, func(yield func(name string, isDir bool, size int64, mod time.Time)) {
		for _, f := range r.File {
			n := normZipName(f.Name)
			isDir := f.FileInfo().IsDir()
			yield(n, isDir, int64(f.UncompressedSize64), f.Modified)
		}
	}), nil
}

func normZipName(n string) string {
	n = strings.TrimSuffix(n, "/")
	return n
}

// ---- RAR support ------------------------------------------------------------

func extractRarEntry(archivePath, innerPath, dest string) error {
	rr, err := rardecode.OpenReader(archivePath, "")
	if err != nil {
		return err
	}
	defer rr.Close()

	for {
		hdr, err := rr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if normRarName(hdr.Name) == innerPath {
			return writeFile(rr, dest)
		}
	}
	return fmt.Errorf("entry not found in rar: %s", innerPath)
}

func (c *Cache) statRar(archivePath, innerPath string) (fs.FileInfo, error) {
	rr, err := rardecode.OpenReader(archivePath, "")
	if err != nil {
		return nil, err
	}
	defer rr.Close()

	for {
		hdr, err := rr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		n := normRarName(hdr.Name)
		if n == innerPath {
			return &rarFileInfo{hdr: hdr}, nil
		}
		if strings.HasPrefix(n+"/", innerPath+"/") {
			return &syntheticDirInfo{name: filepath.Base(innerPath), mod: hdr.ModificationTime}, nil
		}
	}
	return nil, &fs.PathError{Op: "stat", Path: innerPath, Err: fs.ErrNotExist}
}

func (c *Cache) listRar(archivePath, dirPath string) ([]fs.FileInfo, error) {
	rr, err := rardecode.OpenReader(archivePath, "")
	if err != nil {
		return nil, err
	}
	defer rr.Close()

	type entry struct {
		name  string
		isDir bool
		size  int64
		mod   time.Time
	}
	var entries []entry
	for {
		hdr, err := rr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{
			name:  normRarName(hdr.Name),
			isDir: hdr.IsDir,
			size:  hdr.UnPackedSize,
			mod:   hdr.ModificationTime,
		})
	}

	return listEntriesFromNames(dirPath, func(yield func(name string, isDir bool, size int64, mod time.Time)) {
		for _, e := range entries {
			yield(e.name, e.isDir, e.size, e.mod)
		}
	}), nil
}

func normRarName(n string) string {
	n = filepath.ToSlash(n)
	n = strings.TrimSuffix(n, "/")
	return n
}

// ---- generic listing helper -------------------------------------------------

// listEntriesFromNames scans a flat list of archive entries and returns the
// direct children of dirPath (like os.ReadDir but for in-memory name sets).
func listEntriesFromNames(
	dirPath string,
	each func(yield func(name string, isDir bool, size int64, mod time.Time)),
) []fs.FileInfo {
	seen := make(map[string]bool)
	var infos []fs.FileInfo

	prefix := dirPath
	if prefix != "" {
		prefix += "/"
	}

	each(func(name string, isDir bool, size int64, mod time.Time) {
		name = filepath.ToSlash(name)
		if !strings.HasPrefix(name, prefix) {
			return
		}
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" {
			return
		}
		parts := strings.SplitN(rel, "/", 2)
		child := parts[0]
		if child == "" || seen[child] {
			return
		}
		seen[child] = true

		childIsDir := len(parts) > 1 || isDir
		if childIsDir {
			infos = append(infos, &syntheticDirInfo{name: child, mod: mod})
		} else {
			infos = append(infos, &syntheticFileInfo{name: child, size: size, mod: mod})
		}
	})
	return infos
}

// ---- I/O helpers ------------------------------------------------------------

func writeFile(r io.Reader, dest string) error {
	f, err := os.CreateTemp(filepath.Dir(dest), ".tmp-")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	_, err = io.Copy(f, r)
	f.Close()
	if err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, dest)
}

func removeEmptyDirs(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return nil
		}
		entries, _ := os.ReadDir(p)
		if len(entries) == 0 {
			os.Remove(p)
		}
		return nil
	})
}

func (c *Cache) destPath(archivePath, innerPath string) string {
	// Use the archive path as a directory name (replace separators).
	archKey := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(archivePath)
	return filepath.Join(c.dir, archKey, filepath.FromSlash(innerPath))
}

// ---- synthetic FileInfo types -----------------------------------------------

type syntheticDirInfo struct {
	name string
	mod  time.Time
}

func (s *syntheticDirInfo) Name() string       { return s.name }
func (s *syntheticDirInfo) Size() int64        { return 0 }
func (s *syntheticDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (s *syntheticDirInfo) ModTime() time.Time { return s.mod }
func (s *syntheticDirInfo) IsDir() bool        { return true }
func (s *syntheticDirInfo) Sys() any           { return nil }

type syntheticFileInfo struct {
	name string
	size int64
	mod  time.Time
}

func (s *syntheticFileInfo) Name() string       { return s.name }
func (s *syntheticFileInfo) Size() int64        { return s.size }
func (s *syntheticFileInfo) Mode() fs.FileMode  { return 0o444 }
func (s *syntheticFileInfo) ModTime() time.Time { return s.mod }
func (s *syntheticFileInfo) IsDir() bool        { return false }
func (s *syntheticFileInfo) Sys() any           { return nil }

type rarFileInfo struct {
	hdr *rardecode.FileHeader
}

func (r *rarFileInfo) Name() string       { return filepath.Base(normRarName(r.hdr.Name)) }
func (r *rarFileInfo) Size() int64        { return r.hdr.UnPackedSize }
func (r *rarFileInfo) Mode() fs.FileMode  { return 0o444 }
func (r *rarFileInfo) ModTime() time.Time { return r.hdr.ModificationTime }
func (r *rarFileInfo) IsDir() bool        { return r.hdr.IsDir }
func (r *rarFileInfo) Sys() any           { return nil }

type archiveDirInfo struct {
	fs.FileInfo
}

func (a *archiveDirInfo) IsDir() bool       { return true }
func (a *archiveDirInfo) Mode() fs.FileMode { return a.FileInfo.Mode() | fs.ModeDir }

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

	"github.com/nwaples/rardecode/v2"
)

// ---- TOC types --------------------------------------------------------------

// tocRecord is one entry in an archive's table of contents.
type tocRecord struct {
	name  string // normalised slash path, no trailing slash
	isDir bool
	size  int64
	mod   time.Time
}

// tocEntry is the cached TOC for a single archive file.
// It is invalidated when the archive's mtime or size changes.
type tocEntry struct {
	mtime   time.Time
	size    int64
	entries []tocRecord
}

// ---- Cache ------------------------------------------------------------------

// Cache manages on-the-fly extraction of archive entries to a temp directory.
// Each extracted file is stored at:
//
//	<cacheDir>/<archive-key>/<inner-path>
//
// TOCs are kept in memory after the first scan so that repeated
// ReadDir / Stat calls never re-read the archive from disk.
// A TOC is invalidated automatically when the archive's mtime or size changes.
type Cache struct {
	dir string

	// coalescing: only one goroutine extracts a given (archive, inner) pair
	extractMu sync.Mutex
	pending   map[string]*extractTask

	// TOC cache — one entry per archive path
	tocMu  sync.RWMutex
	tocMap map[string]*tocEntry
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
	return &Cache{
		dir:     dir,
		pending: make(map[string]*extractTask),
		tocMap:  make(map[string]*tocEntry),
	}, nil
}

// toc returns a valid (possibly cached) TOC for archivePath.
// It re-reads the archive only when mtime or size has changed.
func (c *Cache) toc(archivePath string) (*tocEntry, error) {
	fi, err := os.Stat(archivePath)
	if err != nil {
		return nil, err
	}

	// Fast path: valid cached TOC (read lock).
	c.tocMu.RLock()
	if e, ok := c.tocMap[archivePath]; ok {
		if e.mtime.Equal(fi.ModTime()) && e.size == fi.Size() {
			c.tocMu.RUnlock()
			return e, nil
		}
	}
	c.tocMu.RUnlock()

	// Slow path: build TOC from disk.
	c.tocMu.Lock()
	defer c.tocMu.Unlock()

	// Re-check after acquiring write lock (another goroutine may have built it).
	if e, ok := c.tocMap[archivePath]; ok {
		if e.mtime.Equal(fi.ModTime()) && e.size == fi.Size() {
			return e, nil
		}
	}

	low := strings.ToLower(archivePath)
	var records []tocRecord
	switch {
	case strings.HasSuffix(low, ".zip"):
		records, err = readZipTOC(archivePath)
	case strings.HasSuffix(low, ".rar"):
		records, err = readRarTOC(archivePath)
	default:
		return nil, fmt.Errorf("unsupported archive: %s", archivePath)
	}
	if err != nil {
		return nil, err
	}

	entry := &tocEntry{
		mtime:   fi.ModTime(),
		size:    fi.Size(),
		entries: records,
	}
	c.tocMap[archivePath] = entry
	log.Printf("[cache] TOC built for %s (%d entries)", archivePath, len(records))
	return entry, nil
}

// ---- public API -------------------------------------------------------------

// OpenEntry returns a ReadCloser for innerPath inside archivePath.
// The file is extracted to the cache on first access; subsequent accesses
// read directly from the cached file.
func (c *Cache) OpenEntry(archivePath, innerPath string) (io.ReadCloser, error) {
	dest := c.destPath(archivePath, innerPath)

	// Fast path: already cached.
	if _, err := os.Stat(dest); err == nil {
		_ = os.Chtimes(dest, time.Now(), time.Now())
		return os.Open(dest)
	}

	// Slow path: extract then open.
	if err := c.extract(archivePath, innerPath, dest); err != nil {
		return nil, err
	}
	return os.Open(dest)
}

// StatEntry returns FileInfo for innerPath inside the archive.
// Uses the in-memory TOC — no disk I/O after the first call per archive.
func (c *Cache) StatEntry(archivePath, innerPath string) (fs.FileInfo, error) {
	toc, err := c.toc(archivePath)
	if err != nil {
		return nil, err
	}

	// Exact match first.
	for i := range toc.entries {
		r := &toc.entries[i]
		if r.name == innerPath {
			if r.isDir {
				return &syntheticDirInfo{name: filepath.Base(r.name), mod: r.mod}, nil
			}
			return &syntheticFileInfo{name: filepath.Base(r.name), size: r.size, mod: r.mod}, nil
		}
	}
	// Virtual directory implied by a child entry.
	prefix := innerPath + "/"
	for i := range toc.entries {
		if strings.HasPrefix(toc.entries[i].name, prefix) {
			return &syntheticDirInfo{name: filepath.Base(innerPath), mod: toc.entries[i].mod}, nil
		}
	}
	return nil, &fs.PathError{Op: "stat", Path: innerPath, Err: fs.ErrNotExist}
}

// ListDir returns FileInfo for direct children of dirPath inside the archive.
// dirPath == "" means the root of the archive.
// Uses the in-memory TOC — no disk I/O after the first call per archive.
func (c *Cache) ListDir(archivePath, dirPath string) ([]fs.FileInfo, error) {
	toc, err := c.toc(archivePath)
	if err != nil {
		return nil, err
	}
	return listEntriesFromRecords(dirPath, toc.entries), nil
}

// Evict removes cached extracted files not accessed within maxAge.
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
	_ = removeEmptyDirs(c.dir)
	return count, err
}

// ---- TOC readers (called once per archive, then cached) ---------------------

func readZipTOC(archivePath string) ([]tocRecord, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	records := make([]tocRecord, 0, len(r.File))
	for _, f := range r.File {
		records = append(records, tocRecord{
			name:  normZipName(f.Name),
			isDir: f.FileInfo().IsDir(),
			size:  int64(f.UncompressedSize64),
			mod:   f.Modified,
		})
	}
	return records, nil
}

func readRarTOC(archivePath string) ([]tocRecord, error) {
	files, err := rardecode.List(archivePath)
	if err != nil {
		return nil, fmt.Errorf("rardecode.List: %w", err)
	}
	records := make([]tocRecord, 0, len(files))
	for _, f := range files {
		records = append(records, tocRecord{
			name:  normRarName(f.Name),
			isDir: f.IsDir,
			size:  f.UnPackedSize,
			mod:   f.ModificationTime,
		})
	}
	return records, nil
}

// ---- extraction (on first file access) --------------------------------------

func (c *Cache) extract(archivePath, innerPath, dest string) error {
	key := archivePath + "|" + innerPath

	c.extractMu.Lock()
	if task, ok := c.pending[key]; ok {
		c.extractMu.Unlock()
		<-task.done
		return task.err
	}
	task := &extractTask{done: make(chan struct{})}
	c.pending[key] = task
	c.extractMu.Unlock()

	go func() {
		task.err = c.doExtract(archivePath, innerPath, dest)
		if task.err != nil {
			log.Printf("[cache] extract error %s|%s: %v", archivePath, innerPath, task.err)
		}
		close(task.done)

		c.extractMu.Lock()
		delete(c.pending, key)
		c.extractMu.Unlock()
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

// ---- ZIP extraction ---------------------------------------------------------

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

func normZipName(n string) string {
	return strings.TrimSuffix(n, "/")
}

// ---- RAR extraction ---------------------------------------------------------

// extractRarEntry uses rardecode.OpenFS (v2) to open the archive as an fs.FS
// and stream only the requested entry to dest. No full-archive scan needed.
func extractRarEntry(archivePath, innerPath, dest string) error {
	rfs, err := rardecode.OpenFS(archivePath)
	if err != nil {
		return fmt.Errorf("rardecode.OpenFS: %w", err)
	}

	src, err := rfs.Open(innerPath)
	if err != nil {
		return fmt.Errorf("rfs.Open %s: %w", innerPath, err)
	}
	defer src.Close()

	return writeFile(src, dest)
}

func normRarName(n string) string {
	n = filepath.ToSlash(n)
	return strings.TrimSuffix(n, "/")
}

// ---- listing helper ---------------------------------------------------------

// listEntriesFromRecords returns direct children of dirPath from a flat TOC.
func listEntriesFromRecords(dirPath string, records []tocRecord) []fs.FileInfo {
	seen := make(map[string]bool)
	var infos []fs.FileInfo

	prefix := dirPath
	if prefix != "" {
		prefix += "/"
	}

	for i := range records {
		r := &records[i]
		name := r.name
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" {
			continue
		}
		parts := strings.SplitN(rel, "/", 2)
		child := parts[0]
		if child == "" || seen[child] {
			continue
		}
		seen[child] = true

		childIsDir := len(parts) > 1 || r.isDir
		if childIsDir {
			infos = append(infos, &syntheticDirInfo{name: child, mod: r.mod})
		} else {
			infos = append(infos, &syntheticFileInfo{name: child, size: r.size, mod: r.mod})
		}
	}
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

// archiveDirInfo wraps a real fs.FileInfo and makes the archive appear as a
// directory to the SMB/WebDAV layer.
type archiveDirInfo struct {
	fs.FileInfo
}

func (a *archiveDirInfo) IsDir() bool       { return true }
func (a *archiveDirInfo) Mode() fs.FileMode { return a.FileInfo.Mode() | fs.ModeDir }

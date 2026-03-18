package server

// WebDAV server backed by the virtual filesystem.
// WebDAV is the pragmatic choice here: it is natively supported by
//   • Windows  (Map Network Drive → https://host/share)
//   • macOS    (Finder → Go → Connect to Server → http://host:PORT)
//   • Linux    (davfs2 / Nautilus / Thunar)
//   • VLC      (Open Network Stream or Media → Open Folder)
//   • Kodi / Jellyfin / Plex
//
// You can add a real SMB2 library on top of the vfs.FS later; the VFS
// interface stays the same.

import (
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rul.sh/media-server-vfs/internal/vfs"
)

// WebDAVServer serves the VFS as a WebDAV endpoint.
type WebDAVServer struct {
	vfs      *vfs.FS
	prefix   string // URL prefix, e.g. "/media"
	user     string // empty = no auth
	password string
}

// NewWebDAV creates a WebDAV server. prefix is the URL mount point (e.g. "/media").
func NewWebDAV(v *vfs.FS, prefix, user, password string) *WebDAVServer {
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return &WebDAVServer{vfs: v, prefix: prefix, user: user, password: password}
}

// Handler returns an http.Handler for use with http.ListenAndServe.
func (w *WebDAVServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.handle)
	if w.user != "" {
		return basicAuth(w.user, w.password, mux)
	}
	return mux
}

// ListenAndServe starts the HTTP/WebDAV server.
func (w *WebDAVServer) ListenAndServe(addr string) error {
	log.Printf("[webdav] listening on http://%s%s", addr, w.prefix)
	return http.ListenAndServe(addr, w.Handler())
}

// ---- main handler -----------------------------------------------------------

func (w *WebDAVServer) handle(rw http.ResponseWriter, r *http.Request) {
	log.Printf("[webdav] %s %s", r.Method, r.URL.Path)

	// Strip prefix to get the VFS-relative path.
	vpath := strings.TrimPrefix(r.URL.Path, w.prefix)
	vpath = strings.TrimPrefix(vpath, "/")

	switch r.Method {
	case "OPTIONS":
		w.handleOptions(rw, r)
	case "PROPFIND":
		w.handlePropfind(rw, r, vpath)
	case "GET", "HEAD":
		w.handleGet(rw, r, vpath)
	default:
		// We are read-only.
		http.Error(rw, "read-only share", http.StatusMethodNotAllowed)
	}
}

func (w *WebDAVServer) handleOptions(rw http.ResponseWriter, _ *http.Request) {
	rw.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND")
	rw.Header().Set("DAV", "1")
	rw.Header().Set("MS-Author-Via", "DAV")
	rw.WriteHeader(http.StatusOK)
}

// ---- PROPFIND ---------------------------------------------------------------

func (w *WebDAVServer) handlePropfind(rw http.ResponseWriter, r *http.Request, vpath string) {
	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "1"
	}

	fi, err := w.vfs.Stat(vpath)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusNotFound)
		return
	}

	var responses []davResponse

	// Add the resource itself.
	responses = append(responses, w.fileInfoToResponse(vpath, fi))

	// If directory and depth != 0, add children.
	if fi.IsDir() && depth != "0" {
		children, err := w.vfs.ReadDir(vpath)
		if err == nil {
			for _, c := range children {
				childPath := path.Join(vpath, c.Name())
				responses = append(responses, w.fileInfoToResponse(childPath, c))
			}
		}
	}

	rw.Header().Set("Content-Type", "application/xml; charset=utf-8")
	rw.WriteHeader(207) // Multi-Status

	enc := xml.NewEncoder(rw)
	enc.Indent("", "  ")
	out := struct {
		XMLName   xml.Name      `xml:"D:multistatus"`
		XMLNS     string        `xml:"xmlns:D,attr"`
		Responses []davResponse `xml:"D:response"`
	}{
		XMLNS:     "DAV:",
		Responses: responses,
	}
	_ = enc.Encode(out)
}

func (w *WebDAVServer) fileInfoToResponse(vpath string, fi fs.FileInfo) davResponse {
	href := w.prefix + "/" + vpath
	if fi.IsDir() && !strings.HasSuffix(href, "/") {
		href += "/"
	}
	href = strings.ReplaceAll(href, "//", "/")

	props := davPropStat{
		Status: "HTTP/1.1 200 OK",
		Prop: davProp{
			DisplayName:      fi.Name(),
			GetLastModified:  fi.ModTime().UTC().Format(http.TimeFormat),
			GetContentLength: strconv.FormatInt(fi.Size(), 10),
		},
	}
	if fi.IsDir() {
		props.Prop.ResourceType = &davResourceType{Collection: &davCollection{}}
	} else {
		props.Prop.GetContentType = mimeByExt(fi.Name())
	}

	return davResponse{
		Href:      href,
		PropStats: []davPropStat{props},
	}
}

// ---- GET/HEAD ---------------------------------------------------------------

func (w *WebDAVServer) handleGet(rw http.ResponseWriter, r *http.Request, vpath string) {
	fi, err := w.vfs.Stat(vpath)
	if err != nil {
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}
	if fi.IsDir() {
		// Return a simple HTML listing for browsers.
		w.serveHTMLListing(rw, r, vpath)
		return
	}

	rc, err := w.vfs.Open(vpath)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	rw.Header().Set("Content-Type", mimeByExt(fi.Name()))
	rw.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	rw.Header().Set("Last-Modified", fi.ModTime().UTC().Format(http.TimeFormat))
	rw.Header().Set("Accept-Ranges", "bytes")

	if r.Method == "HEAD" {
		rw.WriteHeader(http.StatusOK)
		return
	}

	// Support range requests so media players can seek without downloading everything.
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		w.serveRange(rw, r, rc, fi, rangeHeader)
		return
	}

	rw.WriteHeader(http.StatusOK)
	_, _ = io.Copy(rw, rc)
}

func (w *WebDAVServer) serveRange(
	rw http.ResponseWriter,
	_ *http.Request,
	rc io.ReadCloser,
	fi fs.FileInfo,
	rangeHeader string,
) {
	// Parse "bytes=start-end"
	rangeHeader = strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.SplitN(rangeHeader, "-", 2)
	if len(parts) != 2 {
		http.Error(rw, "bad range", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	size := fi.Size()
	var start, end int64
	var err error

	if parts[0] == "" {
		// suffix range: bytes=-N
		n, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || n <= 0 {
			http.Error(rw, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start = size - n
		end = size - 1
	} else {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			http.Error(rw, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if parts[1] == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				http.Error(rw, "bad range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
		}
	}

	if start < 0 || end >= size || start > end {
		http.Error(rw, "range out of bounds", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// For range support we need an io.ReadSeeker.
	// The VFS Open returns a ReadCloser that may wrap a real *os.File (for
	// cached entries) or a plain reader (for in-progress extractions).
	// We attempt a type assertion; if the underlying type is seekable we use it,
	// otherwise we skip to the offset manually.
	type readSeeker interface {
		io.ReadSeeker
	}

	length := end - start + 1
	rw.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	rw.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	rw.Header().Set("Content-Type", mimeByExt(fi.Name()))
	rw.WriteHeader(http.StatusPartialContent)

	if rs, ok := rc.(readSeeker); ok {
		_, _ = rs.Seek(start, io.SeekStart)
		_, _ = io.CopyN(rw, rs, length)
	} else {
		_, _ = io.CopyN(io.Discard, rc, start) // skip
		_, _ = io.CopyN(rw, rc, length)
	}
}

func (w *WebDAVServer) serveHTMLListing(rw http.ResponseWriter, _ *http.Request, vpath string) {
	entries, err := w.vfs.ReadDir(vpath)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(rw, `<!DOCTYPE html><html><head><meta charset=utf-8>
<title>Index of /%s</title>
<style>
body{font-family:monospace;padding:1em}
a{text-decoration:none}
tr:hover{background:#f4f4f4}
td{padding:2px 12px}
</style>
</head><body>
<h2>📁 /%s</h2><table>
<tr><th>Name</th><th>Size</th><th>Modified</th></tr>
`, vpath, vpath)

	if vpath != "" {
		parent := path.Dir("/" + vpath)
		fmt.Fprintf(rw, `<tr><td><a href="%s%s">⬆ ..</a></td><td></td><td></td></tr>`,
			w.prefix, parent)
	}

	for _, fi := range entries {
		name := fi.Name()
		href := w.prefix + "/" + path.Join(vpath, name)
		icon := "📄"
		sizeStr := humanSize(fi.Size())
		if fi.IsDir() {
			icon = "📁"
			href += "/"
			sizeStr = "-"
		}
		fmt.Fprintf(rw, `<tr><td>%s <a href="%s">%s</a></td><td>%s</td><td>%s</td></tr>`,
			icon, href, name, sizeStr, fi.ModTime().Format("2006-01-02 15:04"))
	}
	fmt.Fprint(rw, `</table></body></html>`)
}

// ---- helpers ----------------------------------------------------------------

func humanSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func mimeByExt(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mkv":
		return "video/x-matroska"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".flv":
		return "video/x-flv"
	case ".webm":
		return "video/webm"
	case ".ts", ".m2ts":
		return "video/mp2t"
	case ".mp3":
		return "audio/mpeg"
	case ".flac":
		return "audio/flac"
	case ".ogg":
		return "audio/ogg"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".srt":
		return "text/plain"
	case ".nfo":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

func basicAuth(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			rw.Header().Set("WWW-Authenticate", `Basic realm="sambafs"`)
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(rw, r)
	})
}

// ---- DAV XML types ----------------------------------------------------------

type davResponse struct {
	XMLName   xml.Name      `xml:"D:response"`
	Href      string        `xml:"D:href"`
	PropStats []davPropStat `xml:"D:propstat"`
}

type davPropStat struct {
	XMLName xml.Name `xml:"D:propstat"`
	Prop    davProp  `xml:"D:prop"`
	Status  string   `xml:"D:status"`
}

type davProp struct {
	DisplayName      string           `xml:"D:displayname,omitempty"`
	ResourceType     *davResourceType `xml:"D:resourcetype,omitempty"`
	GetContentLength string           `xml:"D:getcontentlength,omitempty"`
	GetContentType   string           `xml:"D:getcontenttype,omitempty"`
	GetLastModified  string           `xml:"D:getlastmodified,omitempty"`
}

type davResourceType struct {
	Collection *davCollection `xml:"D:collection"`
}

type davCollection struct{}

// keep time import used
var _ = time.Now

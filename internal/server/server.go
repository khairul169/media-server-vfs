// Package smb provides the WebDAV server backed by the virtual filesystem.
// The WebDAV implementation is in server_webdav.go.
package server

import (
	"rul.sh/media-server-vfs/internal/vfs"
)

// Config holds server settings.
type Config struct {
	Listen    string
	ShareName string
	User      string
	Password  string
}

// Server placeholder for future native SMB2 support.
type Server struct {
	cfg Config
	vfs *vfs.FS
}

// New creates a Server.
func New(cfg Config, v *vfs.FS) *Server {
	return &Server{cfg: cfg, vfs: v}
}

// func (s *Server) handleConn(conn net.Conn) {
// 	defer conn.Close()
// 	log.Printf("[smb] SMB2 not implemented; use WebDAV. conn from %s", conn.RemoteAddr())
// }

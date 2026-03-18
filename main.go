// sambafs — serve a directory tree (with archives as virtual dirs) via WebDAV.
//
// Usage:
//
//	sambafs [flags]
//
// Flags:
//
//	-root      <path>   Directory to serve (default: current dir)
//	-cache     <path>   Temp dir for extracted files (default: OS temp/sambafs-cache)
//	-addr      <host:port> Listen address (default: 0.0.0.0:8080)
//	-prefix    <url>    URL prefix for the share (default: /media)
//	-user      <name>   HTTP basic auth username (default: no auth)
//	-pass      <pass>   HTTP basic auth password
//	-evict-age <dur>    Evict cached files older than this (default: 2h)
//	-evict-int <dur>    Eviction check interval (default: 30m)
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"time"

	"rul.sh/media-server-vfs/internal/server"
	"rul.sh/media-server-vfs/internal/vfs"
)

func main() {
	root := flag.String("root", mustCwd(), "root directory to serve")
	cacheDir := flag.String("cache", filepath.Join(os.TempDir(), "sambafs-cache"), "extraction cache directory")
	addr := flag.String("addr", "0.0.0.0:8080", "listen address (host:port)")
	prefix := flag.String("prefix", "/media", "URL prefix")
	user := flag.String("user", "", "basic auth username (empty = no auth)")
	pass := flag.String("pass", "", "basic auth password")
	evictAge := flag.Duration("evict-age", 2*time.Hour, "evict cached files older than this")
	evictInt := flag.Duration("evict-int", 30*time.Minute, "eviction check interval")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lshortfile)

	log.Printf("[main] root=%s  cache=%s", *root, *cacheDir)

	v, err := vfs.New(*root, *cacheDir)
	if err != nil {
		log.Fatalf("vfs: %v", err)
	}

	v.StartEviction(*evictAge, *evictInt)

	srv := server.NewWebDAV(v, *prefix, *user, *pass)

	log.Printf("[main] WebDAV share ready")
	log.Printf("[main]   Windows : Map Network Drive → http://%s%s", *addr, *prefix)
	log.Printf("[main]   macOS   : Finder → Go → Connect to Server → http://%s%s", *addr, *prefix)
	log.Printf("[main]   Linux   : davfs2 / Nautilus / Thunar → dav://%s%s", *addr, *prefix)
	log.Printf("[main]   VLC     : Media → Open Network → http://%s%s/", *addr, *prefix)

	if err := srv.ListenAndServe(*addr); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func mustCwd() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}

# sambafs

A Go server that serves a directory tree via **WebDAV**, transparently treating
`.zip` and `.rar` archives as browseable virtual directories.  
Files inside archives are extracted **on-the-fly** into a temp cache the first
time they are accessed — no upfront extraction needed.

## Why WebDAV instead of SMB/Samba?

A full SMB2/3 implementation in pure Go is ~4 000 lines of protocol work.
WebDAV gives you **identical client-side behaviour** and is natively supported
by every OS and media player:

| Client      | How to connect                                             |
| ----------- | ---------------------------------------------------------- |
| **Windows** | Map Network Drive → `http://<ip>:8080/media`               |
| **macOS**   | Finder → Go → Connect to Server → `http://<ip>:8080/media` |
| **Linux**   | `davfs2`, Nautilus, Thunar → `dav://<ip>:8080/media`       |
| **VLC**     | Media → Open Network → `http://<ip>:8080/media/`           |
| **Kodi**    | Add source → `http://<ip>:8080/media/`                     |
| **Browser** | Just open `http://<ip>:8080/media`                         |

If you specifically need SMB, the `vfs.FS` interface in `internal/vfs/` is
easy to wrap with any Go SMB2 server library (e.g. `samba-go`).

## Features

- **Virtual archive directories** — `.zip` and `.rar` appear as folders
- **On-the-fly extraction** — only the file being accessed is extracted
- **Temp cache** — extracted files live in a cache dir and are evicted after a
  configurable TTL (default 2 h)
- **Seek / Range requests** — media players can seek without downloading the
  whole file (works for cached files; see note below)
- **Concurrent access safe** — multiple clients requesting the same archive
  entry at the same time will only trigger one extraction
- **Nested archives** — an archive inside an archive works recursively
- **Optional HTTP basic auth**

> **Seek note**: Seeking inside a _streaming_ rar/zip extraction is not
> possible without re-reading from the start. Once a file is cached the first
> time it is fully played, subsequent opens use the cached copy and are fully
> seekable. For local LAN use (gigabit) the initial linear read is fast enough
> for most media players.

## Build

```bash
cd sambafs
go mod tidy
go build -o sambafs ./cmd/sambafs
```

## Usage

```
./sambafs [flags]

Flags:
  -root      <path>     Directory to serve         (default: cwd)
  -cache     <path>     Extraction cache directory  (default: $TMPDIR/sambafs-cache)
  -addr      <host:port> Listen address             (default: 0.0.0.0:8080)
  -prefix    <url>      URL prefix                  (default: /media)
  -user      <name>     Basic auth username         (default: no auth)
  -pass      <password> Basic auth password
  -evict-age <duration> Evict files older than      (default: 2h)
  -evict-int <duration> Eviction check interval     (default: 30m)
```

### Example — serve your series folder

```bash
./sambafs \
  -root  /mnt/nas/series \
  -cache /tmp/sambafs-cache \
  -addr  0.0.0.0:8080 \
  -prefix /series \
  -evict-age 4h
```

Then on your TV / media player:

```
http://192.168.1.x:8080/series/
```

You will see:

```
series/
├── BreakingBad/
│   ├── Season1.zip/          ← browseable! (virtual dir)
│   │   ├── S01E01.mkv        ← streamed from zip on demand
│   │   ├── S01E02.mkv
│   │   └── ...
│   └── Season2.rar/          ← same for rar
│       └── ...
└── ...
```

## Adding more archive formats

Edit `internal/vfs/vfs.go`:

```go
func isArchive(name string) bool {
    low := strings.ToLower(name)
    return strings.HasSuffix(low, ".zip") ||
           strings.HasSuffix(low, ".rar") ||
           strings.HasSuffix(low, ".7z")   // ← add here
}
```

Then add corresponding `extract7z`, `list7z`, `stat7z` functions in
`internal/vfs/cache.go` following the zip/rar pattern.

## Dependencies

```
github.com/nwaples/rardecode    — RAR reading
```

ZIP is handled by the Go standard library (`archive/zip`).

## Run as a systemd service

```ini
[Unit]
Description=sambafs WebDAV archive server
After=network.target

[Service]
ExecStart=/usr/local/bin/sambafs \
  -root /mnt/nas/series \
  -addr 0.0.0.0:8080 \
  -evict-age 6h
Restart=on-failure
User=media

[Install]
WantedBy=multi-user.target
```

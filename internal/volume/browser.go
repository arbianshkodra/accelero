// Package volume implements the read-only volume file browser that
// powers GET /api/v1/volumes/{name}/browse.
//
// Docker volumes are managed by the daemon: on Linux they live under
// /var/lib/docker/volumes/<name>/_data, but on Docker Desktop that
// path is inside a hidden VM and unreachable from the host process.
// Asking the daemon to read on our behalf is the only portable way to
// look at volume contents.
//
// The implementation spawns a tiny ephemeral "helper" container with
// the volume mounted read-only at /volume, calls CopyFromContainer to
// extract a tar stream of the requested path, parses the headers, and
// removes the helper. Contents of a single file are extracted straight
// from the tar payload. The helper image defaults to busybox:stable
// (~2MB, has everything we need) and is auto-pulled if missing.
//
// All of this is hidden behind the Browser interface so the HTTP
// handler doesn't need to know Docker APIs, and tests can drop in a
// fake without a live daemon.
package volume

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

// Browser is the narrow surface the HTTP handler uses. The concrete
// DockerBrowser below manages the helper container lifecycle.
type Browser interface {
	ListPath(ctx context.Context, volumeName, subPath string) ([]FileEntry, error)
	ReadFile(ctx context.Context, volumeName, subPath string, maxBytes int64) (ReadResult, error)
	// WriteFile overwrites (or creates) a file at subPath inside the
	// volume. mode is the POSIX file mode (defaults to 0644 when 0 is
	// passed). Parent directories are created as needed.  Implementations
	// mount the helper read-write for this call specifically.
	WriteFile(ctx context.Context, volumeName, subPath string, mode uint32, content io.Reader, size int64) error
}

// FileEntry is one row in a directory listing.
type FileEntry struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`       // absolute within the volume, e.g. "/config/app.yml"
	IsDir     bool      `json:"is_dir"`
	SizeBytes int64     `json:"size_bytes"`
	Mode      string    `json:"mode"`       // like "-rw-r--r--"
	ModTime   time.Time `json:"mod_time"`
	LinkTarget string   `json:"link_target,omitempty"`
}

// ReadResult carries the raw file bytes plus enough metadata for the
// handler to set Content-Length and a sensible Content-Type.
type ReadResult struct {
	Content    io.ReadCloser
	Name       string
	Path       string
	SizeBytes  int64
	Mode       string
	ModTime    time.Time
}

// DockerBrowser is the production Browser backed by a live Docker daemon.
type DockerBrowser struct {
	cli         *client.Client
	helperImage string

	// maxEntriesPerList caps how many directory entries we'll emit
	// before truncating. Guards against accidentally streaming a
	// directory with 10M files. Callers can filter client-side if
	// they need fewer; they cannot get more in a single call.
	maxEntriesPerList int
}

// NewDockerBrowser constructs a Browser backed by the given Docker
// client. The helper image defaults to busybox:stable; the caller can
// override via the env-config field (see cmd/main.go).
func NewDockerBrowser(cli *client.Client, helperImage string) *DockerBrowser {
	if helperImage == "" {
		helperImage = "busybox:stable"
	}
	return &DockerBrowser{
		cli:               cli,
		helperImage:       helperImage,
		maxEntriesPerList: 10000,
	}
}

// ListPath returns the direct children of subPath inside the volume.
// Recursion stops at the requested directory — listing a huge tree
// only returns one level. subPath is treated as absolute within the
// volume; passing "" or "/" lists the volume root.
func (b *DockerBrowser) ListPath(ctx context.Context, volumeName, subPath string) ([]FileEntry, error) {
	abs, err := cleanSubPath(subPath)
	if err != nil {
		return nil, err
	}

	id, err := b.createHelper(ctx, volumeName)
	if err != nil {
		return nil, err
	}
	defer b.removeHelper(ctx, id)

	sourcePath := path.Join("/volume", abs)
	res, err := b.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{
		SourcePath: sourcePath,
	})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("path %q not found in volume %q", abs, volumeName)
		}
		return nil, fmt.Errorf("copy from helper: %w", err)
	}
	defer res.Content.Close()

	// Docker's CopyFromContainer always tars entries under the basename
	// of the *source path* (not the volume-relative path). For abs="/"
	// the source is "/volume" so the prefix is "volume"; for abs="/config"
	// the source is "/volume/config" so the prefix is "config". Strip
	// whichever it is to recover volume-relative names.
	rootName := path.Base(sourcePath)

	entries, err := b.parseListing(res.Content, abs, rootName)
	if err != nil {
		return nil, err
	}

	return entries, nil
}

// ReadFile extracts a single file from the volume. maxBytes caps the
// size returned — files larger than that are refused rather than
// silently truncated; callers set the limit based on operator policy.
func (b *DockerBrowser) ReadFile(ctx context.Context, volumeName, subPath string, maxBytes int64) (ReadResult, error) {
	abs, err := cleanSubPath(subPath)
	if err != nil {
		return ReadResult{}, err
	}

	id, err := b.createHelper(ctx, volumeName)
	if err != nil {
		return ReadResult{}, err
	}

	res, err := b.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{
		SourcePath: path.Join("/volume", abs),
	})
	if err != nil {
		b.removeHelper(ctx, id)
		if cerrdefs.IsNotFound(err) {
			return ReadResult{}, fmt.Errorf("path %q not found in volume %q", abs, volumeName)
		}
		return ReadResult{}, fmt.Errorf("copy from helper: %w", err)
	}

	// Find the matching file inside the single-entry tar that
	// CopyFromContainer emits when SourcePath points at a regular file.
	// On directories the daemon also returns a tar (with headers for
	// every child), which is a useful refusal-signal for callers that
	// asked to download what turned out to be a directory.
	tr := tar.NewReader(res.Content)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			res.Content.Close()
			b.removeHelper(ctx, id)
			return ReadResult{}, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			// The requested path turned out to be a directory. Close and
			// surface a helpful error — the handler turns this into 400.
			continue
		}
		if hdr.Size > maxBytes {
			res.Content.Close()
			b.removeHelper(ctx, id)
			return ReadResult{}, fmt.Errorf("file %q is %d bytes, exceeds maximum of %d", abs, hdr.Size, maxBytes)
		}
		// Wrap so the body read-back also tears the helper down.
		wrapped := &readerWithTeardown{
			reader: io.LimitReader(tr, hdr.Size),
			onClose: func() {
				res.Content.Close()
				b.removeHelper(ctx, id)
			},
		}
		return ReadResult{
			Content:   wrapped,
			Name:      hdr.Name,
			Path:      abs,
			SizeBytes: hdr.Size,
			Mode:      fs.FileMode(hdr.Mode).String(),
			ModTime:   hdr.ModTime,
		}, nil
	}

	res.Content.Close()
	b.removeHelper(ctx, id)
	return ReadResult{}, fmt.Errorf("path %q is a directory, not a file", abs)
}

// WriteFile creates or overwrites a file inside the volume. The caller
// supplies the bytes + their size; implementations stream the payload
// into the container without buffering the whole thing in memory (so
// large writes up to the configured cap don't OOM accelero).
//
// Parent directories are created as needed — Docker's CopyToContainer
// extracts our tar into DestinationPath, so we include TypeDir entries
// for each parent the tar references.
//
// Refuses to write to the volume root (subPath == "/"): the destination
// has to be a file, not a directory, and overwriting the whole volume
// with one byte-stream isn't a sensible operation.
func (b *DockerBrowser) WriteFile(ctx context.Context, volumeName, subPath string, mode uint32, content io.Reader, size int64) error {
	abs, err := cleanSubPath(subPath)
	if err != nil {
		return err
	}
	if abs == "/" || strings.HasSuffix(abs, "/") {
		return fmt.Errorf("subPath must point at a file, not a directory")
	}
	if mode == 0 {
		mode = 0o644
	}

	id, err := b.createHelperRW(ctx, volumeName)
	if err != nil {
		return err
	}
	defer b.removeHelper(ctx, id)

	// Build a tar with directory entries for each parent (so missing
	// intermediate dirs get created during extraction) plus the file
	// payload. DestinationPath=/volume means names in the tar are
	// volume-relative.
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- writeVolumeTar(pw, abs, mode, content, size)
	}()

	if _, err := b.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{
		DestinationPath: "/volume",
		Content:         pr,
	}); err != nil {
		pr.CloseWithError(err) // unblock the writer if it's still pushing
		<-done
		return fmt.Errorf("copy to helper: %w", err)
	}

	if err := <-done; err != nil {
		return fmt.Errorf("tar stream: %w", err)
	}
	return nil
}

// writeVolumeTar emits a tar stream suitable for CopyToContainer:
// TypeDir entries for every parent path component (so Docker's
// extractor creates them), followed by a single TypeReg entry for
// the file with the caller's content.
//
// Parent dirs are emitted with mode 0755 — enough for the daemon to
// cd in, which is all we need for the file to land. If the dir already
// exists, Docker extraction is a no-op for the TypeDir entry; if it
// doesn't, it's created. Either way the file is placed.
func writeVolumeTar(w *io.PipeWriter, abs string, mode uint32, content io.Reader, size int64) error {
	tw := tar.NewWriter(w)
	defer func() {
		_ = tw.Close()
		_ = w.Close()
	}()

	// Emit every parent dir as TypeDir so the extractor can create
	// any that don't exist yet. path.Split leaves trailing slashes on
	// directories, strip before splitting.
	parts := strings.Split(strings.TrimPrefix(path.Dir(abs), "/"), "/")
	var accum string
	for _, p := range parts {
		if p == "" {
			continue
		}
		if accum == "" {
			accum = p
		} else {
			accum = accum + "/" + p
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     accum + "/",
			Typeflag: tar.TypeDir,
			Mode:     0o755,
			ModTime:  time.Now(),
		}); err != nil {
			return err
		}
	}

	// The file itself. Tar names are relative to DestinationPath, so
	// strip the leading slash off abs.
	if err := tw.WriteHeader(&tar.Header{
		Name:     strings.TrimPrefix(abs, "/"),
		Typeflag: tar.TypeReg,
		Mode:     int64(mode),
		Size:     size,
		ModTime:  time.Now(),
	}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, content); err != nil {
		return err
	}
	return nil
}

// parseListing walks the tar headers and keeps only direct children of
// the requested directory. Nested entries are ignored — the caller
// explicitly asked for one level.
func (b *DockerBrowser) parseListing(r io.Reader, requestedDir, rootName string) ([]FileEntry, error) {
	tr := tar.NewReader(r)
	var out []FileEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if len(out) >= b.maxEntriesPerList {
			// Silently truncate; listing a 10k+ entry directory from a
			// browser UI is always a bug on the caller's side.
			break
		}

		// CopyFromContainer prefixes entries with the basename of the
		// source path. Strip it so we're comparing within the dir.
		rel := hdr.Name
		if rootName != "" {
			trimmed := strings.TrimPrefix(rel, rootName)
			trimmed = strings.TrimPrefix(trimmed, "/")
			rel = trimmed
		}
		if rel == "" {
			// Header for the directory itself — skip, it's not a child.
			continue
		}
		// Direct children have no '/' after the first segment.
		// Skip nested files so we only emit one level.
		if strings.Contains(strings.TrimSuffix(rel, "/"), "/") {
			continue
		}

		isDir := hdr.Typeflag == tar.TypeDir
		name := strings.TrimSuffix(rel, "/")
		entry := FileEntry{
			Name:       name,
			Path:       path.Join(requestedDir, name),
			IsDir:      isDir,
			SizeBytes:  hdr.Size,
			Mode:       fs.FileMode(hdr.Mode).String(),
			ModTime:    hdr.ModTime,
			LinkTarget: hdr.Linkname,
		}
		out = append(out, entry)
	}
	return out, nil
}

// createHelper ensures the helper image is present, then creates an
// ephemeral container with the target volume mounted read-only at
// /volume. The container is NOT started — CopyFromContainer works on
// any container that exists, and not starting saves ~100ms per request
// plus avoids spurious "container exited immediately" log noise.
func (b *DockerBrowser) createHelper(ctx context.Context, volumeName string) (string, error) {
	return b.createHelperWithMode(ctx, volumeName, true)
}

// createHelperRW is the read-write variant used by WriteFile.  Split
// out rather than parameterised on createHelper because every other
// browse path should stay strictly read-only by construction — making
// callers opt in to RW makes it obvious in code review which paths
// can mutate volume contents.
func (b *DockerBrowser) createHelperRW(ctx context.Context, volumeName string) (string, error) {
	return b.createHelperWithMode(ctx, volumeName, false)
}

func (b *DockerBrowser) createHelperWithMode(ctx context.Context, volumeName string, readonly bool) (string, error) {
	if err := b.ensureHelperImage(ctx); err != nil {
		return "", err
	}

	mountFlag := "ro"
	if !readonly {
		mountFlag = "rw"
	}

	resp, err := b.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: b.helperImage,
			// Keep a cmd just in case someone tries to start the helper;
			// busybox's default entrypoint does nothing useful otherwise.
			Cmd: []string{"sleep", "60"},
			Labels: map[string]string{
				"managed-by":       "accelero",
				"accelero-helper":  "volume-browser",
			},
		},
		HostConfig: &container.HostConfig{
			Binds:      []string{fmt.Sprintf("%s:/volume:%s", volumeName, mountFlag)},
			AutoRemove: false,
		},
	})
	if err != nil {
		return "", fmt.Errorf("create helper: %w", err)
	}
	return resp.ID, nil
}

// removeHelper is best-effort — failures are logged but not surfaced,
// because the caller is already returning the real result (or error)
// to the HTTP client and a cleanup failure shouldn't mask that.
func (b *DockerBrowser) removeHelper(ctx context.Context, id string) {
	if _, err := b.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
		logrus.WithError(err).WithField("helper_id", id[:12]).Warn("failed to remove volume-browser helper")
	}
}

// ensureHelperImage pulls the helper if it's not on disk yet. On
// subsequent requests the inspect hits and we skip the pull.
func (b *DockerBrowser) ensureHelperImage(ctx context.Context) error {
	if _, err := b.cli.ImageInspect(ctx, b.helperImage); err == nil {
		return nil
	}
	resp, err := b.cli.ImagePull(ctx, b.helperImage, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull helper image %s: %w", b.helperImage, err)
	}
	defer resp.Close()
	if _, err := io.Copy(io.Discard, resp); err != nil {
		return fmt.Errorf("drain helper-image pull stream: %w", err)
	}
	return nil
}

// cleanSubPath normalises the caller-provided path and rejects any
// attempt to escape the /volume root. Returns an absolute path that
// must be joined with "/volume" by the caller.
func cleanSubPath(p string) (string, error) {
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	clean := path.Clean(p)
	// path.Clean can't produce a prefix of ".." because we forced a leading
	// '/', but be explicit: the result must be "/" or start with "/".
	if !strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	// Guard against "/../etc/passwd" which path.Clean leaves as "/etc/passwd"
	// — caller would be surprised. Explicitly reject paths that had any ".."
	// segment before cleaning.
	if containsParentRef(p) {
		return "", fmt.Errorf("path %q contains '..'", p)
	}
	return clean, nil
}

// containsParentRef returns true when the raw path contains a "/../"
// or trailing "/..". Used as the security check before path.Clean
// collapses ".." segments silently.
func containsParentRef(p string) bool {
	return strings.Contains(p, "/../") || strings.HasSuffix(p, "/..")
}

// readerWithTeardown is an io.ReadCloser that runs onClose when the
// caller closes it. Used so the HTTP handler's defer on the response
// body also tears down the helper container.
type readerWithTeardown struct {
	reader  io.Reader
	onClose func()
	closed  bool
}

func (r *readerWithTeardown) Read(p []byte) (int, error) { return r.reader.Read(p) }
func (r *readerWithTeardown) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.onClose()
	return nil
}

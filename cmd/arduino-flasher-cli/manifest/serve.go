// This file is part of arduino-flasher-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/arduino/arduino-flasher-cli/cmd/feedback"
	"github.com/arduino/arduino-flasher-cli/cmd/i18n"
	"github.com/arduino/arduino-flasher-cli/internal/registry"
)

func newServeCmd() *cobra.Command {
	var opts options
	var dir string
	var port uint16
	cmd := &cobra.Command{
		Use: "serve", Short: "Serve a manifest and its local images over HTTP on the LAN",
		Long: `Serve an image manifest at /info.json and local images at /images/<filename>.
Without --manifest, scan --dir (default: current directory) and build a manifest
in memory. Metadata is detected from filenames or requested interactively;
use --board, --os and --version for scripts or overrides. Each image is hashed
once at startup. No JSON file is created or overwritten.
With --manifest, load that file and verify its listed local images instead.
Listens on all network interfaces, without TLS or authentication. Other files,
symlinks and directory listings are not served. URLs use the requested host and
port. Stop and restart after editing the manifest or images. Press Ctrl+C to stop.
`,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.NoArgs(cmd, args); err != nil {
				return err
			}
			if cmd.Flags().Changed("manifest") && opts.file == "" {
				return fmt.Errorf("--manifest requires a file path")
			}
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			if err := serveManifest(cmd, opts, dir, port); err != nil {
				feedback.Fatal(i18n.Tr("error serving the manifest: %v", err), feedback.ErrGeneric)
			}
		},
	}
	cmd.Flags().StringVar(&opts.file, "manifest", "", "Load an existing manifest instead of generating one in memory")
	cmd.Flags().StringVar(&dir, "dir", "", "Image directory (default: current directory, or manifest directory with --manifest)")
	cmd.Flags().Uint16Var(&port, "port", 8080, "HTTP port (0 chooses an available port)")
	cmd.Flags().StringVar(&opts.board, "board", "", "Board ID or label for generated entries (overrides filename detection)")
	cmd.Flags().StringVar(&opts.os, "os", "", "Linux distribution for generated entries (overrides filename detection)")
	cmd.Flags().StringVar(&opts.version, "version", "", "Version for generated entries (overrides filename detection)")
	return cmd
}

func serveManifest(cmd *cobra.Command, opts options, dir string, port uint16) error {
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return err
	}
	if dir == "" {
		dir = filepath.Dir(opts.file)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	feedback.Print(i18n.Tr("Calculating local image checksums..."))
	handler, closeImages, err := newServeHandler(cmd, opts, root)
	if err != nil {
		return err
	}
	defer closeImages()
	publicURL, err := sourceURL("info.json", "")
	if err != nil {
		return err
	}
	advertised, err := httpURL(publicURL)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(int(port))))
	if err != nil {
		return err
	}
	defer listener.Close()
	actualPort := listener.Addr().(*net.TCPAddr).Port
	advertised.Host = net.JoinHostPort(advertised.Hostname(), strconv.Itoa(actualPort))
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       time.Minute,
	}
	defer server.Close()
	shutdownDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(shutdownDone)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	})
	defer func() {
		if !stop() {
			<-shutdownDone
		}
	}()
	feedback.Warning(i18n.Tr("Anyone on the LAN can read this manifest and its listed local files. This HTTP server has no authentication or TLS."))
	feedback.PrintResult(serveResult{URL: advertised.String(), Directory: dir})
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type servedImage struct {
	file *os.File
	info os.FileInfo
	hash string
}

// Bind the checksum to the open file, not merely to a matching filename.
func openServedImage(ctx context.Context, root *os.Root, name, expectedHash string) (*servedImage, error) {
	checked, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !checked.Mode().IsRegular() {
		return nil, nil
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(checked, info) {
		return nil, fmt.Errorf("image changed while opening: %s", name)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("image is empty: %s", name)
	}
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	hash := sha256.New()
	copied, err := io.Copy(hash, f)
	stop()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if copied != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, fmt.Errorf("image changed while hashing; finish copying it before serving: %s", name)
	}
	sum := fmt.Sprintf("%x", hash.Sum(nil))
	if expectedHash != "" && !strings.EqualFold(sum, expectedHash) {
		return nil, fmt.Errorf("local image SHA-256 does not match the manifest: %s", name)
	}
	keep = true
	return &servedImage{file: f, info: info, hash: sum}, nil
}

func loadServeManifest(cmd *cobra.Command, opts options, root *os.Root) (registry.Manifest, error) {
	if opts.file != "" {
		if opts.board != "" || opts.os != "" || opts.version != "" {
			return registry.Manifest{}, fmt.Errorf("--board, --os and --version only apply to generated manifests; omit --manifest to generate one")
		}
		return readManifest(opts.file)
	}
	m := registry.Manifest{Releases: []registry.ManifestRelease{}}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return m, err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !isImageArchive(entry.Name()) {
			continue
		}
		u := &url.URL{Scheme: "http", Host: "localhost", Path: "/images/" + entry.Name()}
		r, err := describeSource(cmd, entry.Name(), u.String(), opts)
		if err != nil {
			return m, err
		}
		if err := checkImageIdentity(m.Releases, r); err != nil {
			return m, err
		}
		m.Releases = append(m.Releases, r)
	}
	if len(m.Releases) == 0 {
		return m, fmt.Errorf("no image archives in directory: %s", root.Name())
	}
	return m, nil
}

// shortcut: fixed snapshot with open image handles, add reload/on-demand verification for large or live inventories.
func newServeHandler(cmd *cobra.Command, opts options, root *os.Root) (http.Handler, func(), error) {
	ctx := cmd.Context()
	generated := opts.file == ""
	m, err := loadServeManifest(cmd, opts, root)
	if err != nil {
		return nil, nil, err
	}
	files := make(map[string]*servedImage)
	localURLs := make(map[string]string)
	closeImages := func() {
		for _, image := range files {
			_ = image.file.Close()
		}
	}
	ready := false
	defer func() {
		if !ready {
			closeImages()
		}
	}()
	for i, r := range m.Releases {
		u, err := httpURL(r.Url)
		if err != nil {
			return nil, nil, err
		}
		name := path.Base(u.Path)
		if !filepath.IsLocal(name) || filepath.Base(name) != name {
			if generated {
				return nil, nil, fmt.Errorf("unsupported local image filename: %s", name)
			}
			continue
		}
		requestPath := "/images/" + name
		image := files[requestPath]
		if image == nil {
			image, err = openServedImage(ctx, root, name, r.Sha256)
			if err != nil {
				return nil, nil, err
			}
			if image == nil {
				if generated {
					return nil, nil, fmt.Errorf("image is no longer a regular file: %s", name)
				}
				continue
			}
			files[requestPath] = image
		} else if !strings.EqualFold(image.hash, r.Sha256) {
			return nil, nil, fmt.Errorf("conflicting local image filename: %s", name)
		}
		if generated {
			r.Sha256 = image.hash
			m.Releases[i] = r
		}
		localURLs[r.Url] = requestPath
	}
	if generated {
		if err := m.SetLatest(""); err != nil {
			return nil, nil, err
		}
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if req.URL.Path == "/info.json" {
			base, err := httpURL("http://" + req.Host)
			if err != nil || base.User != nil || base.Path != "" || base.RawQuery != "" || base.ForceQuery {
				http.Error(w, "invalid HTTP Host header", http.StatusBadRequest)
				return
			}
			response := m
			response.Releases = slices.Clone(m.Releases)
			for i, r := range response.Releases {
				if localPath, local := localURLs[r.Url]; local {
					originalURL := r.Url
					base.Path = localPath
					r.Url = base.String()
					if response.Latest != nil && response.Latest.Url == originalURL {
						response.Latest = &r
					}
					response.Releases[i] = r
				}
			}
			data, err := json.MarshalIndent(response, "", "  ")
			if err != nil {
				http.Error(w, "could not encode manifest", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			http.ServeContent(w, req, "info.json", time.Time{}, bytes.NewReader(append(data, '\n')))
			return
		}
		image := files[req.URL.Path]
		if image == nil {
			http.NotFound(w, req)
			return
		}
		name := path.Base(req.URL.Path)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		reader := io.NewSectionReader(image.file, 0, image.info.Size())
		http.ServeContent(w, req, name, image.info.ModTime(), reader)
	})
	ready = true
	return handler, closeImages, nil
}

type serveResult struct {
	URL       string `json:"url"`
	Directory string `json:"directory"`
}

func (r serveResult) Data() interface{} { return r }
func (r serveResult) String() string {
	return i18n.Tr("Manifest URL: %s\nImages directory: %s\nListening on all network interfaces. Use this host's LAN IP if Bonjour/mDNS is unavailable. Press Ctrl+C to stop.", r.URL, r.Directory)
}

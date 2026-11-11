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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-flasher-cli/internal/registry"
)

const serveFirstName = "unoq-debian-20261101-1 image.zip"
const serveFirstBody = "first-image-content"
const serveSecondName = "ventunoq-ubuntu-20261101-2.zip"
const serveOrigin = "https://origin.example/images/"

func serveFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	first := imageFile(t, dir, serveFirstName, serveFirstBody)
	second := imageFile(t, dir, serveSecondName, "second-image-content")
	file := filepath.Join(dir, "custom-manifest.json")
	edit(t, "create", []string{first, second}, options{file: file, baseURL: serveOrigin})
	m, err := readManifest(file)
	require.NoError(t, err)
	m.Releases = append(m.Releases, registry.ManifestRelease{
		Version: "20261101-3", Board: "UNO Q", Os: "debian",
		Url: "https://remote.example/remote.zip", Sha256: fmt.Sprintf("%x", sha256.Sum256([]byte("remote image"))),
	})
	require.NoError(t, writeManifest(file, m, false))
	return file, dir
}

func serveTestCommand(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetIn(strings.NewReader(""))
	return cmd
}

func startServeTest(t *testing.T, file, dir string) *httptest.Server {
	t.Helper()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	handler, closeImages, err := newServeHandler(serveTestCommand(context.Background()), options{file: file}, root)
	require.NoError(t, err)
	t.Cleanup(closeImages)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestServeManifestAndImages(t *testing.T) {
	file, dir := serveFixture(t)
	before, err := os.ReadFile(file)
	require.NoError(t, err)
	// The manifest need not be in the images directory.
	published := imageFile(t, t.TempDir(), "index.json", string(before))
	srv := startServeTest(t, published, dir)
	resp, err := srv.Client().Get(srv.URL + "/info.json")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var m registry.Manifest
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&m))
	require.Len(t, m.Releases, 3)
	assert.Equal(t, srv.URL+"/images/"+url.PathEscape(serveFirstName), m.Releases[0].Url)
	assert.Equal(t, srv.URL+"/images/"+serveSecondName, m.Latest.Url)
	assert.Equal(t, "https://remote.example/remote.zip", m.Releases[2].Url, "missing local images keep their remote URLs")
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(serveFirstBody))), m.Releases[0].Sha256)

	respImage, err := srv.Client().Get(m.Releases[0].Url)
	require.NoError(t, err)
	defer respImage.Body.Close()
	body, err := io.ReadAll(respImage.Body)
	require.NoError(t, err)
	assert.Equal(t, serveFirstBody, string(body))
	assert.Equal(t, "application/octet-stream", respImage.Header.Get("Content-Type"))
	assert.Contains(t, respImage.Header.Get("Content-Disposition"), "attachment")
	assert.Equal(t, "nosniff", respImage.Header.Get("X-Content-Type-Options"))

	head, err := srv.Client().Head(m.Releases[0].Url)
	require.NoError(t, err)
	defer head.Body.Close()
	assert.Equal(t, int64(len(serveFirstBody)), head.ContentLength)
	body, err = io.ReadAll(head.Body)
	require.NoError(t, err)
	assert.Empty(t, body)

	req, err := http.NewRequest(http.MethodGet, m.Releases[0].Url, nil)
	require.NoError(t, err)
	req.Header.Set("Range", "bytes=2-5")
	ranged, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer ranged.Body.Close()
	body, err = io.ReadAll(ranged.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusPartialContent, ranged.StatusCode)
	assert.Equal(t, serveFirstBody[2:6], string(body))
	after, err := os.ReadFile(published)
	require.NoError(t, err)
	assert.Equal(t, before, after, "URL rewriting must never edit the source manifest")
}

func TestServeDoesNotExposeOtherFilesOrAcceptWrites(t *testing.T) {
	file, dir := serveFixture(t)
	imageFile(t, dir, "secret.txt", "private data")
	srv := startServeTest(t, file, dir)
	for _, requestPath := range []string{"/", "/images/", "/secret.txt", "/images/secret.txt", "/images/../secret.txt", "/images/%2e%2e/secret.txt", "/custom-manifest.json", "/images/custom-manifest.json"} {
		resp, err := srv.Client().Get(srv.URL + requestPath)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, requestPath)
		assert.NotContains(t, string(body), "private data", requestPath)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequest(method, srv.URL+"/images/"+url.PathEscape(serveFirstName), strings.NewReader("overwrite"))
		require.NoError(t, err)
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
		assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"))
	}
	contents, err := os.ReadFile(filepath.Join(dir, serveFirstName))
	require.NoError(t, err)
	assert.Equal(t, serveFirstBody, string(contents))
}

func TestServeRejectsUnrelatedLocalFileWithMatchingName(t *testing.T) {
	file, dir := serveFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, serveSecondName), []byte("private data, not the expected image"), 0o600))
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	_, _, err = newServeHandler(serveTestCommand(context.Background()), options{file: file}, root)
	require.ErrorContains(t, err, "SHA-256 does not match")
}

// AfterFunc registers cancellation after f.Stat; mutate there without timing races.
type hashMutationContext struct {
	context.Context
	once   sync.Once
	mutate func()
}

func (ctx *hashMutationContext) Done() <-chan struct{} {
	ctx.once.Do(ctx.mutate)
	return ctx.Context.Done()
}

func TestServeRejectsImageChangesDuringVerification(t *testing.T) {
	for _, change := range []string{"grow", "shrink", "same-size rewrite"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			name := "unoq-debian-20261101-1.zip"
			file := imageFile(t, dir, name, "original image")
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			defer root.Close()
			ctx := &hashMutationContext{Context: context.Background(), mutate: func() {
				switch change {
				case "grow":
					f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND, 0)
					require.NoError(t, err)
					_, err = f.Write([]byte("finished copying"))
					require.NoError(t, err)
					require.NoError(t, f.Close())
				case "shrink":
					require.NoError(t, os.Truncate(file, 3))
				case "same-size rewrite":
					info, err := os.Stat(file)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(file, []byte("modified image"), 0o600))
					changedTime := info.ModTime().Add(2 * time.Second)
					require.NoError(t, os.Chtimes(file, changedTime, changedTime))
				}
			}}
			for _, generated := range []bool{true, false} {
				// Reuse each mutation scenario for generated and supplied checksums.
				if !generated {
					require.NoError(t, os.WriteFile(file, []byte("original image"), 0o600))
					ctx.once = sync.Once{}
				}
				expected := ""
				if !generated {
					expected = fmt.Sprintf("%x", sha256.Sum256([]byte("original image")))
				}
				image, err := openServedImage(ctx, root, name, expected)
				require.ErrorContains(t, err, "image changed while hashing")
				assert.Nil(t, image)
			}
		})
	}
}

func TestServeRejectsConflictingLocalFilenames(t *testing.T) {
	file, dir := serveFixture(t)
	m, err := readManifest(file)
	require.NoError(t, err)
	m.Releases = append(m.Releases, registry.ManifestRelease{
		Version: "different", Board: "UNO Q", Os: "debian",
		Url:    "https://other.example/" + url.PathEscape(serveFirstName),
		Sha256: fmt.Sprintf("%x", sha256.Sum256([]byte("different image"))),
	})
	require.NoError(t, writeManifest(file, m, false))
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	_, _, err = newServeHandler(serveTestCommand(context.Background()), options{file: file}, root)
	require.ErrorContains(t, err, "conflicting local image filename")
}

func TestServeKeepsVerifiedFileAfterNameReplacement(t *testing.T) {
	file, dir := serveFixture(t)
	srv := startServeTest(t, file, dir)
	image := filepath.Join(dir, serveFirstName)
	if err := os.Rename(image, image+".saved"); err != nil {
		t.Skipf("platform does not allow renaming an open image: %v", err)
	}
	require.NoError(t, os.WriteFile(image, []byte("private replacement data"), 0o600))
	resp, err := srv.Client().Get(srv.URL + "/images/" + url.PathEscape(serveFirstName))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, serveFirstBody, string(body))
}

func TestServedManifestWorksWithRegistryDownloader(t *testing.T) {
	file, dir := serveFixture(t)
	srv := startServeTest(t, file, dir)
	t.Setenv("ARDUINO_FLASHER_ADDITIONAL_URLS", "")
	t.Setenv("ARDUINO_FLASHER_ADDITIONAL_HEADERS", "")
	original := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: manifestReaderTransport{}}
	t.Cleanup(func() { http.DefaultClient = original })
	client := registry.NewClient(srv.URL + "/info.json")
	releases, err := client.Fetch(context.Background())
	require.NoError(t, err)
	r, err := releases.Resolve("debian", "unoq", "20261101-1")
	require.NoError(t, err)
	archive, err := client.DownloadFile(context.Background(), paths.New(t.TempDir()), r, func(int64, int64) {})
	require.NoError(t, err)
	body, err := os.ReadFile(archive.String())
	require.NoError(t, err)
	assert.Equal(t, serveFirstBody, string(body))
}

func TestServeSkipsSymlinks(t *testing.T) {
	file, dir := serveFixture(t)
	secret := imageFile(t, t.TempDir(), "secret.txt", "private data")
	if err := os.Symlink(secret, filepath.Join(dir, "linked.zip")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	m, err := readManifest(file)
	require.NoError(t, err)
	m.Releases = append(m.Releases, registry.ManifestRelease{
		Version: "4", Board: "UNO Q", Os: "debian", Url: "https://remote.example/linked.zip",
		Sha256: fmt.Sprintf("%x", sha256.Sum256([]byte("private data"))),
	})
	require.NoError(t, writeManifest(file, m, false))
	srv := startServeTest(t, file, dir)
	resp, err := srv.Client().Get(srv.URL + "/images/linked.zip")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestServeUsesAStartupSnapshot(t *testing.T) {
	file, dir := serveFixture(t)
	srv := startServeTest(t, file, dir)
	added := imageFile(t, dir, "unoq-debian-20261102-4.zip", "new image")
	edit(t, "add", []string{added}, options{file: file, baseURL: serveOrigin})
	resp, err := srv.Client().Get(srv.URL + "/images/" + filepath.Base(added))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp, err = srv.Client().Get(srv.URL + "/info.json")
	require.NoError(t, err)
	var snapshot registry.Manifest
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&snapshot))
	resp.Body.Close()
	assert.Len(t, snapshot.Releases, 3)
	restarted := startServeTest(t, file, dir)
	resp, err = restarted.Client().Get(restarted.URL + "/images/" + filepath.Base(added))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestServeConcurrentReadsAndHostIsolation(t *testing.T) {
	file, dir := serveFixture(t)
	srv := startServeTest(t, file, dir)
	var workers sync.WaitGroup
	for i := range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			host := fmt.Sprintf("client-%d.local:8080", i)
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/info.json", nil)
			if !assert.NoError(t, err) {
				return
			}
			req.Host = host
			resp, err := srv.Client().Do(req)
			if !assert.NoError(t, err) {
				return
			}
			var m registry.Manifest
			err = json.NewDecoder(resp.Body).Decode(&m)
			resp.Body.Close()
			if !assert.NoError(t, err) {
				return
			}
			assert.Equal(t, "http://"+host+"/images/"+serveSecondName, m.Latest.Url)
			req, err = http.NewRequest(http.MethodGet, srv.URL+"/images/"+url.PathEscape(serveFirstName), nil)
			if !assert.NoError(t, err) {
				return
			}
			req.Header.Set("Range", "bytes=2-5")
			resp, err = srv.Client().Do(req)
			if !assert.NoError(t, err) {
				return
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			assert.NoError(t, err)
			assert.Equal(t, serveFirstBody[2:6], string(body))
		}()
	}
	workers.Wait()
}

func TestServeStopsOnCancellationAndValidatesFlags(t *testing.T) {
	file, _ := serveFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, serveManifest(serveTestCommand(ctx), options{file: file}, "", 0))
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	cmd := NewManifestCmd()
	serve, _, err := cmd.Find([]string{"serve"})
	require.NoError(t, err)
	port, err := serve.Flags().GetUint16("port")
	require.NoError(t, err)
	assert.Equal(t, uint16(8080), port)
	manifest, err := serve.Flags().GetString("manifest")
	require.NoError(t, err)
	assert.Empty(t, manifest, "omitting --manifest must select in-memory generation")
	require.Error(t, serve.ParseFlags([]string{"--port", "65536"}))
	require.Error(t, serve.Args(serve, []string{"unexpected"}))
}

func TestServeGeneratesManifestInMemory(t *testing.T) {
	dir := t.TempDir()
	imageFile(t, dir, serveFirstName, serveFirstBody)
	imageFile(t, dir, serveSecondName, "second-image-content")
	marker := "private JSON: do not overwrite"
	imageFile(t, dir, "info.json", marker)
	imageFile(t, dir, "secret.txt", "private data")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o700))
	imageFile(t, filepath.Join(dir, "nested"), "unoq-debian-20261101-9.zip", "nested image")
	before, err := os.ReadDir(dir)
	require.NoError(t, err)
	srv := startServeTest(t, "", dir)
	resp, err := srv.Client().Get(srv.URL + "/info.json")
	require.NoError(t, err)
	defer resp.Body.Close()
	var m registry.Manifest
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&m))
	require.Len(t, m.Releases, 2)
	require.NotNil(t, m.Latest)
	assert.Equal(t, "20261101-2", m.Latest.Version)
	assert.Equal(t, "VENTUNO Q", m.Latest.Board)
	assert.Equal(t, "ubuntu", m.Latest.Os)
	assert.Equal(t, srv.URL+"/images/"+serveSecondName, m.Latest.Url)
	for _, r := range m.Releases {
		image, err := srv.Client().Get(r.Url)
		require.NoError(t, err)
		body, err := io.ReadAll(image.Body)
		image.Body.Close()
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, image.StatusCode)
		assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(body)), r.Sha256)
	}
	respPrivate, err := srv.Client().Get(srv.URL + "/images/info.json")
	require.NoError(t, err)
	respPrivate.Body.Close()
	assert.Equal(t, http.StatusNotFound, respPrivate.StatusCode)
	after, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, after, len(before), "generation must not create any files")
	contents, err := os.ReadFile(filepath.Join(dir, "info.json"))
	require.NoError(t, err)
	assert.Equal(t, marker, string(contents))
}

func TestGeneratedServeMetadataOverrides(t *testing.T) {
	dir := t.TempDir()
	imageFile(t, dir, "custom image.zip", "custom image")
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	cmd := serveTestCommand(context.Background())
	opts := options{board: "ventunoq", os: "Custom Linux", version: "custom-1"}
	metadata, err := loadServeManifest(cmd, opts, root)
	require.NoError(t, err)
	assert.Empty(t, metadata.Releases[0].Sha256, "metadata detection must not hash image contents")
	handler, closeImages, err := newServeHandler(cmd, opts, root)
	require.NoError(t, err)
	defer closeImages()
	request := httptest.NewRequest(http.MethodGet, "http://images.local:8080/info.json", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var m registry.Manifest
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &m))
	require.Len(t, m.Releases, 1)
	assert.Equal(t, "VENTUNO Q", m.Latest.Board)
	assert.Equal(t, "Custom Linux", m.Latest.Os)
	assert.Equal(t, "custom-1", m.Latest.Version)
	assert.Equal(t, "http://images.local:8080/images/custom%20image.zip", m.Latest.Url)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("custom image"))), m.Latest.Sha256)
}

func TestGeneratedServeUsesCurrentDirectoryAndMetadataFlags(t *testing.T) {
	dir := t.TempDir()
	imageFile(t, dir, "custom.zip", "custom image")
	t.Chdir(dir)
	cmd := newServeCmd()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"--port", "0", "--board", "ventunoq", "--os", "Custom Linux", "--version", "custom-1"})
	require.NoError(t, cmd.Execute())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "custom.zip", entries[0].Name())
}

func TestGeneratedServeRejectsInvalidImages(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		files         map[string]string
	}{
		{"no images", "no image archives", map[string]string{"info.json": "not an image"}},
		{"missing metadata", "--board, --os, --version", map[string]string{"custom.zip": "image"}},
		{"duplicate identity", "already exists", map[string]string{"unoq-debian-20261101-1.zip": "image 1", "unoq-debian-20261101-1-alt.zip": "image 2"}},
		{"empty image", "image is empty", map[string]string{"unoq-debian-20261101-1.zip": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				imageFile(t, dir, name, body)
			}
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			defer root.Close()
			_, _, err = newServeHandler(serveTestCommand(context.Background()), options{}, root)
			require.ErrorContains(t, err, tc.message)
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Len(t, entries, len(tc.files))
		})
	}
}

func TestExplicitManifestDoesNotFallBackToGeneration(t *testing.T) {
	file, dir := serveFixture(t)
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	_, _, err = newServeHandler(serveTestCommand(context.Background()), options{file: filepath.Join(dir, "missing.json")}, root)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, _, err = newServeHandler(serveTestCommand(context.Background()), options{file: file, board: "unoq"}, root)
	require.ErrorContains(t, err, "only apply to generated manifests")
	cmd := newServeCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--manifest", ""}))
	require.ErrorContains(t, cmd.Args(cmd, nil), "--manifest requires a file path")
}

func TestServeReportsStartupFailures(t *testing.T) {
	file, dir := serveFixture(t)
	require.Error(t, serveManifest(serveTestCommand(context.Background()), options{file: filepath.Join(dir, "missing.json")}, dir, 0))
	require.Error(t, serveManifest(serveTestCommand(context.Background()), options{file: file}, filepath.Join(dir, "missing"), 0))
	listener, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer listener.Close()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	require.Error(t, serveManifest(serveTestCommand(context.Background()), options{file: file}, dir, port))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, serveManifest(serveTestCommand(ctx), options{file: file}, dir, 0), context.Canceled)
}

func TestServeRejectsInvalidHostWithoutLeakingPaths(t *testing.T) {
	file, dir := serveFixture(t)
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	handler, closeImages, err := newServeHandler(serveTestCommand(context.Background()), options{file: file}, root)
	require.NoError(t, err)
	defer closeImages()
	req := httptest.NewRequest(http.MethodGet, "/info.json", nil)
	req.Host = "user:password@example.com"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.False(t, bytes.Contains(response.Body.Bytes(), []byte(dir)))
}

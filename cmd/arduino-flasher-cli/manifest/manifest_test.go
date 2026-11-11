// This file is part of arduino-flasher-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-flasher-cli/internal/registry"
)

func imageFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	file := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(file, []byte(body), 0o600))
	return file
}

func edit(t *testing.T, action string, inputs []string, opts options) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	_, err := editManifest(context.Background(), action, inputs, opts, cmd)
	require.NoError(t, err)
}

func TestManifestLifecycle(t *testing.T) {
	dir := t.TempDir()
	old := imageFile(t, dir, "arduino-unoq-debian-20261101-9.tar.zst", "old image")
	newer := imageFile(t, dir, "arduino-unoq-debian-20261101-10.tar.zst", "new image")
	imageFile(t, dir, "README.txt", "not an image")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o700))
	imageFile(t, filepath.Join(dir, "nested"), "unoq-debian-20261101-99.zip", "nested image")
	opts := options{file: filepath.Join(dir, "info.json"), baseURL: "https://images.example/releases"}
	edit(t, "create", []string{dir}, opts)
	m, err := readManifest(opts.file)
	require.NoError(t, err)
	require.Len(t, m.Releases, 2)
	require.NotNil(t, m.Latest)
	assert.Equal(t, "20261101-10", m.Latest.Version)
	assert.Equal(t, "UNO Q", m.Latest.Board)
	assert.Equal(t, "debian", m.Latest.Os)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("new image"))), m.Latest.Sha256)

	oldURL, err := sourceURL(old, opts.baseURL)
	require.NoError(t, err)
	edit(t, "latest", []string{oldURL}, opts)
	third := imageFile(t, dir, "arduino-ventunoq-ubuntu-20261102-1.zip", "third image")
	edit(t, "add", []string{third}, opts)
	m, err = readManifest(opts.file)
	require.NoError(t, err)
	assert.Equal(t, oldURL, m.Latest.Url, "adding images preserves the selected latest")
	require.Len(t, m.Releases, 3)

	edit(t, "remove", []string{old}, opts)
	m, err = readManifest(opts.file)
	require.NoError(t, err)
	require.Len(t, m.Releases, 2)
	assert.Equal(t, "20261102-1", m.Latest.Version, "removing latest selects the newest remaining version")
	_, err = os.Stat(old)
	require.NoError(t, err, "removing an entry never removes its archive")

	newerURL, err := sourceURL(newer, opts.baseURL)
	require.NoError(t, err)
	opts.latest = newerURL
	edit(t, "remove", []string{third}, opts)
	m, err = readManifest(opts.file)
	require.NoError(t, err)
	assert.Equal(t, newerURL, m.Latest.Url)
	opts.latest = ""
	edit(t, "remove", []string{newerURL}, opts)
	m, err = readManifest(opts.file)
	require.NoError(t, err)
	assert.Empty(t, m.Releases)
	assert.Nil(t, m.Latest)
}

func TestCreateFromURLsAndFailedDownloadPreservesManifest(t *testing.T) {
	t.Setenv("ARDUINO_FLASHER_ADDITIONAL_HEADERS", "Authorization: Bearer test-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/octet-stream")
		switch r.URL.Path {
		case "/unavailable.zip":
			w.WriteHeader(http.StatusNotFound)
		case "/empty.zip":
			w.WriteHeader(http.StatusOK)
		case "/truncated.zip":
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("incomplete"))
		case "/login.zip":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html>Please log in</html>"))
		default:
			_, _ = w.Write([]byte("remote image"))
		}
	}))
	defer srv.Close()
	older := srv.URL + "/unoq-fedora-20261101-1.tar.zst?download=1"
	newer := srv.URL + "/ventunoq-ubuntu-20261102-1.tar.zst"
	opts := options{file: filepath.Join(t.TempDir(), "info.json"), latest: older}
	edit(t, "create", []string{older, newer}, opts)
	m, err := readManifest(opts.file)
	require.NoError(t, err)
	require.Len(t, m.Releases, 2)
	assert.Equal(t, older, m.Latest.Url, "an older URL can explicitly be marked latest")
	assert.Equal(t, "fedora", m.Latest.Os)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("remote image"))), m.Latest.Sha256)
	before, err := os.ReadFile(opts.file)
	require.NoError(t, err)
	opts.board, opts.os, opts.version, opts.latest = "unoq", "debian", "test", ""
	for _, endpoint := range []string{"/unavailable.zip", "/truncated.zip", "/login.zip", "/empty.zip"} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := editManifest(context.Background(), "add", []string{srv.URL + endpoint}, opts, &cobra.Command{})
			require.Error(t, err)
			after, err := os.ReadFile(opts.file)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestMissingMetadataRequestsFlagsWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	file := imageFile(t, dir, "image.zip", "image")
	opts := options{file: filepath.Join(dir, "info.json"), baseURL: "https://images.example"}
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	_, err := editManifest(context.Background(), "create", []string{file}, opts, cmd)
	require.ErrorContains(t, err, "--board, --os, --version")
	_, err = os.Stat(opts.file)
	require.ErrorIs(t, err, os.ErrNotExist)
	opts.board, opts.os, opts.version = "ventunoq", "Custom Linux", "release-1"
	edit(t, "create", []string{file}, opts)
	m, err := readManifest(opts.file)
	require.NoError(t, err)
	assert.Equal(t, "VENTUNO Q", m.Latest.Board)
	assert.Equal(t, "Custom Linux", m.Latest.Os)
}

func TestInvalidEditsLeaveManifestUntouched(t *testing.T) {
	dir := t.TempDir()
	file := imageFile(t, dir, "unoq-debian-20261101-1.zip", "image")
	opts := options{file: filepath.Join(dir, "info.json"), baseURL: "https://images.example"}
	edit(t, "create", []string{file}, opts)
	before, err := os.ReadFile(opts.file)
	require.NoError(t, err)
	for _, tc := range []struct {
		action  string
		inputs  []string
		message string
	}{
		{"create", nil, "already exists"},
		{"add", []string{file}, "already in the manifest"},
		{"remove", []string{"https://images.example/missing.zip"}, "not in the manifest"},
		{"latest", []string{"https://images.example/missing.zip"}, "not in the manifest"},
		{"latest", []string{"ftp://images.example/image.zip"}, "HTTP(S)"},
	} {
		t.Run(tc.action+tc.message, func(t *testing.T) {
			_, err := editManifest(context.Background(), tc.action, tc.inputs, opts, &cobra.Command{})
			require.ErrorContains(t, err, tc.message)
			after, err := os.ReadFile(opts.file)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = editManifest(ctx, "remove", []string{file}, opts, &cobra.Command{})
	require.ErrorIs(t, err, context.Canceled)
	after, err := os.ReadFile(opts.file)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestManifestRejectsUnknownOrInvalidJSONWithoutWriting(t *testing.T) {
	for _, body := range []string{`null`, `{"releases":[]}{}`, `{"releases":[],"custom":true}`, `{"releases":null}`, `not json`} {
		t.Run(body, func(t *testing.T) {
			file := imageFile(t, t.TempDir(), "info.json", body)
			_, err := editManifest(context.Background(), "add", nil, options{file: file}, &cobra.Command{})
			require.Error(t, err)
			after, err := os.ReadFile(file)
			require.NoError(t, err)
			assert.Equal(t, body, string(after))
		})
	}
}

func TestEmptyManifestAndCommandFlags(t *testing.T) {
	file := filepath.Join(t.TempDir(), "empty.json")
	cmd := NewManifestCmd()
	cmd.SetArgs([]string{"create", "--output", file})
	require.NoError(t, cmd.Execute())
	m, err := readManifest(file)
	require.NoError(t, err)
	assert.Empty(t, m.Releases)
	assert.Nil(t, m.Latest)
	for _, action := range []string{"add", "remove", "latest"} {
		cmd := NewManifestCmd()
		cmd.SetArgs([]string{action})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		require.Error(t, cmd.Execute(), "editing commands require inputs")
	}
}

func TestFailedReplacementCleansUpTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "info.json")
	edit(t, "create", nil, options{file: file})
	m, err := readManifest(file)
	require.NoError(t, err)
	blocked := filepath.Join(dir, "blocked")
	require.NoError(t, os.Mkdir(blocked, 0o700))
	marker := imageFile(t, blocked, "keep", "original data")
	require.Error(t, writeManifest(blocked, m, false))
	contents, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "original data", string(contents))
	temporary, err := filepath.Glob(filepath.Join(dir, ".manifest-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, temporary)
}

func TestCreateCommandMetadataFlags(t *testing.T) {
	dir := t.TempDir()
	file := imageFile(t, dir, "custom image.zip", "image")
	output := filepath.Join(dir, "info.json")
	cmd := NewManifestCmd()
	cmd.SetArgs([]string{"create", file, "--output", output, "--base-url", "https://images.example", "--board", "ventunoq", "--os", "Debian GNU/Linux 13 (trixie)", "--version", "custom-1", "--latest", "https://images.example/custom%20image.zip"})
	require.NoError(t, cmd.Execute())
	m, err := readManifest(output)
	require.NoError(t, err)
	assert.Equal(t, "VENTUNO Q", m.Latest.Board)
	assert.Equal(t, "Debian GNU/Linux 13 (trixie)", m.Latest.Os)
	assert.Equal(t, "custom-1", m.Latest.Version)
}

func TestFailedCreateDoesNotLeaveAManifest(t *testing.T) {
	dir := t.TempDir()
	image := imageFile(t, dir, "unoq-debian-20261101-1.zip", "image")
	for _, tc := range []struct {
		name, base, board, latest, message string
		duplicate                          bool
	}{
		{name: "missing base", message: "--base-url"},
		{name: "bad base", base: "https://images.example/?q=1", message: "query string"},
		{name: "unknown board", base: "https://images.example", board: "unknown", message: "unknown board"},
		{name: "unknown latest", base: "https://images.example", latest: "https://images.example/other.zip", message: "not in the manifest"},
		{name: "duplicate", base: "https://images.example", duplicate: true, message: "already in the manifest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "info.json")
			inputs := []string{image}
			if tc.duplicate {
				inputs = append(inputs, image)
			}
			_, err := editManifest(context.Background(), "create", inputs, options{file: file, baseURL: tc.base, board: tc.board, latest: tc.latest}, &cobra.Command{})
			require.ErrorContains(t, err, tc.message)
			_, err = os.Stat(file)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestFilenameDetectionIsConservative(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	r, err := describeSource(cmd, "ARDUINO_VENTUNO_Q_Ubuntu_20261101-1.ZIP", "https://images.example/image.zip", options{})
	require.NoError(t, err)
	assert.Equal(t, "VENTUNO Q", r.Board)
	assert.Equal(t, "ubuntu", r.Os)
	assert.Equal(t, "20261101-1", r.Version)
	_, err = describeSource(cmd, "unoq-ventunoq-debian-20261101-1.zip", "https://images.example/image.zip", options{})
	require.ErrorContains(t, err, "--board", "ambiguous board names must not be guessed")
	_, err = describeSource(cmd, "unoq-debian-ubuntu-20261101-1.zip", "https://images.example/image.zip", options{})
	require.ErrorContains(t, err, "--os")
}

func TestSourceURLAndLocalHashValidation(t *testing.T) {
	for _, tc := range []struct{ base, expected string }{
		{"https://images.example/releases/", "https://images.example/releases/image%20name.zip"},
		{"https://images.example/team%2Fimages/", "https://images.example/team%2Fimages/image%20name.zip"},
		{"https://images.example/team%2F", "https://images.example/team%2F/image%20name.zip"},
		{"https://images.example/team%252Fimages/", "https://images.example/team%252Fimages/image%20name.zip"},
		{"https://images.example/team//images/", "https://images.example/team//images/image%20name.zip"},
	} {
		u, err := sourceURL("image name.zip", tc.base)
		require.NoError(t, err)
		assert.Equal(t, tc.expected, u)
	}
	for _, base := range []string{"", "https://images.example/?token=secret", "ftp://images.example", "https://images.example/#fragment"} {
		_, err := sourceURL("image.zip", base)
		require.Error(t, err)
	}
	file := imageFile(t, t.TempDir(), "empty.zip", "")
	_, err := hashSource(context.Background(), file)
	require.ErrorContains(t, err, "empty")
	_, err = hashSource(context.Background(), t.TempDir())
	require.ErrorContains(t, err, "not a regular file")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = hashSource(ctx, file)
	require.ErrorIs(t, err, context.Canceled)
}

func TestInvalidPlanMakesNoImageRequests(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = fmt.Fprint(w, "different image contents for "+r.URL.Path)
	}))
	defer srv.Close()
	first, second := srv.URL+"/first.zip", srv.URL+"/second.zip"
	for _, action := range []string{"create", "add"} {
		for _, tc := range []struct {
			name, latest, message string
			inputs                []string
		}{
			{"unknown latest", srv.URL + "/missing.zip", "not in the manifest", []string{first}},
			{"invalid latest", "ftp://images.example/image.zip", "HTTP(S)", []string{first}},
			{"duplicate URL", "", "already in the manifest", []string{first, first}},
			{"duplicate identity", "", "already exists", []string{first, second}},
		} {
			t.Run(action+"/"+tc.name, func(t *testing.T) {
				requests.Store(0)
				opts := options{file: filepath.Join(t.TempDir(), "info.json"), board: "unoq", os: "debian", version: "1", latest: tc.latest}
				var before []byte
				if action == "add" {
					edit(t, "create", nil, options{file: opts.file})
					var err error
					before, err = os.ReadFile(opts.file)
					require.NoError(t, err)
				}
				_, err := editManifest(context.Background(), action, tc.inputs, opts, &cobra.Command{})
				require.ErrorContains(t, err, tc.message)
				assert.Zero(t, requests.Load(), "invalid plans must fail before downloading any image")
				after, err := os.ReadFile(opts.file)
				if action == "create" {
					require.ErrorIs(t, err, os.ErrNotExist)
				} else {
					require.NoError(t, err)
					assert.Equal(t, before, after)
				}
			})
		}
	}
}

func TestConflictingNormalizedIdentityPreservesManifest(t *testing.T) {
	dir := t.TempDir()
	first := imageFile(t, dir, "unoq-debian-20261101-1.zip", "first image")
	second := imageFile(t, dir, "custom.zip", "different image")
	opts := options{file: filepath.Join(dir, "info.json"), baseURL: "https://images.example"}
	edit(t, "create", []string{first}, opts)
	before, err := os.ReadFile(opts.file)
	require.NoError(t, err)
	opts.board, opts.os, opts.version = "UNO Q", "DEBIAN GNU/Linux 13 (trixie)", "20261101-1"
	_, err = editManifest(context.Background(), "add", []string{second}, opts, &cobra.Command{})
	require.ErrorContains(t, err, "already exists")
	require.ErrorContains(t, err, "different --version")
	after, err := os.ReadFile(opts.file)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestRemovedURLCannotRemainLatest(t *testing.T) {
	dir := t.TempDir()
	first := imageFile(t, dir, "unoq-debian-20261101-1.zip", "first image")
	second := imageFile(t, dir, "unoq-debian-20261101-2.zip", "second image")
	opts := options{file: filepath.Join(dir, "info.json"), baseURL: "https://images.example"}
	edit(t, "create", []string{first, second}, opts)
	before, err := os.ReadFile(opts.file)
	require.NoError(t, err)
	opts.latest, err = sourceURL(second, opts.baseURL)
	require.NoError(t, err)
	_, err = editManifest(context.Background(), "remove", []string{second}, opts, &cobra.Command{})
	require.ErrorContains(t, err, "not in the manifest")
	after, err := os.ReadFile(opts.file)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// Keep generated-manifest checks local, including the built-in index requests.
type manifestReaderTransport struct{}

func (manifestReaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() == "downloads.arduino.cc" {
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestGeneratedManifestResolvesUniqueImageIdentities(t *testing.T) {
	t.Setenv("ARDUINO_FLASHER_ADDITIONAL_URLS", "")
	t.Setenv("ARDUINO_FLASHER_ADDITIONAL_HEADERS", "")
	original := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: manifestReaderTransport{}}
	t.Cleanup(func() { http.DefaultClient = original })
	dir := t.TempDir()
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	first := imageFile(t, dir, "unoq-debian-20261101-1.zip", "first image")
	otherBoard := imageFile(t, dir, "ventunoq-debian-20261101-1.zip", "other board image")
	otherOS := imageFile(t, dir, "unoq-ubuntu-20261101-1.zip", "other OS image")
	opts := options{file: filepath.Join(dir, "info.json"), baseURL: srv.URL, os: "Debian GNU/Linux 13 (trixie)"}
	edit(t, "create", []string{first}, opts)
	opts.os = ""
	edit(t, "add", []string{otherBoard, otherOS}, opts)
	releases, err := registry.NewClient(srv.URL + "/info.json").Fetch(context.Background())
	require.NoError(t, err)
	require.Len(t, releases, 3)
	for _, tc := range []struct{ board, os, file string }{
		{"unoq", "debian", first},
		{"ventunoq", "debian", otherBoard},
		{"unoq", "ubuntu", otherOS},
	} {
		r, err := releases.Resolve(tc.os, tc.board, "20261101-1")
		require.NoError(t, err)
		expected, err := sourceURL(tc.file, opts.baseURL)
		require.NoError(t, err)
		assert.Equal(t, expected, r.Url)
	}
}

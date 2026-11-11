// This file is part of arduino-flasher-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package list

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-flasher-cli/cmd/feedback"
	"github.com/arduino/arduino-flasher-cli/internal/cache"
	"github.com/arduino/arduino-flasher-cli/internal/registry"
)

type manifestTransport struct {
	releases   registry.Releases
	additional map[string]registry.Releases
	requests   []string
}

func (s *manifestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.requests = append(s.requests, req.URL.String())
	status := http.StatusOK
	var body []byte
	images, added := s.additional[req.URL.String()]
	if !added {
		images = s.releases
	}
	switch {
	case added || req.URL.String() == "https://downloads.arduino.cc/debian-im/Stable/info.json":
		var releases []map[string]string
		for _, rel := range images {
			board, ok := registry.BoardByID(rel.Board)
			if !ok {
				return nil, fmt.Errorf("unknown fixture board %s", rel.Board)
			}
			releases = append(releases, map[string]string{
				"board": board.Label, "os": rel.OS, "version": rel.Version,
				"url": rel.Url, "sha256": rel.Sha256,
			})
		}
		var err error
		body, err = json.Marshal(map[string]any{"releases": releases})
		if err != nil {
			return nil, err
		}
	case req.URL.String() == "https://downloads.arduino.cc/ubuntu-im/Stable/info.json":
		status = http.StatusNotFound
	default:
		return nil, fmt.Errorf("list must not download images: %s", req.URL)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

func setupList(t *testing.T) (*cache.Cache, *manifestTransport, registry.Release) {
	t.Helper()
	// Tests use their own cache and in-memory manifests, never the user's images
	// or the network. Environment and HTTP globals prevent parallel subtests.
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LocalAppData", t.TempDir())
	case "darwin":
		t.Setenv("HOME", t.TempDir())
	default:
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
	}
	t.Setenv("ARDUINO_FLASHER_ADDITIONAL_URLS", "")
	t.Setenv("ARDUINO_FLASHER_ADDITIONAL_HEADERS", "")
	sum := sha256.Sum256([]byte("test image"))
	rel := registry.Release{
		Board: registry.UnoQ.ID, OS: "debian", Version: "20250915-173",
		Url: "https://images.example/image.tar.zst", FileName: "image.tar.zst",
		Sha256: hex.EncodeToString(sum[:]),
	}
	transport := &manifestTransport{releases: registry.Releases{rel}}
	original := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: transport}
	t.Cleanup(func() { http.DefaultClient = original })
	c, err := cache.Default()
	require.NoError(t, err)
	return c, transport, rel
}

func seedListCache(t *testing.T, c *cache.Cache, rel registry.Release) string {
	t.Helper()
	archive, err := c.Store(context.Background(), rel, func(_ context.Context, dir string) error {
		return os.WriteFile(filepath.Join(dir, rel.FileName), []byte("test image"), 0o600)
	})
	require.NoError(t, err)
	return archive
}

func executeList(t *testing.T, args ...string) string {
	t.Helper()
	var output bytes.Buffer
	feedback.SetOut(&output)
	t.Cleanup(func() { feedback.SetOut(os.Stdout) })
	cmd := NewListCmd()
	cmd.SetArgs(args)
	require.NoError(t, cmd.ExecuteContext(context.Background()))
	return output.String()
}

func requireCacheMark(t *testing.T, output, version string, cached bool) {
	t.Helper()
	require.Contains(t, output, "CACHED")
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == version {
			// This fixture has one release per board/OS, so it always has a
			// LATEST mark. A cached release has a second mark in CACHED.
			marks := 1
			if cached {
				marks++
			}
			require.Equal(t, marks, strings.Count(line, "✓"), line)
			return
		}
	}
	t.Fatalf("version %s missing from output:\n%s", version, output)
}

func TestListCacheIdentity(t *testing.T) {
	for _, change := range []string{"matching", "different board", "different OS", "different version", "different checksum", "different archive name", "case-insensitive metadata"} {
		t.Run(change, func(t *testing.T) {
			c, transport, rel := setupList(t)
			seedListCache(t, c, rel)
			online := rel
			wantCached := change == "matching"
			switch change {
			case "different board":
				online.Board = registry.VentunoQ.ID
			case "different OS":
				online.OS = "ubuntu"
			case "different version":
				online.Version = "20250916-174"
			case "different checksum":
				online.Sha256 = strings.Repeat("a", 64)
			case "different archive name":
				online.Url = "https://images.example/renamed.tar.zst"
			case "case-insensitive metadata":
				wantCached = true
				online.OS = "Debian GNU/Linux 13 (trixie)"
				online.Sha256 = strings.ToUpper(online.Sha256)
			}
			transport.releases = registry.Releases{online}
			output := executeList(t)
			requireCacheMark(t, output, online.Version, wantCached)
			require.Len(t, transport.requests, 2, "list should only read the two published indexes once")
		})
	}
}

func TestListMissingAndIncompleteCache(t *testing.T) {
	for _, state := range []string{"missing cache", "missing metadata", "missing archive", "wrong size"} {
		t.Run(state, func(t *testing.T) {
			c, _, rel := setupList(t)
			cacheMissing := state == "missing cache"
			if !cacheMissing {
				archive := seedListCache(t, c, rel)
				switch state {
				case "missing metadata":
					require.NoError(t, os.Remove(filepath.Join(filepath.Dir(archive), "meta.json")))
				case "missing archive":
					require.NoError(t, os.Remove(archive))
				case "wrong size":
					require.NoError(t, os.Truncate(archive, 3))
				}
			}
			requireCacheMark(t, executeList(t), rel.Version, false)
			if cacheMissing {
				require.NoDirExists(t, c.Dir(), "listing should not create the cache")
			}
		})
	}
}

func TestListDoesNotHashArchives(t *testing.T) {
	c, _, rel := setupList(t)
	archive := seedListCache(t, c, rel)
	require.NoError(t, os.WriteFile(archive, []byte("corrupted!"), 0o600))
	requireCacheMark(t, executeList(t), rel.Version, true)
	_, err := c.Lookup(context.Background(), rel)
	require.ErrorIs(t, err, cache.ErrMiss, "flashing still verifies same-size corruption")
}

func TestListUnavailableCachePreservesOnlineImages(t *testing.T) {
	c, _, rel := setupList(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(c.Dir()), 0o750))
	require.NoError(t, os.WriteFile(c.Dir(), []byte("not a directory"), 0o600))
	requireCacheMark(t, executeList(t), rel.Version, false)
}

func TestListFiltersWithCacheMarks(t *testing.T) {
	c, transport, rel := setupList(t)
	seedListCache(t, c, rel)
	other := rel
	other.Board, other.OS = registry.VentunoQ.ID, "ubuntu"
	transport.releases = append(transport.releases, other)
	output := executeList(t, "unoq", "--os", "debian")
	requireCacheMark(t, output, rel.Version, true)
	require.NotContains(t, output, "ventunoq")
	require.NotContains(t, output, "ubuntu")
}

func TestListCombinesBoardsAndOnlyShowsSourcesWithFlag(t *testing.T) {
	for _, withSources := range []bool{false, true} {
		t.Run(fmt.Sprintf("sources=%t", withSources), func(t *testing.T) {
			_, transport, uno := setupList(t)
			ventuno := uno
			ventuno.Board, ventuno.OS, ventuno.Version = "ventunoq", "ubuntu", "20250917-175"
			additionalURL := "http://lan-host.local:8080/info.json"
			custom := uno
			custom.Version = "20250916-174"
			transport.additional = map[string]registry.Releases{
				"https://downloads.arduino.cc/ubuntu-im/Stable/info.json": {ventuno},
				additionalURL: {custom},
			}
			var args []string
			if withSources {
				args = []string{"--additional-url", additionalURL}
			} else {
				t.Setenv("ARDUINO_FLASHER_ADDITIONAL_URLS", additionalURL)
			}
			output := executeList(t, args...)
			if withSources {
				sections := strings.Split(output, "\n\n")
				require.Contains(t, sections[0], "unoq")
				require.Contains(t, sections[0], "ventunoq")
				require.NotContains(t, sections[0], "Index:")
				require.NotContains(t, output, "downloads.arduino.cc")
				require.NotContains(t, sections[0], custom.Version)
				require.Equal(t, 1, strings.Count(output, "ARDUINO OFFICIAL"))
				require.Contains(t, sections[1], custom.Version)
			} else {
				require.Contains(t, output, "unoq")
				require.Contains(t, output, "ventunoq")
				require.Contains(t, output, custom.Version, "environment indexes still participate without enabling source headings")
				require.Equal(t, 1, strings.Count(output, "BOARD"), "both boards belong to the same table")
				require.NotContains(t, output, "SOURCE")
				require.NotContains(t, output, "Index:")
				require.NotContains(t, output, "https://")
				require.NotContains(t, output, "across all sources")
			}
		})
	}
}

func TestListGroupsBySourceAndHighlightsTrust(t *testing.T) {
	_, transport, rel := setupList(t)
	additionalURL := "http://lan-host.local:8080/info.json"
	custom := rel
	custom.Version = "20250916-174"
	transport.additional = map[string]registry.Releases{additionalURL: {custom}}
	output := executeList(t, "unoq", "--os", "debian", "--additional-url", additionalURL)
	sections := strings.Split(output, "\n\n")
	require.GreaterOrEqual(t, len(sections), 2)
	require.Contains(t, sections[0], "ARDUINO OFFICIAL")
	require.NotContains(t, sections[0], "Index:")
	require.NotContains(t, output, "downloads.arduino.cc")
	require.Contains(t, sections[0], rel.Version)
	require.NotContains(t, sections[0], custom.Version)
	require.Contains(t, sections[1], "UNOFFICIAL / UNVALIDATED")
	require.Contains(t, sections[1], additionalURL)
	require.Contains(t, sections[1], custom.Version)
	require.NotContains(t, sections[1], rel.Version)
	require.NotContains(t, output, "https://images.example/image.tar.zst", "headings must identify indexes, not archive hosts")
	require.Contains(t, output, "LATEST is calculated across all sources")
	require.Len(t, transport.requests, 3)
}

func TestListSourceHeadingsDoNotExposeCredentials(t *testing.T) {
	result := listResult{showSources: true, Releases: []listRelease{
		{Board: "unoq", Os: "debian", Version: "one", SourceURL: "https://user:secret@example.com/info.json?token=private"},
		{Board: "unoq", Os: "debian", Version: "two", SourceURL: "https://user:secret@example.com/info.json?token=other"},
	}}
	output := result.String()
	require.Contains(t, output, "SOURCE 1")
	require.Contains(t, output, "SOURCE 2")
	require.Contains(t, output, "https://example.com/info.json?[query redacted]")
	require.NotContains(t, output, "user:")
	require.NotContains(t, output, "secret")
	require.NotContains(t, output, "token=")
	require.NotContains(t, output, "private")
}

func TestListResultJSONIncludesCacheStatus(t *testing.T) {
	result := listResult{Releases: []listRelease{
		{Version: "cached", Cached: true, SourceURL: "https://downloads.arduino.cc/debian-im/Stable/info.json", Official: true},
		{Version: "not-cached", Cached: false, SourceURL: "http://lan-host.local:8080/info.json"},
	}}
	data, err := json.Marshal(result.Data())
	require.NoError(t, err)
	var decoded struct {
		Releases []struct {
			Version   string `json:"version"`
			Cached    *bool  `json:"cached"`
			SourceURL string `json:"source_url"`
			Official  *bool  `json:"official"`
		} `json:"releases"`
	}
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Len(t, decoded.Releases, 2)
	for i, release := range decoded.Releases {
		require.NotNil(t, release.Cached, "the JSON field must be present even when false")
		require.Equal(t, result.Releases[i].Cached, *release.Cached)
		require.Equal(t, result.Releases[i].SourceURL, release.SourceURL)
		require.NotNil(t, release.Official, "official must be present even when false")
		require.Equal(t, result.Releases[i].Official, *release.Official)
	}
}

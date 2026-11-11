// This file is part of arduino-flasher-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/arduino/arduino-flasher-cli/cmd/feedback"
	"github.com/arduino/arduino-flasher-cli/cmd/i18n"
	"github.com/arduino/arduino-flasher-cli/internal/registry"
)

type options struct {
	file, baseURL, board, os, version, latest string
}

// NewManifestCmd creates commands for authoring published image indexes.
func NewManifestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "manifest", Short: "Create and edit image manifests (info.json)", Args: cobra.NoArgs,
	}
	for _, action := range []string{"create", "add", "remove", "latest"} {
		cmd.AddCommand(newEditCmd(action))
	}
	return cmd
}

func newEditCmd(action string) *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use: action + " [files, directories or URLs...]",
		Short: map[string]string{
			"create": "Create a new manifest without overwriting an existing file",
			"add":    "Add images to an existing manifest",
			"remove": "Remove images from a manifest without deleting their files",
			"latest": "Select an existing image URL as the manifest's latest entry",
		}[action],
		Args: cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			m, err := editManifest(cmd.Context(), action, args, opts, cmd)
			if err != nil {
				feedback.Fatal(i18n.Tr("error editing the manifest: %v", err), feedback.ErrBadArgument)
			}
			feedback.PrintResult(editResult{File: opts.file, Releases: len(m.Releases), Latest: m.Latest})
		},
	}
	fileFlag := "manifest"
	if action == "create" {
		fileFlag = "output"
		cmd.Args = cobra.ArbitraryArgs
	}
	cmd.Flags().StringVar(&opts.file, fileFlag, "info.json", "Manifest file path")
	if action == "remove" {
		cmd.Use = "remove <files or URLs...>"
	}
	if action == "latest" {
		cmd.Use = "latest <URL>"
		cmd.Args = cobra.ExactArgs(1)
	} else {
		cmd.Flags().StringVar(&opts.baseURL, "base-url", "", "Published HTTP(S) directory for local files (default: http://<hostname>.local/)")
		cmd.Flags().StringVar(&opts.latest, "latest", "", "Existing or added URL to select as latest")
	}
	if action == "create" || action == "add" {
		cmd.Long = cmd.Short + `. Directories are scanned for image archives, without recursion.
Board, OS and build version are detected from filenames. Missing metadata is
asked for interactively; use --board, --os and --version for scripts or overrides.
Remote files are streamed in full to calculate SHA-256, but are not saved.
`
		cmd.Flags().StringVar(&opts.board, "board", "", "Board ID or label (overrides filename detection)")
		cmd.Flags().StringVar(&opts.os, "os", "", "Target Linux distribution (overrides filename detection)")
		cmd.Flags().StringVar(&opts.version, "version", "", "Image version (overrides filename detection)")
	}
	return cmd
}

func editManifest(ctx context.Context, action string, inputs []string, opts options, cmd *cobra.Command) (registry.Manifest, error) {
	m := registry.Manifest{Releases: []registry.ManifestRelease{}}
	if action == "create" {
		if _, err := os.Lstat(opts.file); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = fmt.Errorf("manifest already exists: %s", opts.file)
			}
			return m, err
		}
	} else {
		var err error
		m, err = readManifest(opts.file)
		if err != nil {
			return m, err
		}
	}

	latest := opts.latest
	if action == "latest" {
		latest = inputs[0]
		inputs = nil
	} else if action != "remove" {
		var err error
		inputs, err = expandSources(inputs)
		if err != nil {
			return m, err
		}
	}

	// Validate the resulting URL set before prompting or reading image contents.
	planned := make(map[string]bool, len(m.Releases)+len(inputs))
	for _, r := range m.Releases {
		planned[r.Url] = true
	}
	urls := make([]string, len(inputs))
	for i, input := range inputs {
		u, err := sourceURL(input, opts.baseURL)
		if err != nil {
			return m, err
		}
		urls[i] = u
		if action == "remove" {
			if !planned[u] {
				return m, fmt.Errorf("URL is not in the manifest: %s", u)
			}
			delete(planned, u)
		} else {
			if planned[u] {
				return m, fmt.Errorf("URL is already in the manifest: %s", u)
			}
			planned[u] = true
		}
	}
	if latest == "" && m.Latest != nil && planned[m.Latest.Url] {
		latest = m.Latest.Url
	}
	if latest != "" {
		u, err := httpURL(latest)
		if err != nil {
			return m, err
		}
		latest = u.String()
		if !planned[latest] {
			return m, fmt.Errorf("URL is not in the manifest: %s", latest)
		}
	}

	firstNew := len(m.Releases)
	for i, input := range inputs {
		if action == "remove" {
			index := slices.IndexFunc(m.Releases, func(r registry.ManifestRelease) bool { return r.Url == urls[i] })
			m.Releases = slices.Delete(m.Releases, index, index+1)
			continue
		}
		r, err := describeSource(cmd, input, urls[i], opts)
		if err != nil {
			return m, err
		}
		if slices.ContainsFunc(m.Releases, func(existing registry.ManifestRelease) bool {
			return existing.Board == r.Board && registry.OSName(existing.Os) == registry.OSName(r.Os) && existing.Version == r.Version
		}) {
			return m, fmt.Errorf("image for %s, %s, version %s already exists; remove the previous entry or specify a different --version", r.Board, registry.OSName(r.Os), r.Version)
		}
		m.Releases = append(m.Releases, r)
	}
	if action != "remove" {
		for i, input := range inputs {
			feedback.Print(i18n.Tr("Calculating SHA-256 for %s...", input))
			hash, err := hashSource(ctx, input)
			if err != nil {
				return m, fmt.Errorf("could not hash %s: %w", input, err)
			}
			m.Releases[firstNew+i].Sha256 = hash
		}
	}
	if err := m.SetLatest(latest); err != nil {
		return m, err
	}
	if err := ctx.Err(); err != nil {
		return m, err
	}
	return m, writeManifest(opts.file, m, action == "create")
}

func readManifest(file string) (registry.Manifest, error) {
	var m registry.Manifest
	info, err := os.Lstat(file)
	if err != nil {
		return m, err
	}
	if !info.Mode().IsRegular() {
		return m, fmt.Errorf("manifest must be a regular file: %s", file)
	}
	f, err := os.Open(file)
	if err != nil {
		return m, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	// Reject unfamiliar fields rather than silently deleting them when saving.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return m, fmt.Errorf("manifest must contain a single JSON object")
	}
	if m.Releases == nil {
		return m, fmt.Errorf("manifest must contain a releases array")
	}
	seen := make(map[string]bool)
	for i, r := range m.Releases {
		hash, err := hex.DecodeString(r.Sha256)
		if err != nil || len(hash) != sha256.Size {
			return m, fmt.Errorf("invalid SHA-256 for %s", r.Url)
		}
		u, err := httpURL(r.Url)
		if err != nil {
			return m, err
		}
		r.Url = u.String()
		r.Board, err = boardLabel(r.Board)
		if err != nil {
			return m, err
		}
		if strings.TrimSpace(r.Os) == "" || strings.TrimSpace(r.Version) == "" {
			return m, fmt.Errorf("missing OS or version for %s", r.Url)
		}
		if seen[r.Url] {
			return m, fmt.Errorf("duplicate URL in manifest: %s", r.Url)
		}
		seen[r.Url] = true
		m.Releases[i] = r
	}
	if m.Latest != nil {
		u, err := httpURL(m.Latest.Url)
		if err != nil {
			return m, err
		}
		m.Latest.Url = u.String()
	}
	return m, nil
}

// shortcut: one writer per manifest, add file locking before concurrent editing.
func writeManifest(file string, m registry.Manifest, create bool) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	var f *os.File
	if create {
		f, err = os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	} else {
		f, err = os.CreateTemp(filepath.Dir(file), ".manifest-*.tmp")
	}
	if err != nil {
		return err
	}
	saved := false
	defer func() {
		_ = f.Close()
		if !saved {
			_ = os.Remove(f.Name())
		}
	}()
	if !create {
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		if err := f.Chmod(info.Mode().Perm()); err != nil {
			return err
		}
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !create {
		if err := os.Rename(f.Name(), file); err != nil {
			return err
		}
	}
	saved = true
	return nil
}

type editResult struct {
	File     string                    `json:"manifest"`
	Releases int                       `json:"releases"`
	Latest   *registry.ManifestRelease `json:"latest,omitempty"`
}

func (r editResult) Data() interface{} { return r }
func (r editResult) String() string {
	return i18n.Tr("Manifest saved: %s (%d images)", r.File, r.Releases)
}

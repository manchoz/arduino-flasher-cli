// This file is part of arduino-flasher-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package manifest

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/arduino/arduino-flasher-cli/cmd/feedback"
	"github.com/arduino/arduino-flasher-cli/cmd/i18n"
	"github.com/arduino/arduino-flasher-cli/internal/registry"
)

func httpURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Fragment != "" {
		return nil, fmt.Errorf("expected an absolute HTTP(S) URL without a fragment: %s", raw)
	}
	return u, nil
}

func sourceURL(input, baseURL string) (string, error) {
	if strings.Contains(input, "://") {
		u, err := httpURL(input)
		if err != nil {
			return "", err
		}
		return u.String(), nil
	}
	if baseURL == "" {
		// shortcut: assumes hostname.local is advertised, query mDNS if custom or collision-renamed names need detection.
		hostname, err := os.Hostname()
		if err != nil {
			return "", fmt.Errorf("could not get the current hostname: %w", err)
		}
		hostname, _, _ = strings.Cut(hostname, ".")
		if hostname == "" {
			return "", fmt.Errorf("current hostname is empty; specify --base-url")
		}
		baseURL = "http://" + hostname + ".local/"
	}
	u, err := httpURL(baseURL)
	if err != nil {
		return "", err
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("--base-url must not contain a query string")
	}
	return strings.TrimRight(u.String(), "/") + "/" + url.PathEscape(filepath.Base(input)), nil
}

func expandSources(inputs []string) ([]string, error) {
	var sources []string
	for _, input := range inputs {
		if strings.Contains(input, "://") {
			if _, err := httpURL(input); err != nil {
				return nil, err
			}
			sources = append(sources, input)
			continue
		}
		info, err := os.Stat(input)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("not a regular file: %s", input)
			}
			sources = append(sources, input)
			continue
		}
		entries, err := os.ReadDir(input)
		if err != nil {
			return nil, err
		}
		before := len(sources)
		for _, entry := range entries {
			if entry.Type().IsRegular() && isImageArchive(entry.Name()) {
				sources = append(sources, filepath.Join(input, entry.Name()))
			}
		}
		if len(sources) == before {
			return nil, fmt.Errorf("no image archives in directory: %s", input)
		}
	}
	return sources, nil
}

func isImageArchive(name string) bool {
	for _, ext := range []string{".tar.zst", ".tar.gz", ".tgz", ".tar.xz", ".tar.bz2", ".tar", ".zip", ".img", ".img.zst", ".img.gz", ".img.xz"} {
		if strings.HasSuffix(strings.ToLower(name), ext) {
			return true
		}
	}
	return false
}

func hashSource(ctx context.Context, input string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.Contains(input, "://") {
		u, err := httpURL(input)
		if err != nil {
			return "", err
		}
		return registry.NewClient().SHA256(ctx, u.String())
	}
	f, err := os.Open(input)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", input)
	}
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", fmt.Errorf("file is empty")
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

var (
	boardPattern   = regexp.MustCompile(`(?i)\b(ventuno[-. ]?q|uno[-. ]?q)\b`)
	osPattern      = regexp.MustCompile(`(?i)\b(debian|ubuntu|fedora|alpine|arch|opensuse|yocto)\b`)
	versionPattern = regexp.MustCompile(`\b([0-9]{8}-[0-9]+)\b`)
)

func boardLabel(value string) (string, error) {
	for _, id := range registry.BoardIDs() {
		board, _ := registry.BoardByID(id)
		if strings.EqualFold(strings.TrimSpace(value), id) || strings.EqualFold(strings.TrimSpace(value), board.Label) {
			return board.Label, nil
		}
	}
	return "", fmt.Errorf("unknown board %q; use one of: %s", value, strings.Join(registry.BoardIDs(), ", "))
}

// shortcut: detection uses filenames only, inspect archive metadata if publishers need it.
func describeSource(cmd *cobra.Command, input, publishedURL string, opts options) (registry.ManifestRelease, error) {
	name := filepath.Base(input)
	if strings.Contains(input, "://") {
		u, _ := url.Parse(input) // sourceURL has already validated it.
		name = path.Base(u.Path)
	}
	detect := func(pattern *regexp.Regexp) string {
		matches := pattern.FindAllStringSubmatch(strings.ReplaceAll(name, "_", "-"), -1)
		if len(matches) == 1 {
			return matches[0][1]
		}
		return ""
	}
	board := strings.TrimSpace(opts.board)
	if board == "" {
		board = strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(detect(boardPattern))
	}
	if board != "" {
		var err error
		board, err = boardLabel(board)
		if err != nil {
			return registry.ManifestRelease{}, err
		}
	}
	r := registry.ManifestRelease{
		Url: publishedURL, Board: board,
		Os:      cmp.Or(strings.TrimSpace(opts.os), strings.ToLower(detect(osPattern))),
		Version: cmp.Or(strings.TrimSpace(opts.version), detect(versionPattern)),
	}
	var fields []huh.Field
	var missing []string
	if r.Board == "" {
		missing = append(missing, "--board")
		feedback.Warning(i18n.Tr("Could not detect the board for %s. Specify --board or select it below.", name))
		var choices []huh.Option[string]
		for _, id := range registry.BoardIDs() {
			b, _ := registry.BoardByID(id)
			choices = append(choices, huh.NewOption(b.Label, b.Label))
		}
		fields = append(fields, huh.NewSelect[string]().Title(i18n.Tr("Target board")).Options(choices...).Value(&r.Board))
	}
	required := func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("a value is required")
		}
		return nil
	}
	if r.Os == "" {
		missing = append(missing, "--os")
		feedback.Warning(i18n.Tr("Could not detect the OS for %s. Specify --os or enter it below.", name))
		fields = append(fields, huh.NewInput().Title(i18n.Tr("Target Linux distribution")).Value(&r.Os).Validate(required))
	}
	if r.Version == "" {
		missing = append(missing, "--version")
		feedback.Warning(i18n.Tr("Could not detect the version for %s. Specify --version or enter it below.", name))
		fields = append(fields, huh.NewInput().Title(i18n.Tr("Image version")).Value(&r.Version).Validate(required))
	}
	if len(fields) > 0 {
		in, ok := cmd.InOrStdin().(interface{ Fd() uintptr })
		if !ok || !term.IsTerminal(int(in.Fd())) {
			return r, fmt.Errorf("missing metadata for %s; specify %s (interactive prompts require a terminal)", name, strings.Join(missing, ", "))
		}
		form := huh.NewForm(huh.NewGroup(fields...)).WithInput(cmd.InOrStdin()).WithOutput(cmd.ErrOrStderr())
		if err := form.RunWithContext(cmd.Context()); err != nil {
			return r, err
		}
	}
	r.Os, r.Version = strings.TrimSpace(r.Os), strings.TrimSpace(r.Version)
	return r, nil
}

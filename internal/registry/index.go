// This file is part of arduino-flasher-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Manifest is the published info.json format, shared by the reader and editor.
type Manifest struct {
	// Latest names the publisher's selected release for the index as a whole.
	// The flasher resolves versions separately for each board and distribution.
	Latest   *ManifestRelease  `json:"latest,omitempty"`
	Releases []ManifestRelease `json:"releases"`
}

// ManifestRelease is one entry in a published manifest.
type ManifestRelease struct {
	// Version is a build date and number, "20250807-136".
	Version string `json:"version"`
	// Url points at the image archive, named after the release.
	Url string `json:"url"`
	// Sha256 is the checksum of the archive, hex encoded.
	Sha256 string `json:"sha256"`
	// Board is the label of the board the image is built for, which is how an
	// index names a board: "UNO Q".
	Board string `json:"board,omitempty"`
	// Os spells out the exact distribution: "Debian GNU/Linux 13 (trixie)".
	Os string `json:"os,omitempty"`
	// Kernel is the kernel package the image ships. Unused, published for the
	// record.
	Kernel string `json:"kernel,omitempty"`
}

// parse turns an index into the releases the flasher works with. An entry it
// cannot use is passed over: a newly published board must not break reading the
// others, and one entry the publisher got wrong is no reason to lose the rest.
func (idx Manifest) parse() Releases {
	var releases Releases
	for _, r := range idx.Releases {
		board, ok := boardByLabel(r.Board)
		if !ok {
			continue
		}

		os := OSName(r.Os)

		// The download checks the archive against it, so a checksum that cannot
		// be right makes this release unusable.
		if sha256Byte, err := hex.DecodeString(r.Sha256); err != nil || len(sha256Byte) != sha256.Size {
			continue
		}

		relURL, err := url.Parse(r.Url)
		if err != nil {
			continue
		}

		releases = append(releases, Release{
			Version:  r.Version,
			Url:      r.Url,
			Sha256:   r.Sha256,
			Board:    board.ID,
			OS:       os,
			FileName: path.Base(relURL.Path),
		})
	}
	releases.sortNewestFirst()
	return releases
}

// SetLatest selects an existing URL, or the newest version when url is empty.
func (idx *Manifest) SetLatest(url string) error {
	var latest *ManifestRelease
	for _, r := range idx.Releases {
		if url != "" {
			if r.Url == url {
				idx.Latest = &r
				return nil
			}
		} else if latest == nil || compareVersions(r.Version, latest.Version) > 0 {
			latest = &r
		}
	}
	if url != "" {
		return fmt.Errorf("URL is not in the manifest: %s", url)
	}
	idx.Latest = latest
	return nil
}

// boardByLabel returns the board an index named, which it does by label.
func boardByLabel(label string) (Board, bool) {
	for _, board := range supported {
		if board.Label == label {
			return board, true
		}
	}
	return Board{}, false
}

// OSName normalizes a distribution label, e.g. "Debian GNU/Linux 13" to "debian".
func OSName(distro string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(distro), " ")
	return strings.ToLower(name)
}

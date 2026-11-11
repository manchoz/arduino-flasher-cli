// This file is part of arduino-flasher-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package registry

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SHA256 streams a remote file to calculate its checksum without saving it.
// It uses the same HTTP client and authentication headers as image downloads.
func (c *Client) SHA256(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	c.addHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %s", resp.Status)
	}
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return "", fmt.Errorf("server returned an HTML page, not an image file")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, resp.Body)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", fmt.Errorf("file is empty")
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

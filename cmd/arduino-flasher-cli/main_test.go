// This file is part of arduino-flasher-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"os/exec"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-flasher-cli/cmd/feedback"
)

func TestAdditionalURLFlags(t *testing.T) {
	for _, name := range []string{"list", "download", "flash"} {
		t.Run(name, func(t *testing.T) {
			root := newRootCmd()
			cmd, _, err := root.Find([]string{name})
			require.NoError(t, err)
			urls := []string{"https://a.example/info.json?channels=a,b", "https://b.example/info.json"}
			require.NoError(t, cmd.ParseFlags([]string{"--additional-url", urls[0], "--additional-url", urls[1]}))
			got, err := cmd.Flags().GetStringArray("additional-url")
			require.NoError(t, err)
			assert.Equal(t, urls, got)
			assert.Nil(t, cmd.Flags().Lookup("addtional-url"))
			require.ErrorContains(t, cmd.ParseFlags([]string{"--addtional-url", urls[0]}), "unknown flag")
		})
	}
}

func TestAdditionalURLWarning(t *testing.T) {
	// Feedback selects its output format once per process.
	if os.Getenv("ARDUINO_FLASHER_TEST_WARNING") != "1" {
		child := exec.Command(os.Args[0], "-test.run=^TestAdditionalURLWarning$")
		child.Env = append(os.Environ(), "ARDUINO_FLASHER_TEST_WARNING=1")
		output, err := child.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	var stderr bytes.Buffer
	feedback.SetErr(&stderr)
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"download"})
	require.NoError(t, err)
	// Skip downloads and the wizard; check the warning is shown before either starts.
	cmd.Run = func(cmd *cobra.Command, args []string) {
		assert.Contains(t, stderr.String(), "Images from --additional-url sources are unofficial and have not been validated by Arduino.")
	}
	root.SetArgs([]string{"download", "--additional-url", "https://a.example/info.json", "--additional-url", "https://b.example/info.json"})
	require.NoError(t, root.Execute())
	assert.Equal(t, "Images from --additional-url sources are unofficial and have not been validated by Arduino. Download and flash them only if you trust the source.\n", stderr.String())
}

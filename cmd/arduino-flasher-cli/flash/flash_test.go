// This file is part of arduino-flasher-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package flash

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBughopperSerialFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--bughopper-serial", "DP05DIAN"},
		{"unoq", "--bughopper-serial", "DP05DIAN", "--serial", "0x1A2B3C4D", "--yes"},
	} {
		cmd := NewFlashCmd()
		require.NoError(t, cmd.ParseFlags(args))
		serial, err := cmd.Flags().GetString("bughopper-serial")
		require.NoError(t, err)
		require.Equal(t, "DP05DIAN", serial)
	}
	serial, err := NewFlashCmd().Flags().GetString("bughopper-serial")
	require.NoError(t, err)
	require.Empty(t, serial)
}

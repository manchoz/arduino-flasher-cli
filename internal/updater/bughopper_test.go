// This file is part of arduino-flasher-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/stretchr/testify/require"
)

// Copies of the test binary stand in for both hardware commands.
func TestMain(m *testing.M) {
	logPath := os.Getenv("ARDUINO_FLASHER_TEST_COMMAND_LOG")
	if logPath == "" {
		os.Exit(m.Run())
	}
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	args := append([]string{name}, os.Args[1:]...)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	if err := json.NewEncoder(log).Encode(args); err != nil {
		panic(err)
	}
	_ = log.Close()
	if os.Getenv("ARDUINO_FLASHER_TEST_COMMAND_HANG") != "" {
		time.Sleep(time.Minute)
	}
	fail := os.Getenv("ARDUINO_FLASHER_TEST_COMMAND_FAIL")
	if fail != "" && (fail == name || (name == "pytactl" && fail == os.Args[2])) {
		fmt.Fprintln(os.Stderr, "simulated USB failure")
		os.Exit(2)
	}
	os.Exit(0)
}

func TestBughopperFlashSequence(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	binary, err := os.ReadFile(exe)
	require.NoError(t, err)
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	for _, name := range []string{"qdl", "pytactl"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name+suffix), binary, 0755))
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	edl := []string{"pytactl", "oneshot", "bootToEDL", "--serial", "DP05DIAN"}
	qdl := []string{"qdl", "--allow-missing", "--storage", "emmc", "prog_firehose_ddr.elf", "rawprogram0.xml", "patch0.xml", "--serial", "12345678"}
	boot := []string{"pytactl", "oneshot", "powerOn", "--serial", "DP05DIAN"}
	for _, tc := range []struct {
		name, serial, fail, board, errorText string
		want                                 [][]string
	}{
		{name: "manual", board: "ventunoq", want: [][]string{qdl}},
		{name: "success", serial: "DP05DIAN", board: "ventunoq", want: [][]string{edl, qdl, boot}},
		{name: "EDL failure", serial: "DP05DIAN", board: "ventunoq", fail: "bootToEDL", errorText: "could not enter EDL", want: [][]string{edl}},
		{name: "QDL failure", serial: "DP05DIAN", board: "ventunoq", fail: "qdl", errorText: "exit status 2", want: [][]string{edl, qdl}},
		{name: "boot failure", serial: "DP05DIAN", board: "ventunoq", fail: "powerOn", errorText: "image flashed successfully, but return to normal boot", want: [][]string{edl, qdl, boot}},
		{name: "GPT read failure", serial: "DP05DIAN", board: "unoq", fail: "qdl", errorText: "could not read the board's partition table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := t.TempDir()
			flash := filepath.Join(image, "flash")
			require.NoError(t, os.Mkdir(flash, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(flash, "rawprogram0.xml"), []byte(`<data><program filename="rootfs.img"/></data>`), 0600))
			logPath := filepath.Join(image, "commands.jsonl")
			t.Setenv("ARDUINO_FLASHER_TEST_COMMAND_LOG", logPath)
			t.Setenv("ARDUINO_FLASHER_TEST_COMMAND_FAIL", tc.fail)
			err := flashBoard(context.Background(), paths.New(filepath.Join(dir, "qdl"+suffix)), "305419896", paths.New(image), "test", tc.board, false, 0, tc.serial, func(FlashEvent) {})
			if tc.errorText == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.errorText)
			}
			log, err := os.Open(logPath)
			require.NoError(t, err)
			defer log.Close()
			var got [][]string
			decoder := json.NewDecoder(log)
			for decoder.More() {
				var args []string
				require.NoError(t, decoder.Decode(&args))
				got = append(got, args)
			}
			if tc.board == "unoq" {
				// The failed check happens before qdl writes anything, so the
				// board is returned to normal boot instead of staying in EDL.
				require.Len(t, got, 3)
				require.Equal(t, edl, got[0])
				require.Equal(t, []string{"qdl", "--storage", "emmc", "prog_firehose_ddr.elf"}, got[1][:4])
				require.Equal(t, "read.xml", filepath.Base(got[1][4]))
				require.Equal(t, boot, got[2])
			} else {
				require.Equal(t, tc.want, got)
			}
		})
	}
}

func TestValidateBughopperSerial(t *testing.T) {
	for _, ok := range []string{"DP05DIAN", "abc-123", "A1_b.c:d"} {
		require.NoError(t, validateBughopperSerial(ok), ok)
	}
	for _, bad := range []string{"", " ", "-h", "--config=x", "a b", "a\nb", "a;b", "../x"} {
		require.ErrorContains(t, validateBughopperSerial(bad), "invalid Bughopper serial", bad)
	}
}

func TestBughopperInvalidSerialRunsNothing(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.jsonl")
	t.Setenv("ARDUINO_FLASHER_TEST_COMMAND_LOG", logPath)
	image := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(image, "flash"), 0755))
	err := flashBoard(context.Background(), paths.New("qdl"), "", paths.New(image), "test", "ventunoq", false, 0, "--help", nil)
	require.ErrorContains(t, err, "invalid Bughopper serial")
	require.NoFileExists(t, logPath)
}

func TestBughopperTimeout(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	binary, err := os.ReadFile(exe)
	require.NoError(t, err)
	name := "pytactl"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), binary, 0755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARDUINO_FLASHER_TEST_COMMAND_LOG", filepath.Join(dir, "commands.jsonl"))
	t.Setenv("ARDUINO_FLASHER_TEST_COMMAND_HANG", "1")
	old := bughopperTimeout
	bughopperTimeout = 500 * time.Millisecond
	t.Cleanup(func() { bughopperTimeout = old })

	start := time.Now()
	err = runBughopper(context.Background(), "bootToEDL", "DP05DIAN", func(FlashEvent) {})
	require.Error(t, err)
	require.Less(t, time.Since(start), 30*time.Second)
}

func TestBughopperMissingCommand(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := runBughopper(context.Background(), "bootToEDL", "DP05DIAN", nil)
	require.ErrorContains(t, err, "install it and add it to PATH")
}

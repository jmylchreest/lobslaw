package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

func dispatchData(args []string) bool {
	i := findSubcmd(args, "data")
	if i < 0 {
		return false
	}
	if err := runData(args[i+1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		diagnosticf("data: %v\n", err)
		os.Exit(1)
	}
	return true
}

func runData(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lobslaw data inspect|migrate|accept-recovery --data-dir PATH [--output NEW_PATH] [--legacy-format main-v0|pr348-v0|pr348-early-v0]")
	}
	if args[0] != "inspect" && args[0] != "migrate" && args[0] != "accept-recovery" {
		return fmt.Errorf("unknown data command %q", args[0])
	}
	fs := newFlagSet("data "+args[0], flag.ContinueOnError)
	var local offlineStore
	local.bind(fs)
	profile := fs.String("legacy-format", "", "verified source Raft schema; required for ambiguous unversioned payloads")
	accept := fs.Bool("acknowledge-external-effects", false, "confirm pending work was reviewed and replay of external effects is understood")
	output := fs.String("output", "", "new recovery directory outside source; source is never changed")
	pos, err := parseFlagsAndPositionals(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("unexpected data argument")
	}
	if err := config.LoadDotenv(envOr("LOBSLAW_ENV", "")); err != nil {
		return err
	}
	path, err := local.resolveStatePath()
	if err != nil {
		return err
	}
	if filepath.Base(path) != "state.db" {
		return errors.New("data commands require the complete directory containing state.db, raft.db and snapshots")
	}
	key, err := local.resolveKey()
	if err != nil {
		return err
	}
	if args[0] == "accept-recovery" {
		if !*accept || *output != "" || *profile != "" {
			return errors.New("accept-recovery requires --acknowledge-external-effects and no output/legacy override")
		}
		return memory.AcceptDataRecovery(context.Background(), filepath.Dir(path), key)
	}
	if *accept {
		return errors.New("acknowledgement is only valid with accept-recovery")
	}
	var report memory.DataInspection
	if args[0] == "migrate" {
		if *output == "" {
			return errors.New("migrate requires --output; stop all cluster nodes and retain the source for rollback")
		}
		report, err = memory.MigrateData(context.Background(), filepath.Dir(path), *output, *profile, key)
	} else {
		if *output != "" {
			return errors.New("inspect does not accept --output")
		}
		report, err = memory.InspectData(context.Background(), filepath.Dir(path), *profile, key)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

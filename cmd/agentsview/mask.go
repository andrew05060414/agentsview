// ABOUTME: `mask` command group: one-shot masking of secrets already stored in
// ABOUTME: the archive. Reports contain counts and 8-hex fingerprints only.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/mask"
)

func newMaskCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "mask",
		Short:        "Mask secrets already stored in the archive",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newMaskSQLiteCommand())
	return cmd
}

func newMaskSQLiteCommand() *cobra.Command {
	var (
		dbPath, secretsDir, reportPath, registryOut string
		apply, dryRun, vacuum, verify, fullCheck    bool
		allowLive                                   bool
		workers                                     int
	)
	cmd := &cobra.Command{
		Use:   "sqlite",
		Short: "Mask secrets in a sessions.db (dry-run by default)",
		Long: `Replaces secrets stored in messages, tool calls, tool results, session titles
and recall entries with {{SECRET_xxxxxxxx}} placeholders (sha256 prefix of the
value; one-way, no mapping is kept), rewrites secret_findings to placeholder
form and rebuilds the full-text indexes.

Without --apply nothing is written: the command only counts. The database must
not be in use by a running daemon when --apply is given. Output never contains
a secret value or any part of one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dbPath == "" {
				return fmt.Errorf("--db is required")
			}
			if apply && dryRun {
				return fmt.Errorf("--apply and --dry-run are mutually exclusive")
			}
			abs, err := filepath.Abs(dbPath)
			if err != nil {
				return err
			}
			if home, herr := os.UserHomeDir(); herr == nil {
				live := filepath.Join(home, ".agentsview", "sessions.db")
				if strings.EqualFold(filepath.Clean(abs), filepath.Clean(live)) && apply && !allowLive {
					return fmt.Errorf("refusing to --apply to the live archive %s without --allow-live", abs)
				}
			}
			rules, err := mask.LoadRules()
			if err != nil {
				return err
			}
			var exact []mask.ExactValue
			if secretsDir != "" {
				all, err := mask.LoadExactDir(secretsDir)
				if err != nil {
					return fmt.Errorf("reading secrets dir: %w", err)
				}
				labels := map[string]bool{}
				for _, v := range all {
					if v.Strict {
						exact = append(exact, v)
						labels[v.Label] = true
					}
				}
				fmt.Fprintf(cmd.ErrOrStderr(),
					"exact values: %d candidates, %d strict across %d files (values stay in memory)\n",
					len(all), len(exact), len(labels))
			}
			ctx := context.Background()
			var d *db.DB
			if apply {
				d, err = db.OpenIsolated(ctx, abs)
			} else {
				d, err = db.OpenReadOnly(ctx, abs)
			}
			if err != nil {
				return fmt.Errorf("opening %s: %w", abs, err)
			}
			defer d.Close()
			logf := func(format string, a ...any) {
				fmt.Fprintf(cmd.ErrOrStderr(), "[%s] %s\n",
					time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
			}
			writeReport := func(rep *db.MaskBackfillReport) {
				if rep == nil || reportPath == "" {
					return
				}
				if b, merr := json.MarshalIndent(rep, "", " "); merr == nil {
					_ = os.WriteFile(reportPath, b, 0o600)
				}
			}
			rep, err := d.MaskBackfill(ctx, db.MaskBackfillOptions{
				Rules: rules, Exact: exact, Apply: apply, Workers: workers,
				Log: logf, RegistryOut: registryOut,
			})
			writeReport(rep)
			if err != nil {
				return err
			}
			if apply {
				logf("rebuilding full-text indexes")
				if err := d.MaskRebuildIndexes(ctx); err != nil {
					return err
				}
				if verify {
					logf("verifying indexes")
					checks, _ := d.MaskVerify(ctx, fullCheck)
					rep.Checks = map[string]interface{}{}
					for k, v := range checks {
						rep.Checks[k] = v
						logf("check %s: %s", k, v)
					}
				}
				if err := d.CheckpointWALTruncateWithRetry(ctx); err != nil {
					logf("checkpoint: %v", err)
				}
				if vacuum {
					logf("VACUUM")
					if err := d.Vacuum(ctx); err != nil {
						return err
					}
					_ = d.CheckpointWALTruncateWithRetry(ctx)
				}
				writeReport(rep)
			}
			printMaskSummary(cmd, rep)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&dbPath, "db", "", "Path of the sessions.db to process")
	f.StringVar(&secretsDir, "secrets-dir", "", "Directory of local secret files whose values are matched exactly (read into memory only)")
	f.BoolVar(&apply, "apply", false, "Write the changes (default: dry-run, nothing is written)")
	f.BoolVar(&dryRun, "dry-run", false, "Count only (the default)")
	f.StringVar(&reportPath, "report", "", "Write the JSON report (counts and fingerprints only) to this file")
	f.StringVar(&registryOut, "registry-out", "", "Write the sha256 registry of exact and closure values (hashes only) to this file")
	f.BoolVar(&vacuum, "vacuum", false, "After --apply, VACUUM to drop pages that still hold old text")
	f.BoolVar(&verify, "verify", true, "After --apply, run FTS integrity checks")
	f.BoolVar(&fullCheck, "full-check", false, "With --verify, also run PRAGMA integrity_check (reads the whole file)")
	f.BoolVar(&allowLive, "allow-live", false, "Allow --apply on the default live archive (stop the daemon first)")
	f.IntVar(&workers, "workers", 0, "Parallel workers (default: CPUs minus 2)")
	return cmd
}

func printMaskSummary(cmd *cobra.Command, rep *db.MaskBackfillReport) {
	out := cmd.OutOrStdout()
	mode := "dry-run (nothing written)"
	if rep.Apply {
		mode = "applied"
	}
	fmt.Fprintf(out, "mask sqlite: %s, %.0fs, rules %s\n", mode, rep.Seconds, rep.RulesID)
	fmt.Fprintf(out, "  rows scanned: %v\n", rep.Rows)
	for _, p := range []struct {
		name string
		st   db.MaskStats
	}{{"pass 1 (rules + exact values)", rep.RulesOnly}, {"pass 2 (+ value closure)", rep.Final}} {
		fmt.Fprintf(out, "  %s: sessions=%d locations=%d spans=%d distinct_values=%d rows_changed=%d\n",
			p.name, p.st.Sessions, p.st.Locations, p.st.Spans, p.st.DistinctValues, p.st.RowsChanged)
	}
	fmt.Fprintf(out, "  closure values added: %d, exact values loaded: %d\n", rep.ClosureValues, rep.ExactLoaded)
	if rep.Apply {
		fmt.Fprintf(out, "  rows updated: %v; secret_findings: %+v\n", rep.Updates, rep.Findings)
		for k, v := range rep.Checks {
			fmt.Fprintf(out, "  check %s: %v\n", k, v)
		}
	}
}

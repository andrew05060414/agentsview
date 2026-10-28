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
	"go.kenn.io/agentsview/internal/config"
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
	cmd.AddCommand(newMaskScanFileCommand())
	cmd.AddCommand(newMaskResidueCommand())
	cmd.AddCommand(newMaskExplainCommand())
	return cmd
}

func newMaskSQLiteCommand() *cobra.Command {
	var (
		dbPath, secretsDir, reportPath, registryOut, scanFile, locationsOut string
		apply, dryRun, vacuum, verify, fullCheck                            bool
		allowLive, verifyRaw                                                bool
		workers                                                             int
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
			// The same user classifier prefixes as the daemon, so opening the
			// archive does not see a changed classifier and re-backfill it.
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			applyClassifierConfig(cfg)
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
				Log: logf, RegistryOut: registryOut, ScanFile: scanFile, LocationsOut: locationsOut,
			})
			writeReport(rep)
			if err != nil {
				return err
			}
			if apply {
				rep.Checks = map[string]interface{}{}
				logf("rebuilding full-text indexes")
				if err := d.MaskRebuildIndexes(ctx); err != nil {
					return err
				}
				if verify {
					logf("verifying indexes")
					checks, _ := d.MaskVerify(ctx, fullCheck)
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
				if verifyRaw {
					logf("raw-byte residue scan of %s", abs)
					for _, p := range []string{abs, abs + "-wal"} {
						if _, serr := os.Stat(p); serr != nil {
							continue
						}
						res, serr := mask.ScanFile(p, rep.LiteralValues(), nil)
						if serr != nil {
							return serr
						}
						rep.Checks["raw_scan:"+filepath.Base(p)] = map[string]any{
							"bytes": res.Bytes, "full_value_hits": sumInts(res.Full), "fragment_hits": sumInts(res.Frag),
							"full_by_fp8": res.Full, "fragment_by_fp8": res.Frag, "full_pages": res.FullPages, "fragment_pages": res.FragPages,
						}
						logf("raw scan %s: %d bytes, full-value hits %d, fragment hits %d", filepath.Base(p), res.Bytes, sumInts(res.Full), sumInts(res.Frag))
					}
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
	f.StringVar(&scanFile, "scan-file", "", "Scan this file (for example a masked copy) for the run's literal values and report page numbers of remaining copies")
	f.StringVar(&locationsOut, "locations-out", "", "Write a CSV of masked locations (session, table, ordinal, call index, pass) for reconciliation")
	f.BoolVar(&verifyRaw, "verify-raw", false, "After --apply (and --vacuum), scan the database file and its WAL for remaining values")
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

func newMaskScanFileCommand() *cobra.Command {
	var file, secretsDir string
	var withRules bool
	cmd := &cobra.Command{
		Use:   "scanfile",
		Short: "Count remaining secret values in the raw bytes of a file",
		Long: `Streams a file (a database, its -wal file, a vectors.db) and counts how often the
known secret values (from --secrets-dir) and the rule patterns still occur in
its raw bytes. Prints counts and fingerprints only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return fmt.Errorf("--file is required")
			}
			var vals []string
			if secretsDir != "" {
				all, err := mask.LoadExactDir(secretsDir)
				if err != nil {
					return err
				}
				for _, v := range all {
					if v.Strict {
						vals = append(vals, v.Value)
					}
				}
			}
			var rules *mask.Masker
			if withRules {
				var err error
				if rules, err = mask.NewDefault(mask.Options{}); err != nil {
					return err
				}
			}
			res, err := mask.ScanFile(file, vals, rules)
			if err != nil {
				return err
			}
			return printFileScan(cmd, file, len(vals), res)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "File to scan")
	cmd.Flags().StringVar(&secretsDir, "secrets-dir", "", "Directory of local secret files (values stay in memory)")
	cmd.Flags().BoolVar(&withRules, "rules", false, "Also run the rule patterns on the raw bytes (slow; a value split across pages is missed)")
	return cmd
}

func printFileScan(cmd *cobra.Command, file string, nvals int, res *mask.FileScan) error {
	out := map[string]any{
		"file": file, "bytes": res.Bytes, "values_checked": nvals,
		"full_hit_pages": res.FullPages, "fragment_hit_pages_count": len(res.FragPages), "fragment_hit_pages": res.FragPages,
		"full_value_hits": sumInts(res.Full), "full_value_hits_by_fp8": res.Full,
		"fragment_hits": sumInts(res.Frag), "fragment_hits_by_fp8": res.Frag,
		"rule_pattern_hits": res.Rule,
	}
	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(b))
	return nil
}

func sumInts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func newMaskResidueCommand() *cobra.Command {
	var dbPath, secretsDir string
	cmd := &cobra.Command{
		Use:   "residue",
		Short: "Locate remaining secret values (and fragments) in the text fields of a masked sessions.db",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dbPath == "" || secretsDir == "" {
				return fmt.Errorf("--db and --secrets-dir are required")
			}
			all, err := mask.LoadExactDir(secretsDir)
			if err != nil {
				return err
			}
			var vals []string
			for _, v := range all {
				if v.Strict {
					vals = append(vals, v.Value)
				}
			}
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return err
			}
			applyClassifierConfig(cfg)
			d, err := db.OpenReadOnly(context.Background(), dbPath)
			if err != nil {
				return err
			}
			defer d.Close()
			hits, err := d.MaskResidue(context.Background(), vals, 0, 2000)
			if err != nil {
				return err
			}
			b, _ := json.MarshalIndent(map[string]any{"values_checked": len(vals), "hits": len(hits), "locations": hits}, "", " ")
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "sessions.db to check")
	cmd.Flags().StringVar(&secretsDir, "secrets-dir", "", "Directory of local secret files (values stay in memory)")
	return cmd
}

func newMaskExplainCommand() *cobra.Command {
	var dbPath, table, session string
	var ordinal int
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Show what the masker finds in one stored message or tool field (rule, gate decision, length, fingerprint)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := map[string]string{
				"messages":           "SELECT content FROM messages WHERE session_id = ? AND ordinal = ?",
				"tool_calls":         "SELECT tc.input_json || char(10) || COALESCE(tc.result_content, '') FROM tool_calls tc JOIN messages m ON m.id = tc.message_id WHERE tc.session_id = ? AND m.ordinal = ?",
				"tool_result_events": "SELECT content FROM tool_result_events WHERE session_id = ? AND tool_call_message_ordinal = ?",
			}[table]
			if q == "" || dbPath == "" || session == "" {
				return fmt.Errorf("--db, --session and --table (messages|tool_calls|tool_result_events) are required")
			}
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return err
			}
			applyClassifierConfig(cfg)
			d, err := db.OpenReadOnly(context.Background(), dbPath)
			if err != nil {
				return err
			}
			defer d.Close()
			rs, err := d.Reader().Query(context.Background(), q, session, ordinal)
			if err != nil {
				return err
			}
			defer rs.Close()
			m, err := mask.NewDefault(mask.Options{})
			if err != nil {
				return err
			}
			n := 0
			for rs.Next() {
				var text string
				if err := rs.Scan(&text); err != nil {
					return err
				}
				_, hits := m.Mask(text)
				for _, h := range hits {
					n++
					fmt.Fprintf(cmd.OutOrStdout(), "field %d: rule=%s excluded=%q len=%d fp8=%s\n", n, h.Rule, h.Excluded, h.Len, h.FP)
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d findings\n", n)
			return rs.Err()
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "sessions.db")
	cmd.Flags().StringVar(&table, "table", "messages", "messages | tool_calls | tool_result_events")
	cmd.Flags().StringVar(&session, "session", "", "session id")
	cmd.Flags().IntVar(&ordinal, "ordinal", 0, "message ordinal")
	return cmd
}

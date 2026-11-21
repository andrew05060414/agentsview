// Command muse-reconcile audits an offline Muse Personal agents directory.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

type counts struct {
	Parsed         int `json:"parsed"`
	Skipped        int `json:"skipped"`
	Failed         int `json:"failed"`
	Messages       int `json:"messages"`
	MalformedLines int `json:"malformed_lines"`
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: go run ./scripts/muse-reconcile <agents-root>")
		fmt.Fprintln(flag.CommandLine.Output(), "Reports aggregate transcript-file counts. Use a stable local copy of the agents directory.")
		fmt.Fprintln(flag.CommandLine.Output(), "Exit status: 1 for failed files or malformed records, 2 if the scan could not run or found no files.")
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, flag.Arg(0), os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, root string, output, diagnostics io.Writer) int {
	total, err := reconcile(ctx, root)
	if err != nil {
		fmt.Fprintln(diagnostics, "Muse audit did not finish reading the agents directory")
		return 2
	}
	if err := json.NewEncoder(output).Encode(total); err != nil {
		return 2
	}
	if total.Failed > 0 || total.MalformedLines > 0 {
		return 1
	}
	if total.Parsed+total.Skipped == 0 {
		fmt.Fprintln(diagnostics, "No Muse transcript files were found")
		return 2
	}
	return 0
}

func reconcile(ctx context.Context, root string) (counts, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return counts{}, err
	}
	provider, ok := parser.NewProvider(parser.AgentMusePersonal, parser.ProviderConfig{Roots: []string{root}})
	if !ok {
		return counts{}, errors.New("Muse Personal provider unavailable")
	}
	agents, err := os.ReadDir(root)
	if err != nil {
		return counts{}, err
	}
	var total counts
	seen := make(map[string]bool)
	for _, agent := range agents {
		if err := ctx.Err(); err != nil {
			return counts{}, err
		}
		if !agent.IsDir() || !strings.HasPrefix(agent.Name(), "agent-") {
			continue
		}
		dir := filepath.Join(root, agent.Name(), "sessions")
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return counts{}, err
		}
		if !info.IsDir() {
			continue
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return counts{}, err
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return counts{}, err
			}
			if file.Type().IsRegular() && strings.HasSuffix(file.Name(), ".jsonl") {
				auditFile(ctx, provider, root, filepath.Join(dir, file.Name()), seen, &total)
			}
		}
	}
	return total, ctx.Err()
}

func auditFile(ctx context.Context, provider parser.Provider, root, path string, seen map[string]bool, total *counts) {
	rel, err := filepath.Rel(root, path)
	if err != nil || !parser.MusePersonalSessionFileShape(rel) {
		total.Skipped++
		return
	}
	// The provider rejects unsupported versions. Classify them separately from
	// malformed or unreadable files in this audit.
	version, err := headerVersion(path)
	if err != nil {
		total.Failed++
		return
	}
	if version > 1 {
		total.Skipped++
		return
	}
	sources, err := provider.SourcesForChangedPath(ctx, parser.ChangedPathRequest{Path: path, WatchRoot: root})
	if err != nil || len(sources) != 1 || seen[sources[0].Key] {
		total.Failed++
		return
	}
	out, err := provider.Parse(ctx, parser.ParseRequest{Source: sources[0]})
	if err != nil || !out.ResultSetComplete || len(out.Results) != 1 || len(out.SourceErrors) != 0 {
		total.Failed++
		return
	}
	seen[sources[0].Key] = true
	result := out.Results[0].Result
	total.Parsed++
	total.Messages += len(result.Messages)
	total.MalformedLines += result.Session.MalformedLines
}

func headerVersion(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	// Limit the read even if the file has no header newline.
	line, err := bufio.NewReader(io.LimitReader(file, 16<<10)).ReadBytes('\n')
	if err != nil {
		return 0, err
	}
	var header struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(line, &header); err != nil {
		return 0, err
	}
	if header.Type != "session_header" || header.Version < 1 {
		return 0, errors.New("invalid Muse session header")
	}
	return header.Version, nil
}

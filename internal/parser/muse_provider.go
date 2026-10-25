package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Muse Code logs each session under a local-date directory tree:
// <root>/YYYY/MM/DD/<session-id>/session.jsonl, with subagent logs at
// <session-dir>/subagent/<child-id>/session.jsonl. Discovery descends only
// into that shape, which keeps tool output spills, approval-review logs, and
// the derived .msp-view-v1 cache out of the scan.
func newMuseProviderFactory(def AgentDef) ProviderFactory {
	return museProviderFactory{NewSourceSetFactory(
		def,
		museProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newMuseSourceSet(cfg.Roots) },
	)}
}

// museProviderFactory adds raw capture of session.jsonl to the source set
// provider. The source set already decides which files are sessions, so the
// plan reuses that discovery instead of walking the tree again.
type museProviderFactory struct{ ProviderFactory }

func (f museProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	return &museProvider{f.ProviderFactory.NewProvider(cfg).(*SourceSetProvider)}
}

type museProvider struct{ *SourceSetProvider }

var _ StreamingRawCaptureSourceProvider = (*museProvider)(nil)

func (p *museProvider) Definition() AgentDef {
	return p.SourceSetProvider.Definition()
}

func (p *museProvider) Capabilities() Capabilities {
	return p.SourceSetProvider.Capabilities()
}

func (p *museProvider) DiscoverRawCaptureSourcesEach(
	ctx context.Context, yield func(SourceRef) error,
) (bool, error) {
	err := p.DiscoverEach(withRawCaptureStreamingTraversal(ctx), yield)
	return err == nil, err
}

// PlanRawCapture captures one session.jsonl. Tool output spills,
// approval-review logs, and the derived .msp-view-v1 cache never reach this
// plan because discovery does not yield them.
func (p *museProvider) PlanRawCapture(
	ctx context.Context, source SourceRef,
) (RawCapturePlan, error) {
	if err := ctx.Err(); err != nil {
		return RawCapturePlan{}, err
	}
	src, ok := source.Opaque.(JSONLSource)
	if !ok || src.Root == "" || src.Path == "" || isS3URI(src.Root) ||
		!isMuseSourcePath(src.Root, src.Path) {
		return RawCapturePlan{}, invalidRawCapturePlan("muse source is not a local session log")
	}
	info, err := os.Lstat(src.Path)
	if err != nil {
		return RawCapturePlan{}, err
	}
	if !info.Mode().IsRegular() {
		return RawCapturePlan{}, invalidRawCapturePlan("muse session log is not a regular file")
	}
	rel, err := filepath.Rel(src.Root, src.Path)
	if err != nil {
		return RawCapturePlan{}, invalidRawCapturePlan(
			"resolve muse session log path: %s", rawCaptureFilesystemError(err),
		)
	}
	return RawCapturePlan{
		ConfiguredRoot: src.Root,
		CaptureRoot:    src.Root,
		SourceKey:      source.Key,
		Entries: []RawCaptureEntry{{
			Path: filepath.ToSlash(rel), LocalPath: src.Path, Appendable: true,
		}},
	}, nil
}

func newMuseSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentMuse, roots,
		WithRecursive(),
		WithDescendPath(isMuseDescendPath),
		WithIncludePath(isMuseSourcePath),
		WithProjectHint(func(root, path string) string { return "" }),
		WithSessionIDFromPath(func(root, path string) string {
			if !isMuseSourcePath(root, path) {
				return ""
			}
			return museSessionIDFromLogPath(path)
		}),
		WithParseFile(museParseFile),
		// The fold can move usage between a message and a standalone usage
		// event as a response's later records arrive, so each parse
		// replaces the stored session.
		WithForceReplace(),
	)
}

func museParseFile(
	ctx context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	result, err := parseMuseSession(ctx, path, req.Machine)
	if err != nil || result == nil {
		return nil, nil, err
	}
	if req.Fingerprint.Hash != "" {
		result.Session.File.Hash = req.Fingerprint.Hash
	}
	return []ParseResult{*result}, nil, nil
}

// museRelParts splits path into components relative to root, or returns
// nil when path is not below root.
func museRelParts(root, path string) []string {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	return strings.Split(filepath.ToSlash(rel), "/")
}

// isMuseDirChain reports whether parts is a prefix of the Muse session
// directory shape: year, month, day, session ID, then any number of
// subagent/<child-id> pairs.
func isMuseDirChain(parts []string) bool {
	for i, part := range parts {
		var ok bool
		switch {
		case i == 0:
			ok = museDigits(part, 4)
		case i == 1 || i == 2:
			ok = museDigits(part, 2)
		case i == 3:
			ok = IsValidSessionID(part)
		case i%2 == 0:
			ok = part == museSubagentDirName
		default:
			ok = IsValidSessionID(part)
		}
		if !ok {
			return false
		}
	}
	return true
}

func isMuseDescendPath(root, path string) bool {
	parts := museRelParts(root, path)
	return parts != nil && isMuseDirChain(parts)
}

func isMuseSourcePath(root, path string) bool {
	parts := museRelParts(root, path)
	if len(parts) < 5 || parts[len(parts)-1] != museLogName {
		return false
	}
	dirs := parts[:len(parts)-1]
	// A session directory ends on an ID, never on a bare subagent/ folder.
	return len(dirs)%2 == 0 && isMuseDirChain(dirs)
}

func museDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func museProviderCapabilities() Capabilities {
	caps := Capabilities{
		RawCapture: RawCaptureCapabilities{
			Support:  CapabilitySupported,
			Shape:    RawCaptureShapeFiles,
			Append:   RawCaptureAppendOne,
			Snapshot: RawCaptureSnapshotNone,
		},
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			GitBranch:            CapabilitySupported,
			Relationships:        CapabilitySupported,
			Subagents:            CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
	caps.Source.ForceReplaceOnParse = CapabilitySupported
	return caps
}

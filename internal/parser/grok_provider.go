package parser

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	_ Provider            = (*grokProvider)(nil)
	_ MultiFileStatHasher = (*grokProvider)(nil)
)

type grokProviderFactory struct {
	inner ProviderFactory
}

func (f grokProviderFactory) Definition() AgentDef {
	return f.inner.Definition()
}

func (f grokProviderFactory) Capabilities() Capabilities {
	return f.inner.Capabilities()
}

func (f grokProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	p := f.inner.NewProvider(cfg)
	ssp, ok := p.(*SourceSetProvider)
	if !ok {
		return p
	}
	return &grokProvider{SourceSetProvider: ssp}
}

type grokProvider struct {
	*SourceSetProvider
}

// ComputeMultiFileStatHash implements parser.MultiFileStatHasher across
// summary.json, every companion metadata/transcript file, and any sibling
// subagent parent metadata so warm sweeps skip unchanged sessions without
// re-hashing their contents.
func (p *grokProvider) ComputeMultiFileStatHash(path string) uint64 {
	return grokComputeMultiFileStatHash(path)
}

func newGrokProviderFactory(def AgentDef) ProviderFactory {
	return grokProviderFactory{
		inner: NewSingleFileProviderFactory(
			def,
			grokProviderCapabilities(),
			func(cfg ProviderConfig) singleFileSourceSet {
				return NewSingleFileSourceSet(
					AgentGrok,
					cfg.Roots,
					WithStreamingFileDiscovery(grokDiscoverEach),
					WithFileWatchRoots(grokWatchRoots),
					WithFileChangedPathClassifier(grokClassifyPath),
					WithFileLookup(func(_ context.Context, root, rawID string) (singleFileMatch, bool) {
						return grokFindFile(root, rawID)
					}),
					WithFileFingerprint(grokFingerprintSource),
					WithFileParse(grokParseFile),
				)
			},
		),
	}
}

func grokDiscoverEach(
	ctx context.Context, root string, yield func(singleFileMatch) error,
) error {
	return streamDirectoryEntries(ctx, root, func(cwd os.DirEntry) error {
		isCwdDir, dirErr := streamingDirCandidateOrIncomplete(
			AgentGrok, "Grok cwd directory", cwd, root,
		)
		if dirErr != nil {
			return dirErr
		}
		if !isCwdDir {
			return nil
		}
		cwdRoot := filepath.Join(root, cwd.Name())
		return streamDirectoryEntries(ctx, cwdRoot, func(session os.DirEntry) error {
			if !IsValidSessionID(session.Name()) {
				return nil
			}
			isSessionDir, sessionErr := streamingDirCandidateOrIncomplete(
				AgentGrok, "Grok session directory", session, cwdRoot,
			)
			if sessionErr != nil {
				return sessionErr
			}
			if !isSessionDir {
				return nil
			}
			if match, ok := grokStrictMatch(
				root, filepath.Join(cwdRoot, session.Name(), "summary.json"),
			); ok {
				return yield(match)
			}
			return nil
		})
	})
}

func grokWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:      root,
			Recursive: true,
			IncludeGlobs: []string{
				"summary.json",
				"signals.json",
				"chat_history.jsonl",
				"updates.jsonl",
				"prompt_context.json",
				"meta.json",
			},
			DebounceKey: string(AgentGrok) + ":sessions:" + root,
		})
	}
	return out
}

func grokClassifyPath(
	root, path string, allowMissing bool,
) (singleFileMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return singleFileMatch{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if match, ok := grokClassifySubagentMeta(root, path, parts, allowMissing); ok {
		return match, true
	}
	if len(parts) != 3 || !IsValidSessionID(parts[1]) ||
		!grokTrackedFileName(parts[2]) {
		return singleFileMatch{}, false
	}
	summaryPath := filepath.Join(root, parts[0], parts[1], "summary.json")
	if allowMissing {
		return singleFileMatch{
			Path:        summaryPath,
			ProjectHint: parts[0],
		}, true
	}
	return grokStrictMatch(root, summaryPath)
}

func grokFindFile(root, rawID string) (singleFileMatch, bool) {
	if !IsValidSessionID(rawID) {
		return singleFileMatch{}, false
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return singleFileMatch{}, false
	}
	for _, entry := range entries {
		if !isDirOrSymlink(entry, root) {
			continue
		}
		summaryPath := filepath.Join(root, entry.Name(), rawID, "summary.json")
		if match, ok := grokStrictMatch(root, summaryPath); ok {
			return match, true
		}
	}
	return singleFileMatch{}, false
}

func grokStrictMatch(root, path string) (singleFileMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return singleFileMatch{}, false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return singleFileMatch{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[2] != "summary.json" ||
		!IsValidSessionID(parts[1]) {
		return singleFileMatch{}, false
	}
	return singleFileMatch{
		Path:        path,
		ProjectHint: parts[0],
	}, true
}

func grokTrackedFileName(name string) bool {
	switch name {
	case "summary.json", "signals.json", "chat_history.jsonl", "updates.jsonl",
		"prompt_context.json":
		return true
	default:
		return false
	}
}

func grokClassifySubagentMeta(
	root, path string, parts []string, allowMissing bool,
) (singleFileMatch, bool) {
	if len(parts) != 5 || parts[2] != "subagents" || parts[4] != "meta.json" {
		return singleFileMatch{}, false
	}
	if !IsValidSessionID(parts[1]) || !IsValidSessionID(parts[3]) {
		return singleFileMatch{}, false
	}
	childID := parts[3]
	if _, child, ok := readGrokSubagentMeta(path); ok && child != "" {
		childID = child
	}
	summaryPath := filepath.Join(root, parts[0], childID, "summary.json")
	if match, ok := grokStrictMatch(root, summaryPath); ok {
		return match, true
	}
	if match, ok := grokFindFile(root, childID); ok {
		return match, true
	}
	if allowMissing {
		return singleFileMatch{
			Path:        summaryPath,
			ProjectHint: parts[0],
		}, true
	}
	return singleFileMatch{}, false
}

func grokFingerprintSource(ctx context.Context, src singleFileSource) (SourceFingerprint, error) {
	info, err := os.Stat(src.Path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf(
			"stat %s: source is a directory", src.Path,
		)
	}
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	h := sha256.New()
	if err := addSiblingMetadataFingerprintPart(
		ctx, h, "summary", src.Path, info,
	); err != nil {
		return SourceFingerprint{}, err
	}
	companions := grokCompanionFiles(src.Path)
	labels := make([]string, 0, len(companions))
	for label := range companions {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		companion := companions[label]
		companionInfo, err := siblingMetadataFileInfo(companion)
		if err != nil {
			return SourceFingerprint{}, err
		}
		if companionInfo == nil {
			continue
		}
		size += companionInfo.Size()
		if ts := companionInfo.ModTime().UnixNano(); ts > mtime {
			mtime = ts
		}
		if err := addSiblingMetadataFingerprintPart(
			ctx, h, label, companion, companionInfo,
		); err != nil {
			return SourceFingerprint{}, err
		}
	}
	if metaPath := grokParentSubagentMetaPath(src.Path); metaPath != "" {
		metaInfo, err := siblingMetadataFileInfo(metaPath)
		if err != nil {
			return SourceFingerprint{}, err
		}
		if metaInfo != nil {
			size += metaInfo.Size()
			if ts := metaInfo.ModTime().UnixNano(); ts > mtime {
				mtime = ts
			}
			if err := addSiblingMetadataFingerprintPart(
				ctx, h, "subagent_parent_meta", metaPath, metaInfo,
			); err != nil {
				return SourceFingerprint{}, err
			}
		}
	}
	return SourceFingerprint{
		Size:    size,
		MTimeNS: mtime,
		Hash:    hex.EncodeToString(h.Sum(nil)),
	}, nil
}

func grokCompanionFiles(summaryPath string) map[string]string {
	dir := filepath.Dir(summaryPath)
	return map[string]string{
		"signals":        filepath.Join(dir, "signals.json"),
		"chat_history":   filepath.Join(dir, "chat_history.jsonl"),
		"updates":        filepath.Join(dir, "updates.jsonl"),
		"prompt_context": filepath.Join(dir, "prompt_context.json"),
	}
}

func grokParseFile(
	ctx context.Context, src singleFileSource, req ParseRequest,
) ([]ParseResult, []string, error) {
	result, err := ParseGrokSummary(ctx, src.Path, req.Source.ProjectHint, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if req.Fingerprint.Size > 0 {
		result.Session.File.Size = req.Fingerprint.Size
	}
	if req.Fingerprint.MTimeNS > 0 {
		result.Session.File.Mtime = req.Fingerprint.MTimeNS
	}
	if req.Fingerprint.Hash != "" {
		result.Session.File.Hash = req.Fingerprint.Hash
	}
	return []ParseResult{result}, nil, nil
}

// GrokEffectiveMtime returns the max ModTime().UnixNano() across summary.json
// and its companion and subagent metadata files, matching grokFingerprintSource.
func GrokEffectiveMtime(summaryPath string, base os.FileInfo) int64 {
	mtime := base.ModTime().UnixNano()
	companions := grokCompanionFiles(summaryPath)
	labels := make([]string, 0, len(companions))
	for label := range companions {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		ci, err := siblingMetadataFileInfo(companions[label])
		if err != nil || ci == nil {
			continue
		}
		if ts := ci.ModTime().UnixNano(); ts > mtime {
			mtime = ts
		}
	}
	if metaPath := grokParentSubagentMetaPath(summaryPath); metaPath != "" {
		if mi, err := siblingMetadataFileInfo(metaPath); err == nil && mi != nil {
			if ts := mi.ModTime().UnixNano(); ts > mtime {
				mtime = ts
			}
		}
	}
	return mtime
}

func grokComputeMultiFileStatHash(summaryPath string) uint64 {
	clean := filepath.Clean(summaryPath)
	if filepath.Base(clean) != "summary.json" {
		return 0
	}
	info, err := os.Stat(clean)
	if err != nil || info.IsDir() {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte{0x67})

	var buf [32]byte
	writeStatTuple := func(label, p string, fi os.FileInfo) bool {
		ctime, ok := codexIndexChangeTime(p, fi)
		if !ok {
			return false
		}
		inode, _ := sourceFileIdentityForPath(p, fi)
		if inode == 0 {
			return false
		}
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", label, p)
		binary.LittleEndian.PutUint64(buf[:8], uint64(fi.Size()))
		binary.LittleEndian.PutUint64(buf[8:16], uint64(fi.ModTime().UnixNano()))
		binary.LittleEndian.PutUint64(buf[16:24], uint64(ctime))
		binary.LittleEndian.PutUint64(buf[24:32], inode)
		_, _ = h.Write(buf[:])
		return true
	}

	if !writeStatTuple("summary", clean, info) {
		return 0
	}
	companions := grokCompanionFiles(clean)
	labels := make([]string, 0, len(companions))
	for label := range companions {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		companion := companions[label]
		ci, err := siblingMetadataFileInfo(companion)
		if err != nil {
			return 0
		}
		if ci == nil {
			continue
		}
		if !writeStatTuple(label, companion, ci) {
			return 0
		}
	}
	if metaPath := grokParentSubagentMetaPath(clean); metaPath != "" {
		mi, err := siblingMetadataFileInfo(metaPath)
		if err != nil {
			return 0
		}
		if mi != nil && !writeStatTuple("subagent_parent_meta", metaPath, mi) {
			return 0
		}
	}

	sum := h.Sum64()
	if sum == 0 {
		return 1
	}
	return sum
}

func grokProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.MultiFileStatHash = CapabilitySupported
	return Capabilities{
		Source: source,
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			ToolResultEvents:     CapabilitySupported,
			TerminationStatus:    CapabilityNotApplicable,
			MalformedLineCount:   CapabilityNotApplicable,
			AggregateUsageEvents: CapabilitySupported,
		},
	}
}

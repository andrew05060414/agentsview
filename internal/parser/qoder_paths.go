package parser

const (
	qoderDotDir   = ".qoder"
	qoderCNDotDir = ".qoder-cn"
)

// qoderDefaultDirs returns platform-specific default directories that
// hold Qoder session transcripts. The IDE stores its data under
// an Electron-style user-data directory per platform:
//
//	macOS:   ~/Library/Application Support/Qoder
//	Linux:   ~/.config/Qoder
//	Windows: %APPDATA%\Qoder
//
// In recent versions Qoder also writes session transcripts to a
// SharedClientCache path (used as the actual storage location on
// Windows installs), so we list the legacy, CN, and SharedClientCache
// locations. Paths that don't exist on a given platform are skipped
// silently by discovery.
//
//	Windows: %APPDATA%\Qoder\SharedClientCache\cli\projects
//
// Qoder CLI CN stores the same project-scoped layout under
// ~/.qoder-cn/projects.
func qoderDefaultDirs() []string {
	return []string{
		// Legacy export paths (kept for users who enable the in-IDE export).
		qoderDotDir + "/projects",
		".qoderwork/projects",
		// Qoder CLI CN
		qoderCNDotDir + "/projects",
		// macOS
		"Library/Application Support/Qoder/SharedClientCache/cli/projects",
		// Linux
		".config/Qoder/SharedClientCache/cli/projects",
		// Windows
		"AppData/Roaming/Qoder/SharedClientCache/cli/projects",
	}
}

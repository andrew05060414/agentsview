//go:build windows

package main

// Windows ReadDirectoryChangesW opens a directory handle per watched folder
// rather than per-fd kqueue slots, so a higher cap avoids unnecessary
// 2m fallback polling across large local agent trees while still bounding
// runaway walks.
const recursiveWatchBudget = 65536

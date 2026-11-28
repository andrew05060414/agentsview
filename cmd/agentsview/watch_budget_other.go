//go:build !linux && !windows

package main

// BSD/kqueue uses one file descriptor per watched directory; macOS FSEvents ignores this budget.
const recursiveWatchBudget = 8192

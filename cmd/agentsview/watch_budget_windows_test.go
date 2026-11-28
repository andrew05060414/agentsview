//go:build windows

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecursiveWatchBudgetWindowsCoversLargeTrees(t *testing.T) {
	assert.GreaterOrEqual(t, recursiveWatchBudget, 65536,
		"Windows watch budget must avoid prematurely degrading large local agent trees to 2m polling")
}

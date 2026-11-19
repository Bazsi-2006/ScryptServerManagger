//go:build unix

package main

import (
	"os"
	"syscall"
)

// ownerUID returns the numeric owner of path.
func ownerUID(path string) (uint32, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return sys.Uid, true
}

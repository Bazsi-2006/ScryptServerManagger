//go:build !unix

package main

func ownerUID(string) (uint32, bool) { return 0, false }

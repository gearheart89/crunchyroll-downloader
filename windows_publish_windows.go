package main

import "syscall"

// MoveFile (unlike MoveFileEx with REPLACE_EXISTING) never overwrites a target.
func publishWindowDirectory(source, destination string) error {
	a, e := syscall.UTF16PtrFromString(source)
	if e != nil {
		return e
	}
	b, e := syscall.UTF16PtrFromString(destination)
	if e != nil {
		return e
	}
	return syscall.MoveFile(a, b)
}

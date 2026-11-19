package main

import (
	"runtime"
	"syscall"
	"unsafe"
)

func publishWindowDirectory(source, destination string) error {
	// Linux renameat2(RENAME_NOREPLACE) is atomic even if another process
	// creates an empty destination directory between validation and publication.
	var trap uintptr
	switch runtime.GOARCH {
	case "amd64":
		trap = 316
	case "arm64":
		trap = 276
	default:
		return wf("unsupported_capability")
	}
	a, e := syscall.BytePtrFromString(source)
	if e != nil {
		return e
	}
	b, e := syscall.BytePtrFromString(destination)
	if e != nil {
		return e
	}
	const atCWD = -100
	dirfd := int64(atCWD)
	_, _, errno := syscall.Syscall6(trap, uintptr(dirfd), uintptr(unsafe.Pointer(a)), uintptr(dirfd), uintptr(unsafe.Pointer(b)), 1, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

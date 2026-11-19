//go:build !windows && !linux

package main

func publishWindowDirectory(source, destination string) error { return wf("unsupported_capability") }

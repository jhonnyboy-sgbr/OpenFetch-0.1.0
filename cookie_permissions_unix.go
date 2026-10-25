//go:build !windows

package main

import "os"

func restrictPrivateDirectory(path string) error {
	return os.Chmod(path, 0700)
}

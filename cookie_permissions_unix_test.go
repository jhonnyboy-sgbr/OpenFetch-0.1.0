//go:build !windows

package main

import "os"

func cookieCopyPermissionsPrivate(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().Perm()&0077 == 0
}

//go:build unix

package main

import "golang.org/x/sys/unix"

// writableDir reports whether the process's real user/group may create entries
// in the directory at path. access(2) is asked for both the write and search bits:
// a directory that is writable but not searchable cannot receive new files.
// The permission is advisory and queried without writing.
func writableDir(path string) *bool {
	writable := unix.Access(path, unix.W_OK|unix.X_OK) == nil
	return &writable
}

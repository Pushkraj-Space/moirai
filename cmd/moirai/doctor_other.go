//go:build !unix

package main

// writableDir returns nil because write permission is not checked on this
// platform; doctor reports it as not checked.
func writableDir(string) *bool { return nil }

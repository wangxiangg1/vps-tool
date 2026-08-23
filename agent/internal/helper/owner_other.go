//go:build !linux

package helper

import "os"

func validateOwner(string, os.FileInfo) error { return nil }

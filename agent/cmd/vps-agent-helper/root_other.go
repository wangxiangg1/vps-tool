//go:build !linux

package main

import (
	"errors"
	"os"
)

func requireRoot() error { return errors.New("helper is only supported on Linux") }

func trustedOwner(os.FileInfo) bool { return false }

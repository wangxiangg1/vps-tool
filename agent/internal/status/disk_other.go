//go:build !linux

package status

import "fmt"

func rootDiskUsage() (uint64, uint64, error) {
	return 0, 0, fmt.Errorf("root disk usage is only implemented on Linux")
}

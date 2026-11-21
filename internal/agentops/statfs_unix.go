//go:build unix

package agentops

import "syscall"

// freeBytes returns the space available to unprivileged writers on the file
// system holding path.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:unconvert // field types differ per OS
}

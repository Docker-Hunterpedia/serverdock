//go:build !unix

package agentops

import "errors"

func freeBytes(string) (uint64, error) { return 0, errors.New("free space is not readable on this OS") }

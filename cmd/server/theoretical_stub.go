//go:build !linux

package main

import "errors"

// runEpollServer is a stub for non-Linux platforms.
func runEpollServer(serverType, port string) error {
	return errors.New("epoll servers are only supported on Linux")
}

// runIOUringServer is a stub for non-Linux platforms.
func runIOUringServer(serverType, port string) error {
	return errors.New("io_uring servers are only supported on Linux")
}

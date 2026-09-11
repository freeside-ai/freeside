package main

import (
	"errors"

	"golang.org/x/sys/unix"
)

func socketPeerUID(fd int) (uint32, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	// XUCRED_VERSION is 0 in the Darwin local-socket credential ABI.
	if cred.Version != 0 {
		return 0, errors.New("unrecognized local peer credential version")
	}
	return cred.Uid, nil
}

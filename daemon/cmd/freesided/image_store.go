package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os/exec"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
)

// reseedWalkthrough names the walkthrough section that reseeds a pinned
// image whose seeding registry is gone.
const reseedWalkthrough = `reseed it as "Reseed A Pinned Image That Stopped Resolving" in docs/production-walkthrough.md describes`

// checkImageInStore reports whether the local container store holds ref
// under its digest. `container image inspect` reads only the store: it needs
// no rig lease and contacts no registry, so a digest whose seeding registry
// is gone fails here while its tag is still cached, before a container
// create fails on it.
func checkImageInStore(ctx context.Context, containerBin, ref string) error {
	_, err := projectimage.InspectImageDigest(ctx, containerBin, ref)
	return err
}

// imageStoreFailure is the evidence and fix for a failed store check of ref.
// A missing container CLI is installed: LookPath reports ErrNotFound for a
// name and ErrNotExist for a path. Apple container 1.1.0 reports a reference
// the store has no entry for as "image not found", which a reseed fixes and a
// rebuild would not, since the pinned digest stays the same. Any other
// failure, such as a stopped container system, names the command that shows
// its cause.
func imageStoreFailure(ref string, err error) (evidence, fix string) {
	switch {
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist):
		return "the container CLI that inspects " + ref + " was not found", "install the Apple container CLI"
	case strings.Contains(err.Error(), "image not found"):
		return ref + " is not in the local container store", reseedImageFix(ref)
	default:
		return ref + " could not be inspected", "run container image inspect " + ref +
			" to see why; start the container system with container system start if it is stopped, " +
			"and if the image is not found, " + reseedImageFix(ref)
	}
}

// reseedImageFix is the reseed for ref. Only a loopback registry reference
// names its port's pull, since a local seeding registry is what goes away.
func reseedImageFix(ref string) string {
	host, _, _ := strings.Cut(ref, "/")
	addr, err := netip.ParseAddrPort(host)
	if err != nil || !addr.Addr().IsLoopback() {
		return reseedWalkthrough
	}
	return fmt.Sprintf("if a registry runs on %s, run container image pull --scheme http %s; otherwise %s",
		host, ref, reseedWalkthrough)
}

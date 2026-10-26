package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"testing"
)

func TestImageStoreFailureNamesTheFix(t *testing.T) {
	t.Parallel()
	digest := "@sha256:" + strings.Repeat("c", 64)
	missing := errors.New("inspect image: exit status 1\nError: image not found")
	tests := map[string]struct {
		ref      string
		err      error
		evidence string
		fix      string
	}{
		"loopback registry": {
			"127.0.0.1:5523/project-agent" + digest, missing,
			"127.0.0.1:5523/project-agent" + digest + " is not in the local container store",
			"if a registry runs on 127.0.0.1:5523, run container image pull --scheme http 127.0.0.1:5523/project-agent" +
				digest + "; otherwise " + reseedWalkthrough,
		},
		"remote registry": {
			"registry.local/agent" + digest, missing,
			"registry.local/agent" + digest + " is not in the local container store", reseedWalkthrough,
		},
		"named loopback host": {
			"localhost:5005/exporter" + digest, missing,
			"localhost:5005/exporter" + digest + " is not in the local container store", reseedWalkthrough,
		},
		"stopped container system": {
			"127.0.0.1:5005/exporter" + digest, errors.New("inspect image: exit status 1\nError: XPC connection error"),
			"127.0.0.1:5005/exporter" + digest + " could not be inspected",
			"run container image inspect 127.0.0.1:5005/exporter" + digest + " to see why; start the container " +
				"system with container system start if it is stopped, and if the image is not found, if a registry " +
				"runs on 127.0.0.1:5005, run container image pull --scheme http 127.0.0.1:5005/exporter" + digest +
				"; otherwise " + reseedWalkthrough,
		},
		"no container CLI at a path": {
			"registry.local/agent" + digest, &exec.Error{Name: "/opt/missing/container", Err: fs.ErrNotExist},
			"the container CLI that inspects registry.local/agent" + digest + " was not found",
			"install the Apple container CLI",
		},
		"no container CLI": {
			"127.0.0.1:5005/exporter" + digest, fmt.Errorf("resolve container executable: %w", exec.ErrNotFound),
			"the container CLI that inspects 127.0.0.1:5005/exporter" + digest + " was not found",
			"install the Apple container CLI",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			evidence, fix := imageStoreFailure(tt.ref, tt.err)
			if evidence != tt.evidence || fix != tt.fix {
				t.Fatalf("got (%q, %q), want (%q, %q)", evidence, fix, tt.evidence, tt.fix)
			}
		})
	}
}

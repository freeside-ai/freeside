package ward

import "testing"

func TestMountTypeValid(t *testing.T) {
	for _, m := range AllMountTypes {
		if !m.valid() {
			t.Errorf("MountType %q: valid() = false, want true", m)
		}
	}
	// MountBind is a reject-only decode target after #591, so it must report
	// invalid alongside the unknown kinds.
	for _, m := range []MountType{"", "tmpfs", "virtiofs", MountBind} {
		if m.valid() {
			t.Errorf("MountType %q: valid() = true, want false", m)
		}
	}
}

func TestContainerStateValid(t *testing.T) {
	for _, s := range AllContainerStates {
		if !s.valid() {
			t.Errorf("ContainerState %q: valid() = false, want true", s)
		}
	}
	for _, s := range []ContainerState{"", "stopping", "STOPPED"} {
		if s.valid() {
			t.Errorf("ContainerState %q: valid() = true, want false", s)
		}
	}
}

func TestNetworkModeValid(t *testing.T) {
	for _, m := range AllNetworkModes {
		if !m.valid() {
			t.Errorf("NetworkMode %q: valid() = false, want true", m)
		}
	}
	for _, m := range []NetworkMode{"", "hostOnly", "bridge"} {
		if m.valid() {
			t.Errorf("NetworkMode %q: valid() = true, want false", m)
		}
	}
}

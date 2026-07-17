package ward

import "testing"

func TestMountTypeValid(t *testing.T) {
	for _, m := range AllMountTypes {
		if !m.valid() {
			t.Errorf("MountType %q: valid() = false, want true", m)
		}
	}
	for _, m := range []MountType{"", "tmpfs", "virtiofs"} {
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

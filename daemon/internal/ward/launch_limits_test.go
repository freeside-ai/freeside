package ward

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func TestHandoffCreatesEveryContainerAtTheDeclaredSize(t *testing.T) {
	fx := newHandoffFixture(t)
	hs := testHandoffSpec()
	hs.Size = ContainerSize{CPUs: 6, MemoryMiB: 3072}
	var created []ContainerSpec
	fx.rt.onCreateContainer = func(spec ContainerSpec) error {
		created = append(created, spec)
		return nil
	}
	if _, err := fx.runSpec(t, hs); err != nil {
		t.Fatal(err)
	}
	if len(created) == 0 {
		t.Fatal("no container was created")
	}
	for _, spec := range created {
		if spec.Size != hs.Size {
			t.Errorf("%s created at %s, want %s", spec.Name, spec.Size, hs.Size)
		}
	}
}

func TestHandoffRefusesAnUnsizedSpec(t *testing.T) {
	fx := newHandoffFixture(t)
	hs := testHandoffSpec()
	hs.Size = ContainerSize{}
	if _, err := fx.runSpec(t, hs); !errors.Is(err, ErrInvalidHandoffSpec) {
		t.Fatalf("Handoff = %v, want ErrInvalidHandoffSpec", err)
	}
	if len(fx.rt.calls) != 0 {
		t.Fatalf("an unsized handoff reached the runtime: %q", fx.rt.calls)
	}
}

// TestUnsizedSpecDigestsInThePreSizeFormat pins why pre-size journal records
// still recover: with Class and Size zero, the digested bytes carry neither.
func TestUnsizedSpecDigestsInThePreSizeFormat(t *testing.T) {
	t.Parallel()
	hs := testHandoffSpec()
	hs.Class, hs.Size = "", ContainerSize{}
	body, err := json.Marshal(hs)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`"Class"`)) || bytes.Contains(body, []byte(`"Size"`)) {
		t.Fatalf("unsized spec digests with size fields: %s", body)
	}
}

func TestRecoverAdoptsARecordJournaledBeforeSizes(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	presize := hs
	presize.Class, presize.Size = "", ContainerSize{}
	fx.openRecord(t, presize)

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want the pre-size record recovered", err)
	}
	if res.Outcome != RecoveryLoss {
		t.Fatalf("Outcome = %q, want loss", res.Outcome)
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}

func TestRecoverRefusesARecordJournaledAtAnotherSize(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	other := hs
	other.Size = ContainerSize{CPUs: 8, MemoryMiB: 8192}
	fx.openRecord(t, other)

	if _, err := fx.recover(t, hs.RunID, hs); !errors.Is(err, ErrInvalidJournalRecord) {
		t.Fatalf("Recover = %v, want ErrInvalidJournalRecord", err)
	}
	fx.wantOpen(t, hs.RunID)
}

func TestProjectImageRoomDeclaresItsSize(t *testing.T) {
	size := ContainerSize{CPUs: 6, MemoryMiB: 4096}
	runtime := newFakeRuntime(t)
	var calls [][]string
	room := newProjectImageRoom(
		"container", verificationProjectImage(t, []string{"prepare"}), runtime,
		verificationRunner(t, runtime, []verify.StepResult{{}, {}}, &calls),
		nil, verify.DefaultMaxRoomOutputBytes, size,
	)
	if _, err := room.Run(t.Context(), t.TempDir(), []string{"verify"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) == 0 {
		t.Fatal("the room ran no container")
	}
	for _, call := range calls {
		if !slices.Contains(call, "--cpus") ||
			!strings.Contains(strings.Join(call, " "), strings.Join(sizeArgs(size), " ")) {
			t.Fatalf("room container %q does not declare %s", call, size)
		}
	}
}

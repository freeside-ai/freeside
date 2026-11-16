package ward

import (
	"context"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// launchReservationKey is the context key under which a container start
// carries the memory reservation it runs under.
type launchReservationKey struct{}

// WithLaunchReservation returns ctx carrying the reservation the containers
// started under it run within (plan §5.4). The code that reserves puts it
// there; a launch guard reads it before a verb that boots a VM.
func WithLaunchReservation(ctx context.Context, id domain.InvocationID) context.Context {
	return context.WithValue(ctx, launchReservationKey{}, id)
}

// LaunchReservationFrom returns the reservation ctx carries, and false when
// it carries none.
func LaunchReservationFrom(ctx context.Context) (domain.InvocationID, bool) {
	id, ok := ctx.Value(launchReservationKey{}).(domain.InvocationID)
	return id, ok && id != ""
}

// ReservationLabel is the label key a container started under a daemon
// reservation carries, valued with the reservation's id. Work that leaves no
// other record of its containers, an image build's probes or the room that
// reads its recipe, is proved gone after a restart by finding no container
// with its reservation's label.
const ReservationLabel = "ai.freeside.reservation"

// ReservationLabelArgs returns the label arguments for a container started
// under ctx's reservation, and none when ctx carries none.
func ReservationLabelArgs(ctx context.Context) []string {
	id, ok := LaunchReservationFrom(ctx)
	if !ok {
		return nil
	}
	return []string{"--label", ReservationLabel + "=" + string(id)}
}

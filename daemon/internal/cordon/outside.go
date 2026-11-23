package cordon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

// outsideHost is what Full does outside the launch on the account's own
// launchd and Simulator: its stand-in job, and the effects it looks for
// after the probe. Every job it starts, reads, or stops is under
// probeLabelPrefix.
type outsideHost interface {
	// guiDomain is the account's launchd GUI domain, gui/<uid>.
	guiDomain() string
	// startJob bootstraps the job plist names into the GUI domain.
	startJob(ctx context.Context, plist string) error
	// job reads the job at target (domain/label): its pid, and whether it
	// is loaded.
	job(ctx context.Context, target string) (pid int, loaded bool, err error)
	// kickstartJob kills and restarts the job at target (kickstart -k).
	kickstartJob(ctx context.Context, target string) error
	// stopJob boots the job at target out.
	stopJob(ctx context.Context, target string) error
	// simDeviceType is a device type simctl can create, or "" when simctl
	// can't run on this host.
	simDeviceType(ctx context.Context) (string, error)
	// simDevices counts the devices named name.
	simDevices(ctx context.Context, name string) (int, error)
	// simDelete deletes the devices named name, a name only the probe
	// uses; none is not an error.
	simDelete(ctx context.Context, name string) error
}

const (
	launchctlPath = "/bin/launchctl"
	xcrunPath     = "/usr/bin/xcrun"
)

// systemOutside runs launchctl and simctl as the daemon.
type systemOutside struct{}

func (systemOutside) guiDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func (o systemOutside) startJob(ctx context.Context, plist string) error {
	out, err := runOutside(ctx, true, launchctlPath, "bootstrap", o.guiDomain(), plist)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

var launchctlPid = regexp.MustCompile(`(?m)^\s*pid = (\d+)$`)

// job reads launchctl print, which fails for a label nothing loaded.
func (systemOutside) job(ctx context.Context, target string) (int, bool, error) {
	if err := ownTarget(target); err != nil {
		return 0, false, err
	}
	out, err := runOutside(ctx, false, launchctlPath, "print", target)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("launchctl print: %w", err)
	}
	m := launchctlPid.FindSubmatch(out)
	if m == nil {
		return 0, true, nil
	}
	pid, err := strconv.Atoi(string(m[1]))
	return pid, true, err
}

func (systemOutside) kickstartJob(ctx context.Context, target string) error {
	if err := ownTarget(target); err != nil {
		return err
	}
	out, err := runOutside(ctx, true, launchctlPath, "kickstart", "-k", target)
	if err != nil {
		return fmt.Errorf("launchctl kickstart: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (systemOutside) stopJob(ctx context.Context, target string) error {
	if err := ownTarget(target); err != nil {
		return err
	}
	out, err := runOutside(ctx, true, launchctlPath, "bootout", target)
	if err != nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ownTarget refuses a launchd target outside the suite's own label space,
// so nothing the suite runs can reach another job.
func ownTarget(target string) error {
	_, label, ok := strings.Cut(strings.TrimPrefix(target, "gui/"), "/")
	if !strings.HasPrefix(target, "gui/") || !ok || !strings.HasPrefix(label, probeLabelPrefix) {
		return fmt.Errorf("cordon: launchd target %q is not the suite's own", target)
	}
	return nil
}

func (systemOutside) simDeviceType(ctx context.Context) (string, error) {
	out, ok, err := simctlList(ctx, "devicetypes")
	if err != nil || !ok {
		return "", err
	}
	var list struct {
		DeviceTypes []struct {
			Identifier string `json:"identifier"`
		} `json:"devicetypes"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", fmt.Errorf("simctl list devicetypes: %w", err)
	}
	if len(list.DeviceTypes) == 0 {
		return "", nil
	}
	return list.DeviceTypes[0].Identifier, nil
}

func (systemOutside) simDevices(ctx context.Context, name string) (int, error) {
	out, ok, err := simctlList(ctx, "devices")
	if err != nil || !ok {
		return 0, err
	}
	var list struct {
		Devices map[string][]struct {
			Name string `json:"name"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return 0, fmt.Errorf("simctl list devices: %w", err)
	}
	n := 0
	for _, devices := range list.Devices {
		for _, d := range devices {
			if d.Name == name {
				n++
			}
		}
	}
	return n, nil
}

func (o systemOutside) simDelete(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, probeLabelPrefix) {
		return fmt.Errorf("cordon: device %q is not the probe's own", name)
	}
	n, err := o.simDevices(ctx, name)
	if err != nil || n == 0 {
		return err
	}
	out, err := runOutside(ctx, true, xcrunPath, "simctl", "delete", name)
	if err != nil {
		return fmt.Errorf("simctl delete: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// simctlList runs simctl list in JSON. ok is false when simctl can't run
// here: no xcrun, or no Xcode for it to find.
func simctlList(ctx context.Context, what string) ([]byte, bool, error) {
	if _, err := os.Stat(xcrunPath); errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	out, err := runOutside(ctx, false, xcrunPath, "simctl", "list", "-j", what)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("simctl list %s: %w", what, err)
	}
	return out, true, nil
}

// runOutside runs a launchctl or simctl command under procbound, capturing
// stdout, and stderr too when combined. Every caller aims it at the
// suite's own stand-in job, plist, or device, or at a fixed listing.
func runOutside(ctx context.Context, combined bool, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: callers name only the suite's own targets
	var out bytes.Buffer
	cmd.Stdout = &out
	if combined {
		cmd.Stderr = &out
	}
	err := procbound.Run(cmd, 0)
	return out.Bytes(), err
}

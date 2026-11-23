package cordon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrRunEndUnproven means the backend could not show that no process from a
// launch is alive. A launch that ends this way is blocked: something it
// started may still be running.
var ErrRunEndUnproven = errors.New("cordon: no live process from the launch is unproven")

// proofTarget names what the run-end proof asks about for one launch.
type proofTarget struct {
	// tag is the path only this launch's rules deny the metadata of.
	tag string
	// control sits beside tag and no rule names it. A process the query
	// says is denied it is not a member: the query is answering "denied" for
	// a reason other than the tag rule (a missing path, a zombie).
	control string
	// canary is a process the backend started under this launch's rules
	// before the agent, so it is a member by construction. A proof that
	// can't find it is not finding members, and fails.
	canary int
}

// hostProcess is one process as a listing saw it. Its start time tells it
// apart from a later process that reuses its pid.
type hostProcess struct {
	pid   int
	start int64
	// settled is false for a process fork was still creating or that was
	// exiting, whose sandbox the query can't read reliably.
	settled bool
}

// hostProcesses is what the proof asks the kernel. The darwin cgo build
// implements it with sysctl and sandbox_check; every other build has none.
type hostProcesses interface {
	// live lists every process on the host that isn't a zombie.
	live() ([]hostProcess, error)
	// member reports whether pid runs under the rules of target's launch.
	// A pid that no longer exists is not a member.
	member(pid int, target proofTarget) (bool, error)
	// same reports whether the process p names is still alive and settled:
	// its pid exists with the start time the listing saw, and it is neither
	// being created, exiting, nor a zombie.
	same(p hostProcess) (bool, error)
	// kill sends SIGKILL to pid. A pid that no longer exists is not an
	// error.
	kill(pid int) error
}

// proofPollInterval paces the proof's passes while a killed member is
// still on its way to being a zombie.
const proofPollInterval = 10 * time.Millisecond

// endLaunch ends every live process under target's rules and returns nil
// only once a full pass over the host's processes finds none. It finds a
// process by its sandbox, not its process group or session, so a child that
// called setsid or double-forked is found too: Seatbelt passes a process's
// rules to its children and a process can't shed them.
//
// Any query error, a canary that doesn't read as a member, a tag or control
// file that is gone, or timeout passing first returns an error wrapping
// ErrRunEndUnproven.
//
// A pass proves nothing about a listed process that is gone by the time it
// is queried: a gone pid reads as no member, and it may have forked a child
// the listing missed, so a chain of processes that each fork and exit would
// pass. A process fork is still creating or that is exiting reads as
// unsandboxed for the same reason. Only a pass that finds no member and
// finds every listed process settled, and still the same live process
// after its query, ends the proof.
//
// A pid could still be reused between that check and the kill, which the
// check narrows to microseconds without closing
// (devlog/2026-11-22-1236-cordoned-host-launch.md).
func endLaunch(ctx context.Context, host hostProcesses, target proofTarget, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := targetFilesPresent(target); err != nil {
		return err
	}
	// The canary is still alive here, since only this proof kills it, so a
	// query that answers "not a member" for everything fails now.
	isMember, err := host.member(target.canary, target)
	if err != nil {
		return fmt.Errorf("%w: query the canary %d: %w", ErrRunEndUnproven, target.canary, err)
	}
	if !isMember {
		return fmt.Errorf("%w: the canary %d does not read as a member", ErrRunEndUnproven, target.canary)
	}
	self := os.Getpid()
	for {
		found, unresolved, err := killMembers(host, target, self)
		if err != nil {
			return err
		}
		// A missing tag makes every sandboxed process read as denied, and
		// the control alone then keeps non-members out; refuse rather than
		// rely on one check.
		if err := targetFilesPresent(target); err != nil {
			return err
		}
		if found == 0 && unresolved == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: the last pass found %d member(s) and %d process(es) gone before their query: %w",
				ErrRunEndUnproven, found, unresolved, ctx.Err())
		case <-time.After(proofPollInterval):
		}
	}
}

// killMembers runs one pass: it lists the host's live processes and kills
// each member. It returns how many members it found and how many listed
// processes were gone, or replaced, by the time it asked about them.
func killMembers(host hostProcesses, target proofTarget, self int) (found, unresolved int, _ error) {
	procs, err := host.live()
	if err != nil {
		return 0, 0, fmt.Errorf("%w: list processes: %w", ErrRunEndUnproven, err)
	}
	for _, p := range procs {
		if p.pid <= 1 || p.pid == self {
			continue
		}
		if !p.settled {
			unresolved++
			continue
		}
		isMember, err := host.member(p.pid, target)
		if err != nil {
			return 0, 0, fmt.Errorf("%w: query %d: %w", ErrRunEndUnproven, p.pid, err)
		}
		// Checked after the query, so a yes means the process the listing
		// saw settled was still alive and settled when it was asked about:
		// a process can't return from exiting.
		same, err := host.same(p)
		if err != nil {
			return 0, 0, fmt.Errorf("%w: recheck %d: %w", ErrRunEndUnproven, p.pid, err)
		}
		if !same {
			unresolved++
			continue
		}
		if !isMember {
			continue
		}
		found++
		if err := host.kill(p.pid); err != nil {
			return 0, 0, fmt.Errorf("%w: kill %d: %w", ErrRunEndUnproven, p.pid, err)
		}
	}
	return found, unresolved, nil
}

// targetFilesPresent checks that the tag and control files are both there,
// since sandbox_check reads a missing path as denied.
func targetFilesPresent(target proofTarget) error {
	for _, path := range []string{target.tag, target.control} {
		if _, err := os.Lstat(path); err != nil {
			return fmt.Errorf("%w: proof file: %w", ErrRunEndUnproven, err)
		}
	}
	return nil
}

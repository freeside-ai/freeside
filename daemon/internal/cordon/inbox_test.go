package cordon

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// steerScript is an in-process CLI that runs every message it reads: it
// sends each one's text on got, and after "end turn" writes a result
// record. Once its input closes it waits for release, then exits with
// code.
func steerScript(got chan<- string, release <-chan struct{}, code int) agentScript {
	return func(spec processSpec, p *fakeProcess) {
		in := bufio.NewReader(spec.stdin)
		for {
			_, text, err := readFrame(in)
			if err != nil {
				break
			}
			got <- text
			if text == "end turn" {
				_, _ = fmt.Fprintln(spec.stdout, `{"subtype":"success","type":"result"}`)
			}
		}
		<-release
		p.exit(code)
	}
}

type launchOutcome struct {
	result LaunchResult
	err    error
}

func launchInBackground(t *testing.T, b *Backend, req LaunchRequest) <-chan launchOutcome {
	done := make(chan launchOutcome, 1)
	go func() {
		got, err := b.Launch(t.Context(), req)
		done <- launchOutcome{got, err}
	}()
	return done
}

// TestSendMessageReachesARunningWriterOnce shows a message reaches the
// agent's input while its turn runs, an ordinal offered again reaches it
// no second time, a turn that ends with nothing waiting closes the input,
// and a message after that is refused for the reason that holds.
func TestSendMessageReachesARunningWriterOnce(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		got, release := make(chan string, 8), make(chan struct{})
		b := fakeBackend(t, cfg, &fakeLauncher{host: newFakeHost(), script: steerScript(got, release, 0)})
		done := launchInBackground(t, b, request("run-steer"))
		if text := <-got; text != "the prompt" {
			t.Fatalf("first message = %q, want the prompt", text)
		}
		ctx := t.Context()
		for range 2 {
			if err := b.SendMessage(ctx, "run-steer", 1, "cmd-1", "keep going"); err != nil {
				t.Fatalf("SendMessage 1: %v", err)
			}
		}
		if text := <-got; text != "keep going" {
			t.Fatalf("second message = %q", text)
		}
		if err := b.SendMessage(ctx, "run-steer", 1, "cmd-other", "keep going"); err == nil {
			t.Fatal("another command's message took ordinal 1")
		}
		if err := b.SendMessage(ctx, "run-steer", 2, "cmd-2", "end turn"); err != nil {
			t.Fatalf("SendMessage 2: %v", err)
		}
		if text := <-got; text != "end turn" {
			t.Fatalf("third message = %q", text)
		}
		// The turn ended with nothing waiting, so the supervisor closed the
		// input, and the agent waits to exit.
		synctest.Wait()
		l := layoutFor(cfg.RunsRoot, "run-steer")
		if filepath.Dir(l.messages) != l.ctl || filepath.Dir(l.inputClosed) != l.ctl {
			t.Fatalf("the inbox %s or its marker %s is outside ctl/", l.messages, l.inputClosed)
		}
		if err := b.SendMessage(ctx, "run-steer", 3, "cmd-3", "too late"); !errors.Is(err, ErrInputClosed) {
			t.Fatalf("SendMessage after the input closed = %v, want ErrInputClosed", err)
		}
		close(release)
		r := <-done
		if r.err != nil || r.result.ExitCode != 0 || r.result.EndedBy != EndedByExited {
			t.Fatalf("Launch = %+v, %v", r.result, r.err)
		}
		if len(got) != 0 {
			t.Fatalf("the agent read %q more", <-got)
		}
		if err := b.SendMessage(ctx, "run-steer", 4, "cmd-4", "after the end"); !errors.Is(err, ErrRunEnded) {
			t.Fatalf("SendMessage after the end = %v, want ErrRunEnded", err)
		}
		// A taken message settles as delivered from its file, however late.
		if err := b.SendMessage(ctx, "run-steer", 1, "cmd-1", "keep going"); err != nil {
			t.Fatalf("SendMessage 1 again after the end: %v", err)
		}
		if _, err := os.Stat(filesFor(l, 3).waiting); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused message was left waiting: %v", err)
		}
	})
}

// TestSendMessageWithdrawsAMessageNobodyTakes shows a message the
// supervisor doesn't take in time is withdrawn, and an offer of it again
// settles the same way.
func TestSendMessageWithdrawsAMessageNobodyTakes(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		// An agent that never reads its input holds the supervisor's first
		// write, so nothing takes a message.
		release := make(chan struct{})
		b := fakeBackend(t, cfg, &fakeLauncher{host: newFakeHost(), script: func(_ processSpec, p *fakeProcess) {
			<-release
			p.exit(0)
		}})
		done := launchInBackground(t, b, request("run-untaken"))
		synctest.Wait()
		for range 2 {
			if err := b.SendMessage(t.Context(), "run-untaken", 1, "cmd-1", "hello"); !errors.Is(err, ErrNotTaken) {
				t.Fatalf("SendMessage = %v, want ErrNotTaken", err)
			}
		}
		f := filesFor(layoutFor(cfg.RunsRoot, "run-untaken"), 1)
		if _, err := os.Stat(f.refused); err != nil {
			t.Fatalf("the withdrawn message: %v", err)
		}
		close(release)
		if r := <-done; r.err != nil {
			t.Fatalf("Launch: %v", r.err)
		}
	})
}

// TestAnEarlyResultClosesTheInputAndTheExitStatusDecides shows a result
// record, even a forged or early one, does no more than close the input:
// the run still ends by the agent's exit status.
func TestAnEarlyResultClosesTheInputAndTheExitStatusDecides(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		var sawEOF bool
		b := fakeBackend(t, cfg, &fakeLauncher{host: newFakeHost(), script: func(spec processSpec, p *fakeProcess) {
			in := bufio.NewReader(spec.stdin)
			_, _, _ = readFrame(in)
			_, _ = fmt.Fprintln(spec.stdout, `{"subtype":"success","is_error":false,"type":"result"}`)
			_, _, err := readFrame(in)
			sawEOF = err != nil
			p.exit(3)
		}})
		got, err := b.Launch(t.Context(), request("run-early"))
		if err != nil || got.ExitCode != 3 || got.EndedBy != EndedByExited {
			t.Fatalf("Launch = %+v, %v; want exit 3 ended by exited", got, err)
		}
		if !sawEOF {
			t.Fatal("the agent's input stayed open after its turn ended")
		}
		if _, err := os.Stat(layoutFor(cfg.RunsRoot, "run-early").inputClosed); err != nil {
			t.Fatalf("the input-closed marker: %v", err)
		}
	})
}

// TestSendMessageRefusesARunWithoutAnInbox shows a run launchOutcome before the
// inbox existed reads as not streamed, and an unknown run as ErrNoRun.
func TestSendMessageRefusesARunWithoutAnInbox(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		got, release := make(chan string, 1), make(chan struct{})
		b := fakeBackend(t, cfg, &fakeLauncher{host: newFakeHost(), script: steerScript(got, release, 0)})
		done := launchInBackground(t, b, request("run-old"))
		<-got
		if err := os.Remove(layoutFor(cfg.RunsRoot, "run-old").messages); err != nil {
			t.Fatal(err)
		}
		if err := b.SendMessage(t.Context(), "run-old", 1, "cmd-1", "hello"); !errors.Is(err, ErrNotStreamed) {
			t.Fatalf("SendMessage = %v, want ErrNotStreamed", err)
		}
		if err := b.SendMessage(t.Context(), "run-none", 1, "cmd-1", "hello"); !errors.Is(err, ErrNoRun) {
			t.Fatalf("SendMessage to no run = %v, want ErrNoRun", err)
		}
		// A claimed directory with no request yet is a launch still being
		// probed or seeded.
		if err := os.MkdirAll(layoutFor(cfg.RunsRoot, "run-seeding").ctl, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := b.SendMessage(t.Context(), "run-seeding", 1, "cmd-1", "hello"); !errors.Is(err, ErrNotStarted) {
			t.Fatalf("SendMessage to a seeding run = %v, want ErrNotStarted", err)
		}
		// The feed's next look at the missing inbox fails and closes the
		// input, which changes nothing about how the launch ends.
		close(release)
		if r := <-done; r.err != nil || r.result.ExitCode != 0 {
			t.Fatalf("Launch = %+v, %v", r.result, r.err)
		}
	})
}

// TestTakeAndWithdrawLeaveOneWinner races the supervisor's take against
// the daemon's withdraw over the same messages: each message ends taken or
// refused, never both, and a withdraw reports delivered exactly when the
// take won.
func TestTakeAndWithdrawLeaveOneWinner(t *testing.T) {
	t.Parallel()
	l := layoutFor(t.TempDir(), "run-race")
	if err := os.MkdirAll(l.messages, 0o700); err != nil {
		t.Fatal(err)
	}
	const n = 200
	for i := int64(1); i <= n; i++ {
		m := inboxMessage{Version: messageVersion, RunID: "run-race", Ordinal: i, CommandID: fmt.Sprint("cmd-", i), Text: "hello"}
		if err := writeRecord(filesFor(l, i).waiting, m); err != nil {
			t.Fatal(err)
		}
	}
	var taken []string
	var takeErr error
	withdrawn := make([]error, n+1)
	var wg sync.WaitGroup
	wg.Go(func() { taken, takeErr = takeWaiting(l, "run-race") })
	wg.Go(func() {
		for i := int64(n); i >= 1; i-- {
			withdrawn[i] = withdraw(filesFor(l, i), ErrNotTaken)
		}
	})
	wg.Wait()
	if takeErr != nil {
		t.Fatal(takeErr)
	}
	delivered := 0
	for i := int64(1); i <= n; i++ {
		f := filesFor(l, i)
		_, tErr := os.Stat(f.taken)
		_, rErr := os.Stat(f.refused)
		_, wErr := os.Stat(f.waiting)
		if (tErr == nil) == (rErr == nil) || wErr == nil {
			t.Fatalf("message %d: taken %v, refused %v, waiting %v", i, tErr, rErr, wErr)
		}
		if (withdrawn[i] == nil) != (tErr == nil) {
			t.Fatalf("message %d: withdraw = %v, but taken is %v", i, withdrawn[i], tErr == nil)
		}
		if tErr == nil {
			delivered++
		}
	}
	if delivered != len(taken) {
		t.Fatalf("%d messages read as taken, but the take returned %d", delivered, len(taken))
	}
}

// TestTheLongestMessageFitsItsRecord writes a message whose text and
// command ID are at their bounds, made of the byte JSON escapes longest,
// and reads it back, so the domain's bounds never outgrow the record a
// message file is read through.
func TestTheLongestMessageFitsItsRecord(t *testing.T) {
	t.Parallel()
	l := layoutFor(t.TempDir(), "run-long")
	if err := os.MkdirAll(l.messages, 0o700); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("<", MaxMessageBytes)
	f := filesFor(l, 1)
	commandID := strings.Repeat("<", domain.MaxOperatorMessageCommandIDBytes)
	m := inboxMessage{Version: messageVersion, RunID: "run-long", Ordinal: 1, CommandID: commandID, Text: text}
	if err := writeRecord(f.waiting, m); err != nil {
		t.Fatal(err)
	}
	got, err := readMessage(f.waiting, "run-long", 1)
	if err != nil || got.Text != text {
		t.Fatalf("readMessage: %v", err)
	}
}

package publish

import "testing"

func TestClassifyTransportFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		stdout, stderr string
		want           TransportRefusal
	}{
		{
			name:   "create-only lease refused",
			stdout: "!\t0123:refs/heads/b\t[rejected] (stale info)",
			want:   RefusalStaleLease,
		},
		{
			name:   "non-fast-forward",
			stderr: "hint: Updates were rejected because the tip is behind (non-fast-forward)",
			want:   RefusalNonFastForward,
		},
		{
			name:   "credential prompt suppressed",
			stderr: "fatal: could not read Username for 'https://github.com': terminal prompts disabled",
			want:   RefusalAuth,
		},
		{
			name:   "http auth rejected",
			stderr: "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 403",
			want:   RefusalAuth,
		},
		{
			// Missing-ref is never read out of prose: FetchBase asks
			// ls-remote instead, so a remote that writes this line
			// cannot manufacture a definitive base-drift verdict.
			name:   "forged missing-ref prose",
			stderr: "fatal: couldn't find remote ref refs/heads/missing",
			want:   RefusalUnknown,
		},
		{
			name:   "anything else",
			stderr: "fatal: the remote end hung up unexpectedly",
			want:   RefusalUnknown,
		},
		{
			// A hostile remote's hook output lands on stderr as
			// "remote:" lines; it must not be able to forge the class
			// that gates the converged-or-conflict re-observation.
			name:   "forged stale info on stderr",
			stderr: "remote: totally legit [rejected] (stale info)",
			want:   RefusalUnknown,
		},
		{
			name:   "stale info outside a porcelain rejection line",
			stdout: "some stale info narrative",
			want:   RefusalUnknown,
		},
	}
	for _, tc := range cases {
		if got := classifyTransportFailure([]byte(tc.stdout), []byte(tc.stderr)); got != tc.want {
			t.Errorf("%s: classified %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTransportRefusalValid(t *testing.T) {
	t.Parallel()
	for _, r := range AllTransportRefusals {
		if !r.valid() {
			t.Errorf("registered refusal %q reports invalid", r)
		}
	}
	if TransportRefusal("").valid() {
		t.Error("zero refusal reports valid")
	}
}

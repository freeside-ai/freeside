package export

import (
	"math"
	"testing"
)

// TestBlobAllowedOverflow: a hostile size near MaxInt64 must not wrap the
// aggregate-budget arithmetic negative and slip past the cap. The blob-count
// rows check that budget refuses exactly once it is spent.
func TestBlobAllowedOverflow(t *testing.T) {
	cases := []struct {
		name    string
		size    int64
		written int64
		blobs   int
		opts    Options
		want    bool
	}{
		{"within budget", 10, 5, 0, Options{MaxTotalBlobBytes: 20}, true},
		{"exactly remaining headroom", 15, 5, 0, Options{MaxTotalBlobBytes: 20}, true},
		{"over remaining headroom", 16, 5, 0, Options{MaxTotalBlobBytes: 20}, false},
		{"hostile size would wrap a sum", math.MaxInt64, 5, 0, Options{MaxTotalBlobBytes: 20}, false},
		{"hostile size with budget disabled", math.MaxInt64, 5, 0, Options{}, true},
		{"per-file cap still applies", math.MaxInt64, 0, 0, Options{MaxBlobBytes: 100, MaxTotalBlobBytes: 0}, false},
		{"below blob count", 1, 0, 2, Options{MaxBlobs: 3}, true},
		{"at blob count", 1, 0, 3, Options{MaxBlobs: 3}, false},
		{"blob count disabled", 1, 0, 3, Options{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blobAllowed(tc.size, tc.written, tc.blobs, tc.opts); got != tc.want {
				t.Fatalf("blobAllowed(%d, %d, %d, %+v) = %v, want %v", tc.size, tc.written, tc.blobs, tc.opts, got, tc.want)
			}
		})
	}
}

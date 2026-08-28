package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func TestCandidateBodyValidatorsAgreeAtBudgetBoundary(t *testing.T) {
	t.Parallel()
	low, high := 0, 64<<10
	for low < high {
		mid := (low + high + 1) / 2
		if err := publish.ValidateCandidateBody(strings.Repeat("x", mid)); err == nil {
			low = mid
		} else {
			high = mid - 1
		}
	}

	for _, size := range []int{low - 1, low, low + 1} {
		body := strings.Repeat("x", size)
		publishErr := publish.ValidateCandidateBody(body)
		fakeErr := fakepublication.ValidateCandidateBody(body)
		if fmt.Sprint(publishErr) != fmt.Sprint(fakeErr) {
			t.Errorf("%d-byte body: publish error %q, fake error %q", size, publishErr, fakeErr)
		}
	}
}

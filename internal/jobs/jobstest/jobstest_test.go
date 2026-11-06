package jobstest

import (
	"context"
	"errors"
	"strconv"
	"testing"
)

func TestCheckIdempotent(t *testing.T) {
	n := 0
	appendOnce := func(context.Context) error { n = 1; return nil }
	appendEach := func(context.Context) error { n++; return nil }
	digest := func(context.Context) (string, error) { return strconv.Itoa(n), nil }

	if err := CheckIdempotent(t.Context(), appendOnce, digest); err != nil {
		t.Errorf("idempotent work rejected: %v", err)
	}
	n = 0
	if err := CheckIdempotent(t.Context(), appendEach, digest); err == nil {
		t.Error("non-idempotent work accepted")
	}
	boom := errors.New("boom")
	if err := CheckIdempotent(t.Context(), func(context.Context) error { return boom }, digest); !errors.Is(err, boom) {
		t.Errorf("run error not reported: %v", err)
	}
}

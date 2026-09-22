package clock

import (
	"context"
	"errors"
	"testing"
)

func TestRealClock(t *testing.T) {
	c := Real{}
	if c.Now().IsZero() {
		t.Fatal("zero current time")
	}
	if err := c.Wait(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Wait(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatal("expected cancellation")
	}
}

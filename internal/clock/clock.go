// Package clock provides cancelable waits for future lifecycle countdowns.
package clock

import (
	"context"
	"time"
)

type Clock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Package clock injects time wherever a week boundary or timestamp matters,
// so tests never depend on the real clock.
package clock

import "time"

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// Real is the system clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

// Fake is a settable clock for tests.
type Fake struct {
	T time.Time
}

func (f *Fake) Now() time.Time { return f.T }

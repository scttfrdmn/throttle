package bedrock

import "time"

// stall bounds how long one forwarding step waits for the caller to accept an
// event.
//
// It exists because a governed stream renews a budget hold while it is alive, so a
// caller who neither reads, closes, nor cancels would otherwise pin a goroutine and
// keep headroom encumbered indefinitely. The bound is per event rather than per
// stream: a slow consumer is fine, a stopped one is not.
//
// Shared by every governed event stream in this package. The reset sequence below
// -- stop, drain a timer that has already fired, then reset -- is the one piece of
// this machinery that is genuinely easy to get wrong, and having one copy of it is
// worth more than a generic stream framework would be.
type stall struct {
	timeout time.Duration
	timer   *time.Timer
}

// next arms the bound for one forwarding step and returns the channel to select on.
//
// A zero timeout means unbounded and yields a nil channel, which blocks forever in
// a select -- exactly the behaviour "no stall bound" should have.
func (s *stall) next() <-chan time.Time {
	if s.timeout <= 0 {
		return nil
	}
	if s.timer == nil {
		s.timer = time.NewTimer(s.timeout)
		return s.timer.C
	}
	if !s.timer.Stop() {
		// The timer already fired and nobody read it. Draining is what keeps the
		// following Reset from returning an instantly-expired channel.
		select {
		case <-s.timer.C:
		default:
		}
	}
	s.timer.Reset(s.timeout)
	return s.timer.C
}

// stop releases the bound at the terminal state.
func (s *stall) stop() {
	if s.timer != nil {
		s.timer.Stop()
	}
}

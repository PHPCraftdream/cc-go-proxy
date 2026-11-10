package codex

import (
	"time"
)

// A websocket is replaced at a planned age (wsManagerMaxAge, 25 minutes): the
// backend ends connections after a fixed age (30 minutes seen), and replacing
// first keeps a request from meeting the cut. If the backend ends connections
// younger than planned, again and again, the age is lowered a step at a time
// until they stop.
const (
	// wsLearnMinAge: a connection that broke younger than this broke for some
	// other reason (network, restart), not by an age limit.
	wsLearnMinAge = 5 * time.Minute
	// wsLowerAfter: this many early breaks lower the age.
	wsLowerAfter = 2
	// wsLowerWindow: early breaks further apart than this do not add up.
	wsLowerWindow = time.Hour
	// wsLowerStep is how much the age drops each time.
	wsLowerStep = 5 * time.Minute
	// wsLimitFloor is the lowest planned age.
	wsLimitFloor = 5 * time.Minute
	// wsLimitRestore: a lowered age goes back to the default after this long
	// without an early break, so a backend that relaxed its limit is used again.
	wsLimitRestore = 6 * time.Hour
)

// WSLimit says that the planned age of connections was lowered.
type WSLimit struct {
	Age    time.Duration // the planned age now
	Was    time.Duration // and before
	Broke  time.Duration // age of the connection whose break lowered it
	Status int           // websocket close status of that break, -1 none
}

// wsLifetime is what one owner (endpoint + account + way out) has shown.
type wsLifetime struct {
	limit     time.Duration // lowered planned age, 0 = the default
	early     int           // early breaks counted toward the next step
	lastEarly time.Time
	touched   time.Time // last early break or step
}

// current is the planned age at now, given the default.
func (l *wsLifetime) current(def time.Duration, now time.Time) time.Duration {
	if l.limit != 0 && now.Sub(l.touched) > wsLimitRestore {
		l.limit, l.early = 0, 0
	}
	if l.limit != 0 && l.limit < def {
		return l.limit
	}
	return def
}

// observe takes a break of a connection of the given age (the peer ended it);
// it returns the planned age afterwards and whether this break lowered it.
func (l *wsLifetime) observe(def, age time.Duration, now time.Time) (limit time.Duration, lowered bool) {
	cur := l.current(def, now)
	if age < wsLearnMinAge || age >= cur {
		return cur, false
	}
	if now.Sub(l.lastEarly) > wsLowerWindow {
		l.early = 0
	}
	l.early++
	l.lastEarly, l.touched = now, now
	if l.early < wsLowerAfter {
		return cur, false
	}
	l.early = 0
	next := max(cur-wsLowerStep, wsLimitFloor)
	if next >= cur {
		return cur, false
	}
	l.limit = next
	return next, true
}

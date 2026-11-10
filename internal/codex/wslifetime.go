package codex

import (
	"sort"
	"time"
)

// The backend ends a websocket after a fixed age (30 minutes seen, documented
// as 60). A break that finds a connection mid-request costs a retry; the manager
// learns the age from the breaks it sees and replaces connections just before.
const (
	// wsLearnMinAge: a connection that broke younger than this broke for some
	// other reason (network, restart), not by an age limit.
	wsLearnMinAge = 5 * time.Minute
	// wsLearnSamples: how many recent breaks are compared.
	wsLearnSamples = 5
	// wsLearnTolerance: breaks within this share of each other's age agree on a limit.
	wsLearnTolerance = 0.10
	// wsLearnExpire: a learned limit is dropped after this long (its samples are
	// kept), so a backend that changed its limit is found out again at the cost
	// of one break.
	wsLearnExpire = 12 * time.Hour
	// wsLearnChange: a new limit within this of the old one is the same limit.
	wsLearnChange = 10 * time.Second
)

// WSLimit says what the manager learned about the backend's connection age.
type WSLimit struct {
	Limit        time.Duration // the age the backend ends connections at
	ReplaceAfter time.Duration // connections are replaced at this age
	Samples      int           // breaks that agree
	Status       int           // websocket close status of the last break, -1 none
}

// wsLifetime is what one owner (endpoint + account + way out) has shown.
type wsLifetime struct {
	ages      []time.Duration
	limit     time.Duration
	learnedAt time.Time
}

// wsReplaceAfter is the age to replace a connection at, given the backend's
// limit: a tenth of it earlier (1 to 5 minutes), so a request that starts just
// before and runs a few minutes still ends before the cut.
func wsReplaceAfter(limit time.Duration) time.Duration {
	lead := min(max(limit/10, time.Minute), 5*time.Minute)
	return limit - lead
}

// observe takes a break at age (the peer ended a connection of that age) and
// returns the learned limit and whether it is new or changed.
func (l *wsLifetime) observe(age time.Duration, now time.Time) (limit time.Duration, samples int, changed bool) {
	l.expire(now)
	if age >= wsLearnMinAge {
		l.ages = append(l.ages, age)
		if len(l.ages) > wsLearnSamples {
			l.ages = l.ages[len(l.ages)-wsLearnSamples:]
		}
	} else {
		return l.limit, 0, false
	}
	var group []time.Duration
	for _, a := range l.ages {
		if diff := a - age; max(diff, -diff) <= time.Duration(wsLearnTolerance*float64(age)) {
			group = append(group, a)
		}
	}
	if len(group) < 2 {
		return l.limit, len(group), false
	}
	sort.Slice(group, func(i, j int) bool { return group[i] < group[j] })
	candidate := group[0] // the earliest agreeing break: never replace too late
	if l.limit != 0 && candidate > l.limit-wsLearnChange && candidate < l.limit+wsLearnChange {
		l.learnedAt = now
		return l.limit, len(group), false
	}
	l.limit, l.learnedAt = candidate, now
	return l.limit, len(group), true
}

// current is the learned limit at now, 0 when none or forgotten.
func (l *wsLifetime) current(now time.Time) time.Duration {
	l.expire(now)
	return l.limit
}

func (l *wsLifetime) expire(now time.Time) {
	// The samples stay: one more break at the same age brings the limit back.
	if l.limit != 0 && now.Sub(l.learnedAt) > wsLearnExpire {
		l.limit = 0
	}
}

package liveactivities

// Usage is what the registration caps read about the table before a
// register (SC-03): row counts table-wide, in the region and on the push
// token and stop, the region's distinct stops, and whether the activity is
// already registered (and on which stop).
type Usage struct {
	Total          int64
	InRegion       int64
	StopsInRegion  int64
	ForPushToken   int64
	OnStop         int64
	Exists         bool
	ExistingStopID string
}

// Caps bounds live subscriptions (SC-03). Every live row costs a push a
// minute and every distinct stop an upstream call a minute for up to
// Lifetime, and registration is unauthenticated. A zero field means "use
// the DefaultCaps value".
type Caps struct {
	// Total caps rows across every region.
	Total int
	// PerRegion caps rows in one region.
	PerRegion int
	// StopsPerRegion caps the distinct stops one region's rows may name.
	StopsPerRegion int
	// PerPushToken caps rows sharing one ActivityKit push token. A push
	// token belongs to exactly one activity, so a real client has one.
	PerPushToken int
}

// DefaultCaps sit far above real use: a Live Activity is one rider
// watching one bookmark for at most eight hours, and a region's riders
// concentrate on a few hundred busy stops.
var DefaultCaps = Caps{Total: defaultTotalCap, PerRegion: 5000, StopsPerRegion: 1000, PerPushToken: 5}

// defaultTotalCap is DefaultCaps.Total, a constant so the updater's stop
// cache can be sized from it.
const defaultTotalCap = 20000

// CapResult is the outcome of Caps.Check.
type CapResult int

// The outcomes of Caps.Check.
const (
	// CapOK admits the registration.
	CapOK CapResult = iota
	// CapPushToken refuses it: the push token already has PerPushToken rows.
	CapPushToken
	// CapCapacity refuses it: the table, the region, or the region's
	// distinct stops are full.
	CapCapacity
)

// WithDefaults fills every zero field from DefaultCaps.
func (c Caps) WithDefaults() Caps {
	pick := func(v, d int) int {
		if v <= 0 {
			return d
		}
		return v
	}
	return Caps{
		Total:          pick(c.Total, DefaultCaps.Total),
		PerRegion:      pick(c.PerRegion, DefaultCaps.PerRegion),
		StopsPerRegion: pick(c.StopsPerRegion, DefaultCaps.StopsPerRegion),
		PerPushToken:   pick(c.PerPushToken, DefaultCaps.PerPushToken),
	}
}

// Check decides a registration for stopID given u. A re-registration of an
// existing activity (ActivityKit token rotation) is never refused for row
// counts -- refusing it would freeze a card mid-trip -- and is refused for
// the stop cap only if it moves the row to a stop the region does not
// already have.
func (c Caps) Check(u Usage, stopID string) CapResult {
	c = c.WithDefaults()
	newStop := u.OnStop == 0 && (!u.Exists || u.ExistingStopID != stopID)
	if newStop && u.StopsInRegion >= int64(c.StopsPerRegion) {
		return CapCapacity
	}
	if u.Exists {
		return CapOK
	}
	if u.ForPushToken >= int64(c.PerPushToken) {
		return CapPushToken
	}
	if u.Total >= int64(c.Total) || u.InRegion >= int64(c.PerRegion) {
		return CapCapacity
	}
	return CapOK
}

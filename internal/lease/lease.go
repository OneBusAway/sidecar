// Package lease is the coordination primitive under the sidecar's
// background loops (spec section 12). Every loop -- alarm checker, Live
// Activity updater, alert push dispatcher, pruning, directory sync, ghost
// bus enrichment -- is written to be safe under at-least-once execution,
// but "safe" is not "free": two processes sweeping the same alarms table
// would double every OBA lookup and race on every push. A named, expiring
// lease in the shared database makes one process the loop's owner at a
// time, with ownership passing to a survivor within one TTL plus a poll
// after the owner dies, and at the survivor's next poll after a clean
// shutdown.
package lease

import (
	"context"
	"time"
)

// Repository persists named leases. Implementations must make Acquire
// atomic: two holders racing for a free lease must see exactly one true.
type Repository interface {
	// Acquire takes the named lease for holder if it is free, has expired
	// at now, or is already holder's own -- in which case the expiry is
	// pushed out to now+ttl (this is how a live holder renews). It reports
	// whether holder holds the lease afterwards. Expiry is inclusive: a
	// lease whose expiry equals now is free. ttl must be positive. now is
	// compared against the instants other processes wrote, so the
	// coordination is only as good as the participants' clock agreement.
	Acquire(ctx context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error)
	// Release drops the named lease if holder holds it, and does nothing
	// otherwise (including for a name nobody holds).
	Release(ctx context.Context, name, holder string) error
}

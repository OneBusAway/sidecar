package sqlite_test

import (
	"testing"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
	"github.com/OneBusAway/sidecar/internal/store/storetest"
)

// TestGhostBusRepositoryConformance runs the shared ghost bus conformance
// suite against the SQLite adapter. When a Postgres adapter is added, it
// runs the same suite unchanged to prove behavioral equivalence.
func TestGhostBusRepositoryConformance(t *testing.T) {
	t.Parallel()

	storetest.RunGhostBusRepository(t, func(t *testing.T) (ghostbus.Repository, regions.Repository) {
		t.Helper()
		s := sqlitetest.Open(t)
		return s.GhostBus(), s.Regions()
	})
}

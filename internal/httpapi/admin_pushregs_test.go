package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// TestAdminPushRegistrations_Count. Counts only: no token listing exists,
// and adding one later must be a deliberate change rather than a field.
func TestAdminPushRegistrations_Count(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	f.seedRegistrationOn(t, regionPuget, "ios-1", "ios", false)
	f.seedRegistrationOn(t, regionPuget, "ios-2", "ios", true)
	f.seedRegistrationOn(t, regionPuget, "android-1", "android", false)
	f.seedRegistrationOn(t, regionTampa, "elsewhere", "ios", false)

	got := object(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/push_registrations/count", ""), http.StatusOK)
	assertKeys(t, "counts", got, []string{"total", "ios", "android", "test"})
	if got["total"] != float64(3) || got["ios"] != float64(2) || got["android"] != float64(1) {
		t.Errorf("counts = %v", got)
	}
	test, _ := got["test"].(map[string]any)
	if test["total"] != float64(1) || test["ios"] != float64(1) || test["android"] != float64(0) {
		t.Errorf("test counts = %v", got["test"])
	}
	if strings.Contains(bodyText(f.do(http.MethodGet, "/api/admin/v1/regions/1/push_registrations/count", "")), "ios-1") {
		t.Error("a token reached the response")
	}
}

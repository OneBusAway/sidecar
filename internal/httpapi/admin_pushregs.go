package httpapi

import "net/http"

// pushRegistrationCountJSON is one region's registration counts (design spec
// section 5.5). The `test` sub-object is a second CountAudience call rather
// than a subtraction, so the two numbers cannot drift against each other:
// audienceCountJSON already exists for the alert-push family (admin_pushes.go),
// and is reused here rather than redefined.
type pushRegistrationCountJSON struct {
	audienceCountJSON
	Test audienceCountJSON `json:"test"`
}

// adminPushRegsHandler serves the read-only push registration count route
// (design spec section 5.5). Counts only: there is deliberately no token
// listing, so adding one later has to be a visible, deliberate change to
// this file rather than a field that appeared on a struct nobody re-audited.
type adminPushRegsHandler struct {
	deps Deps
}

// count handles GET /api/admin/v1/regions/{regionId}/push_registrations/count.
func (h *adminPushRegsHandler) count(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	all, err := h.deps.PushRegs.CountAudience(r.Context(), region.ID, false)
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "count push registrations", err)
		return
	}
	test, err := h.deps.PushRegs.CountAudience(r.Context(), region.ID, true)
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "count test push registrations", err)
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, pushRegistrationCountJSON{
		audienceCountJSON: toAudienceCountJSON(all),
		Test:              toAudienceCountJSON(test),
	})
}

package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
)

// regionJSON is the response shape for one region (design spec §5).
// DefaultAgencyID and Timezone are the two locally-managed fields: the
// regions directory carries neither, and the periodic refresh must never
// overwrite them.
type regionJSON struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	OBABaseURL      string `json:"oba_base_url"`
	SidecarBaseURL  string `json:"sidecar_base_url"`
	Language        string `json:"language"`
	Active          bool   `json:"active"`
	DefaultAgencyID string `json:"default_agency_id"`
	Timezone        string `json:"timezone"`
}

// patchRegionRequest is the PATCH /regions/{id} body. Both fields are
// pointers so an omitted one keeps its current value; the store writes both
// columns in one statement, so the handler has to merge before writing.
type patchRegionRequest struct {
	DefaultAgencyID *string `json:"default_agency_id"`
	Timezone        *string `json:"timezone"`
}

// adminRegionsHandler serves the authenticated region endpoints. Regions
// themselves come from the directory, so there is no create or delete here --
// only the two locally-managed fields are editable.
type adminRegionsHandler struct {
	deps Deps
}

// list handles GET /api/admin/v1/regions.
func (h *adminRegionsHandler) list(w http.ResponseWriter, r *http.Request) {
	list, err := h.deps.Regions.List(r.Context())
	if err != nil {
		writeStoreError(w, h.deps.Logger, "list regions", err)
		return
	}
	out := make([]regionJSON, 0, len(list))
	for _, reg := range list {
		out = append(out, toRegionJSON(reg))
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, out)
}

// patch handles PATCH /api/admin/v1/regions/{id}.
//
// An unknown id is a 404 rather than an implicit insert: regions come from the
// directory, and SetLocalFields on a missing row would report success while
// writing nothing.
func (h *adminRegionsHandler) patch(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}

	var req patchRegionRequest
	if decodeErr := decodeJSON(w, r, maxAdminBody, &req); decodeErr != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, decodeErr.Error())
		return
	}
	if req.DefaultAgencyID == nil && req.Timezone == nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest,
			"provide default_agency_id and/or timezone")
		return
	}

	ctx := r.Context()
	current, err := h.deps.Regions.Get(ctx, id)
	if err != nil {
		writeRegionError(w, h.deps.Logger, "get region", id, err)
		return
	}

	agencyID := current.DefaultAgencyID
	if req.DefaultAgencyID != nil {
		// An empty value is allowed: it clears the default, after which
		// creating an alert for this region requires an explicit agency_id.
		agencyID = *req.DefaultAgencyID
	}

	timezone := current.Timezone
	if req.Timezone != nil {
		if tzErr := validateTimezone(*req.Timezone); tzErr != nil {
			writeJSONError(w, h.deps.Logger, http.StatusBadRequest, tzErr.Error())
			return
		}
		timezone = *req.Timezone
	}

	// No new flag or JSON field carries OBAAPIKey yet -- a later task adds
	// those. Carrying the current value through here is what keeps this PATCH
	// from silently wiping a key an operator already configured.
	if setErr := h.deps.Regions.SetLocalFields(ctx, id, regions.LocalFields{
		DefaultAgencyID: agencyID,
		Timezone:        timezone,
		OBAAPIKey:       current.OBAAPIKey,
	}, h.deps.Now()); setErr != nil {
		writeStoreError(w, h.deps.Logger, "set region local fields", setErr)
		return
	}

	updated, err := h.deps.Regions.Get(ctx, id)
	if err != nil {
		writeRegionError(w, h.deps.Logger, "get updated region", id, err)
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, toRegionJSON(updated))
}

// validateTimezone rejects the two values time.LoadLocation accepts but this
// system must not store, along with ordinary typos.
//
// LoadLocation returns a nil error for both "" and "Local": storing "" would
// silently blank a configured zone, and "Local" would resolve to whatever zone
// the server process happens to run in -- exactly the machine-local dependence
// the design bans everywhere else, and invisible once written. Validating here
// puts the error at the point of the mistake rather than in a timestamp error
// message weeks later.
func validateTimezone(tz string) error {
	switch tz {
	case "":
		return fmt.Errorf("timezone must not be empty; pass an IANA zone name such as America/Los_Angeles")
	case "Local":
		return fmt.Errorf("timezone %q is machine-dependent; use an explicit IANA zone name", tz)
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", tz, err)
	}
	return nil
}

// toRegionJSON renders a stored region for the API.
func toRegionJSON(r regions.Region) regionJSON {
	return regionJSON{
		ID:              r.ID,
		Name:            r.Name,
		OBABaseURL:      r.OBABaseURL,
		SidecarBaseURL:  r.SidecarBaseURL,
		Language:        r.Language,
		Active:          r.Active,
		DefaultAgencyID: r.DefaultAgencyID,
		Timezone:        r.Timezone,
	}
}

package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/pushreg"
)

// pushNotFoundMessage is the client-facing 404 body for every alert-push
// lookup: the push id is unknown, or it belongs to a different alert.
const pushNotFoundMessage = "alert push not found"

// pushJSON is the admin wire shape of one alert push (design spec §2.9).
//
// LastError is a plain string rather than a pointer because "no error yet"
// and "an error whose text is empty" are the same thing to the SPA, and a
// nullable field would make every caller unwrap it. FailureReasons and
// Messages are always present as an array and an object: the SPA iterates
// both unconditionally.
type pushJSON struct {
	ID             int64               `json:"id"`
	AlertID        int64               `json:"alert_id"`
	RegionID       int64               `json:"region_id"`
	Audience       string              `json:"audience"`
	Status         string              `json:"status"`
	DeviceCount    int64               `json:"device_count"`
	SubmittedCount int64               `json:"submitted_count"`
	FailedCount    int64               `json:"failed_count"`
	Attempts       int64               `json:"attempts"`
	LastError      string              `json:"last_error"`
	Messages       alertpush.Messages  `json:"messages"`
	FailureReasons []failureReasonJSON `json:"failure_reasons"`
	CreatedAt      string              `json:"created_at"`
	StartedAt      *string             `json:"started_at"`
	CompletedAt    *string             `json:"completed_at"`
}

// failureReasonJSON is one grouped row of a push's failure accounting
// (design spec §2.8). Only counts and reasons cross the wire; the tokens
// behind them are stored as hashes and never read back.
type failureReasonJSON struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// audienceCountJSON is one audience's size split by platform.
type audienceCountJSON struct {
	Total   int64 `json:"total"`
	IOS     int64 `json:"ios"`
	Android int64 `json:"android"`
}

// audienceJSON is the reach preview for one alert. ForcedTest is true when
// the alert's own test flag overrules the audience choice, so the SPA can
// hide a control the server would ignore.
type audienceJSON struct {
	All        audienceCountJSON `json:"all"`
	Test       audienceCountJSON `json:"test"`
	ForcedTest bool              `json:"forced_test"`
}

// createPushRequest is the POST /alerts/{id}/pushes body. An absent audience
// means "all", so an empty body is a complete request. Messages, when
// present, replaces the mechanical copy derivation with the caller's own
// per-language {title, body} snapshot (migration design spec §2.3); it is
// the same shape pushJSON emits. Absent (nil) derives as before; present
// but empty is a 400, since it can only mean the caller forgot the text.
type createPushRequest struct {
	Audience string             `json:"audience"`
	Messages alertpush.Messages `json:"messages"`
}

// adminPushesHandler serves the alert push routes (design spec §2.9). It is
// only reachable when a transport is configured; see alertPushRoutesEnabled.
type adminPushesHandler struct {
	deps Deps
}

// enqueuer builds the shared precondition/enqueue path. The admin API and
// the CLI go through the same Enqueuer so the two trigger surfaces cannot
// drift on what "may this be sent" means.
func (h *adminPushesHandler) enqueuer() *alertpush.Enqueuer {
	return &alertpush.Enqueuer{
		Repo:     h.deps.AlertPushes,
		Alerts:   h.deps.Alerts,
		PushRegs: h.deps.PushRegs,
	}
}

// create handles POST /api/admin/v1/regions/{regionId}/alerts/{id}/pushes.
func (h *adminPushesHandler) create(w http.ResponseWriter, r *http.Request) {
	alert, ok := loadAlert(w, r, h.deps)
	if !ok {
		return
	}

	// An empty body is a valid request meaning {}: the SPA's send button has
	// nothing to say beyond the alert id, and demanding "{}" would make the
	// simplest possible client the one that fails. decodeJSON reports an
	// empty body as io.EOF, which is the only decode error that is not a
	// malformed body.
	var req createPushRequest
	if decodeErr := decodeJSON(w, r, maxAdminBody, &req); decodeErr != nil && !errors.Is(decodeErr, io.EOF) {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, decodeErr.Error())
		return
	}

	audience, err := alertpush.ParseAudience(req.Audience)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}

	p, err := h.enqueuer().Enqueue(r.Context(), alert.ID, audience, req.Messages, h.deps.Now())
	if err != nil {
		h.enqueueError(w, err)
		return
	}
	// Non-nil by construction: these routes only exist when the waker does.
	// Waking after the insert (not before) means the dispatcher can only
	// ever find a row that is already there.
	h.deps.AlertPushWaker.Wake()
	writeJSON(w, h.deps.Logger, http.StatusAccepted, toPushJSON(p))
}

// list handles GET /api/admin/v1/regions/{regionId}/alerts/{id}/pushes,
// newest first.
func (h *adminPushesHandler) list(w http.ResponseWriter, r *http.Request) {
	// The alert is loaded first so an unknown (or another region's) id is a
	// 404 rather than an empty array: "this alert has never been pushed" and
	// "there is no such alert" are different answers, and the SPA renders
	// them differently.
	alert, ok := loadAlert(w, r, h.deps)
	if !ok {
		return
	}

	pushes, err := h.deps.AlertPushes.ListByAlert(r.Context(), alert.ID)
	if err != nil {
		h.storeError(w, "list alert pushes", err)
		return
	}
	// make, not nil: the response is always an array.
	out := make([]pushJSON, 0, len(pushes))
	for _, p := range pushes {
		out = append(out, toPushJSON(p))
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, out)
}

// cancel handles
// DELETE /api/admin/v1/regions/{regionId}/alerts/{id}/pushes/{pushId}.
func (h *adminPushesHandler) cancel(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	alert, ok := loadAlert(w, r, h.deps)
	if !ok {
		return
	}
	pushID, err := pathInt64(r, "pushId")
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}

	// The push is read before it is canceled so a push belonging to another
	// alert -- or another region -- is a 404 rather than a successful cancel
	// of somebody else's send: {pushId} is globally unique, so without these
	// checks the {id} and {regionId} segments would be decoration. The row
	// carries its own region id, so it is asserted directly rather than
	// inferred from the alert.
	p, err := h.deps.AlertPushes.Get(r.Context(), pushID)
	if err != nil || p.AlertID != alert.ID || p.RegionID != region.ID {
		if err != nil && !errors.Is(err, alertpush.ErrNotFound) {
			serverErrorJSON(w, h.deps.Logger, "get alert push", err)
			return
		}
		writeJSONError(w, h.deps.Logger, http.StatusNotFound, pushNotFoundMessage)
		return
	}

	switch err := h.deps.AlertPushes.Cancel(r.Context(), pushID, h.deps.Now()); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, alertpush.ErrNotFound):
		writeJSONError(w, h.deps.Logger, http.StatusNotFound, pushNotFoundMessage)
	case errors.Is(err, alertpush.ErrTerminal):
		// The sentinel's own text, not err.Error(): the repository wraps its
		// sentinels with the statement that failed, and that wrapper is for
		// the operator's log, not the client's screen (design spec §5).
		writeJSONError(w, h.deps.Logger, http.StatusConflict, alertpush.ErrTerminal.Error())
	default:
		serverErrorJSON(w, h.deps.Logger, "cancel alert push", err)
	}
}

// audience handles
// GET /api/admin/v1/regions/{regionId}/alerts/{id}/push_audience.
func (h *adminPushesHandler) audience(w http.ResponseWriter, r *http.Request) {
	alert, ok := loadAlert(w, r, h.deps)
	if !ok {
		return
	}
	report, err := h.enqueuer().AudienceFor(r.Context(), alert.ID)
	if err != nil {
		h.storeError(w, "count push audience", err)
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, toAudienceJSON(report))
}

// enqueueError maps the Enqueuer's refusals onto the §2.2 status codes.
// Every one of them is a fact about the alert or the region, not about the
// request body, so they are 404/409 rather than 400 -- except
// ErrInvalidMessages, which is the body.
func (h *adminPushesHandler) enqueueError(w http.ResponseWriter, err error) {
	if errors.Is(err, alertpush.ErrInvalidMessages) {
		// The body, not the alert, is at fault -- and the wrapped text names
		// which field, without any store framing (design spec §5).
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, alerts.ErrNotFound) {
		writeJSONError(w, h.deps.Logger, http.StatusNotFound, "alert not found")
		return
	}
	// Each sentinel's own text is what reaches the client, never err.Error():
	// a wrapped copy would carry the failing statement into the response
	// (design spec §5).
	for _, sentinel := range []error{
		alertpush.ErrNotPublished, alertpush.ErrInFlight, alertpush.ErrEmptyAudience,
	} {
		if errors.Is(err, sentinel) {
			writeJSONError(w, h.deps.Logger, http.StatusConflict, sentinel.Error())
			return
		}
	}
	serverErrorJSON(w, h.deps.Logger, "enqueue alert push", err)
}

// storeError maps a repository error onto a response, sharing the alert and
// region not-found mapping with the rest of the admin API.
func (h *adminPushesHandler) storeError(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, alertpush.ErrNotFound) {
		writeJSONError(w, h.deps.Logger, http.StatusNotFound, pushNotFoundMessage)
		return
	}
	writeStoreError(w, h.deps.Logger, op, err)
}

// toPushJSON renders a stored push for the API: timestamps as RFC 3339 UTC,
// nil slices and maps as their empty JSON forms rather than null.
func toPushJSON(p alertpush.Push) pushJSON {
	reasons := make([]failureReasonJSON, 0, len(p.FailureReasons))
	for _, fr := range p.FailureReasons {
		reasons = append(reasons, failureReasonJSON{Reason: fr.Reason, Count: fr.Count})
	}
	messages := p.Messages
	if messages == nil {
		messages = alertpush.Messages{}
	}
	out := pushJSON{
		ID:             p.ID,
		AlertID:        p.AlertID,
		RegionID:       p.RegionID,
		Audience:       string(p.Audience),
		Status:         string(p.Status),
		DeviceCount:    p.DeviceCount,
		SubmittedCount: p.SubmittedCount,
		FailedCount:    p.FailedCount,
		Attempts:       p.Attempts,
		LastError:      p.LastError,
		Messages:       messages,
		FailureReasons: reasons,
		CreatedAt:      formatInstant(p.CreatedAt),
	}
	if p.StartedAt != nil {
		started := formatInstant(*p.StartedAt)
		out.StartedAt = &started
	}
	if p.CompletedAt != nil {
		completed := formatInstant(*p.CompletedAt)
		out.CompletedAt = &completed
	}
	return out
}

// toAudienceJSON renders a reach preview.
func toAudienceJSON(report alertpush.AudienceReport) audienceJSON {
	return audienceJSON{
		All:        toAudienceCountJSON(report.All),
		Test:       toAudienceCountJSON(report.Test),
		ForcedTest: report.ForcedTest,
	}
}

func toAudienceCountJSON(c pushreg.AudienceCount) audienceCountJSON {
	return audienceCountJSON{Total: c.Total, IOS: c.IOS, Android: c.Android}
}

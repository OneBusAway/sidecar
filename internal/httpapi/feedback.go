package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/push"
)

// feedbackLimitPerMinute bounds an unauthenticated /webhooks/gorush per
// client IP. Deliberately far above the push_registrations throttle: gorush
// reports one failure per dead token and a mass uninstall arrives in a burst,
// so this is an abuse ceiling rather than a normal-volume limit.
const feedbackLimitPerMinute = 600

// gorushFeedback is gorush's failed-push webhook payload. Token, error and
// notif_id matter here; the rest is logged context.
type gorushFeedback struct {
	Type     string `json:"type"`
	Platform string `json:"platform"`
	Token    string `json:"token"`
	Error    string `json:"error"`
	// NotifID is echoed back from the send. The alert push fan-out stamps
	// alertpush.NotifID on every batch, which is what lets an asynchronous
	// bounce be counted against the push that caused it (design spec §2.8).
	NotifID string `json:"notif_id"`
}

type feedbackHandler struct{ deps Deps }

// receive consumes async delivery feedback (spec §6.5) and prunes both the
// push registration and the Live Activity subscription tables, whichever are
// configured. Deleting either requires only knowing its token -- exactly the
// power the public opt-out DELETE endpoints already grant -- so this
// endpoint being unauthenticated adds no new capability.
func (h *feedbackHandler) receive(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		// No body is read and nothing is looked up: an unauthorized caller
		// must not be able to probe which tokens exist.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var fb gorushFeedback
	if err := decodeJSON(w, r, requestBodyLimit, &fb); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	h.recordAlertPushFailure(r, fb)
	if fb.Token == "" || !push.IsTerminal(fb.Error) {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Every configured table is pruned even when an earlier delete fails:
	// gorush's feedback dispatch is a single fire-and-forget POST (it logs a
	// non-2xx and moves on, no retry), so a delete skipped here is a delete
	// that never happens -- and a Live Activity row left behind is pushed to
	// every minute until it expires. The response is 500 if any delete
	// failed so the failure is at least visible in gorush's log.
	failed := false
	if h.deps.PushRegs != nil {
		n, err := h.deps.PushRegs.DeleteByToken(r.Context(), fb.Token)
		switch {
		case err != nil:
			h.deps.Logger.Error("httpapi: delete registration from feedback", "err", sanitizeToken(err, fb.Token))
			failed = true
		case n > 0:
			h.deps.Logger.Info("httpapi: pruned dead push token",
				"platform", fb.Platform, "reason", fb.Error, "registrations", n)
		}
	}
	if h.deps.LiveActivities != nil {
		// A terminal ActivityKit token means every future update would
		// bounce: delete the subscription, no end push (spec §6.4/§6.5).
		n, err := h.deps.LiveActivities.DeleteByPushToken(r.Context(), fb.Token)
		switch {
		case err != nil:
			h.deps.Logger.Error("httpapi: delete live activity from feedback", "err", sanitizeToken(err, fb.Token))
			failed = true
		case n > 0:
			h.deps.Logger.Info("httpapi: retired live activity for dead token",
				"platform", fb.Platform, "reason", fb.Error, "live_activities", n)
		}
	}
	if failed {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// recordAlertPushFailure counts one bounce against the push that sent it
// (design spec §2.8).
//
// It runs *before* receive's terminal-reason early return on purpose: most
// gorush bounces are non-terminal (TooManyRequests, Unavailable, ...) and
// every one of them counts against the push, so accounting that only ran on
// the terminal path would report a near-zero failure count for a send that
// mostly failed. The terminal prune is unchanged and still runs after.
//
// Nothing here can fail the request. An unrecognised notif_id is a stale or
// foreign correlation id and is skipped; a repository error (an unknown push
// id trips the foreign key) is logged at Info and dropped, because gorush
// fires the webhook once and never retries -- answering 500 would lose the
// prune signal carried in the same payload without saving the count.
func (h *feedbackHandler) recordAlertPushFailure(r *http.Request, fb gorushFeedback) {
	// Now is guaranteed by both blocks that register this route, but a nil
	// clock must not turn accounting into a panic on a live webhook.
	if fb.Token == "" || h.deps.AlertPushes == nil || h.deps.Now == nil {
		return
	}
	pushID, ok := alertpush.ParseNotifID(fb.NotifID)
	if !ok {
		return
	}
	if _, err := h.deps.AlertPushes.RecordFailure(r.Context(), pushID, fb.Token, fb.Error, h.deps.Now()); err != nil {
		h.deps.Logger.Info("httpapi: alert push feedback not recorded",
			"push_id", pushID, "err", sanitizeToken(err, fb.Token))
	}
}

// authorized reports whether r carries the configured shared secret. An empty
// FeedbackSecret leaves the endpoint open -- the throttle in NewRouter is what
// bounds it then. The comparison is constant-time so a caller cannot recover
// the secret from response timing.
func (h *feedbackHandler) authorized(r *http.Request) bool {
	if h.deps.FeedbackSecret == "" {
		return true
	}
	// The "Bearer " prefix is optional: requiring it would add no security,
	// since the secret still has to match, but would break a sender that can
	// only set a raw header value.
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(presented), []byte(h.deps.FeedbackSecret)) == 1
}

package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/OneBusAway/sidecar/internal/push"
)

// feedbackLimitPerMinute bounds an unauthenticated /webhooks/gorush per
// client IP. Deliberately far above the push_registrations throttle: gorush
// reports one failure per dead token and a mass uninstall arrives in a burst,
// so this is an abuse ceiling rather than a normal-volume limit.
const feedbackLimitPerMinute = 600

// gorushFeedback is gorush's failed-push webhook payload. Only token and
// error matter here; the rest is logged context.
type gorushFeedback struct {
	Type     string `json:"type"`
	Platform string `json:"platform"`
	Token    string `json:"token"`
	Error    string `json:"error"`
}

type feedbackHandler struct{ deps Deps }

// receive consumes async delivery feedback (spec §6.5). Deleting a
// registration requires only knowing its token -- exactly the power the
// public opt-out DELETE already grants -- so this endpoint being
// unauthenticated adds no new capability.
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
	if fb.Token == "" || !push.IsTerminal(fb.Error) {
		w.WriteHeader(http.StatusOK)
		return
	}
	n, err := h.deps.PushRegs.DeleteByToken(r.Context(), fb.Token)
	if err != nil {
		h.deps.Logger.Error("httpapi: delete registration from feedback", "err", sanitizeToken(err, fb.Token))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if n > 0 {
		h.deps.Logger.Info("httpapi: pruned dead push token",
			"platform", fb.Platform, "reason", fb.Error, "registrations", n)
	}
	w.WriteHeader(http.StatusOK)
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

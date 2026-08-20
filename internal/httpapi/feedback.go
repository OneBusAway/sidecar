package httpapi

import (
	"net/http"

	"github.com/OneBusAway/sidecar/internal/push"
)

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
	var fb gorushFeedback
	if err := decodeJSON(w, r, 64<<10, &fb); err != nil {
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

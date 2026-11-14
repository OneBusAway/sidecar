package push

import (
	"log/slog"
	"strings"
)

// MaskedTokenWarning is logged, once per process component, when gorush
// hands back a token it has masked. gorush's log.hide_token defaults to
// true and replaces the first and last ten characters of every token in its
// logs -- the same log entries it returns inline in sync mode and posts to
// the feedback webhook -- so no dead token can be matched to a registration
// and pruning silently never happens. The manifests set it to false.
const MaskedTokenWarning = "push: gorush is masking device tokens (log.hide_token); dead tokens can never be pruned or matched -- set GORUSH_LOG_HIDE_TOKEN=false on the gorush service"

// IsMaskedToken reports whether tok is a gorush-masked token. Neither an
// APNs token (hex) nor an FCM registration token ever contains '*'.
func IsMaskedToken(tok string) bool {
	return strings.Contains(tok, "*")
}

// SetLogger routes the Gorush sender's own warnings (today only
// MaskedTokenWarning) through logger instead of slog's default. A nil
// logger keeps the current one.
func (g *Gorush) SetLogger(logger *slog.Logger) {
	if logger != nil {
		g.logger = logger
	}
}

// warnMasked logs MaskedTokenWarning the first time this sender sees a
// masked token.
func (g *Gorush) warnMasked() {
	if g.maskedWarned.CompareAndSwap(false, true) {
		g.log().Error(MaskedTokenWarning)
	}
}

func (g *Gorush) log() *slog.Logger {
	if g.logger != nil {
		return g.logger
	}
	return slog.Default()
}

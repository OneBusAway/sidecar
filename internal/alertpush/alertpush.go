// Package alertpush joins the alert catalog to the push registry: one Push
// row per send of one alert, the Repository that persists it, the pure copy
// builder (messages.go), the Enqueuer that applies the send preconditions
// (enqueue.go), and the Dispatcher that performs the fan-out (dispatcher.go).
// Spec §4 "What gets pushed", §12 loop table row 3; design spec
// docs/superpowers/specs/2026-08-25-alert-push-fanout-design.md.
//
// Nothing here reads the clock; every method takes now.
package alertpush

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Fan-out constants (design spec §2.3, §2.4, §2.6, §2.7).
const (
	// BatchSize is the audience page size and the gorush batch ceiling.
	BatchSize = 500
	// TitleLimit and BodyLimit clamp push copy in runes.
	TitleLimit = 48
	BodyLimit  = 120
	// MaxAttempts is how many transport failures a push survives.
	MaxAttempts = 5
	// StuckAfter is how long a sending row may go untouched before the
	// dispatcher reclaims it (a crashed worker's work re-runs, spec §12).
	StuckAfter = 15 * time.Minute
	// FeedbackWindow is how long after a push completes that gorush feedback
	// still counts against it (SC-11). Real bounces arrive within minutes;
	// anything later is a replay or a forgery, and counting it would let an
	// old push's report be rewritten indefinitely.
	FeedbackWindow = 24 * time.Hour
	// MaxFailureReasonRunes caps a feedback failure reason (SC-11). gorush's
	// reasons are short APNs/FCM codes; the cap only bites on a forged body.
	MaxFailureReasonRunes = 120
	// EnglishKey is the Messages key for the alert's own text.
	EnglishKey = "en"
	// NotifIDPrefix prefixes the gorush notif_id stamped on every batch so
	// feedback can find its push (design spec §2.8).
	NotifIDPrefix = "alertpush:"
)

// Status is a push's lifecycle state: queued → sending → sent|failed|canceled.
type Status string

// The five statuses (design spec §2.1).
const (
	StatusQueued   Status = "queued"
	StatusSending  Status = "sending"
	StatusSent     Status = "sent"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
)

// Terminal reports whether s never changes again.
func (s Status) Terminal() bool {
	return s == StatusSent || s == StatusFailed || s == StatusCanceled
}

// Audience selects who receives a push (design spec §2.2).
type Audience string

// AudienceAll is every registration in the region; AudienceTest only
// admin-marked test devices.
const (
	AudienceAll  Audience = "all"
	AudienceTest Audience = "test"
)

// ParseAudience maps a request value onto an Audience; empty means all.
func ParseAudience(s string) (Audience, error) {
	switch strings.TrimSpace(s) {
	case "", string(AudienceAll):
		return AudienceAll, nil
	case string(AudienceTest):
		return AudienceTest, nil
	}
	return "", fmt.Errorf("audience must be %q or %q", AudienceAll, AudienceTest)
}

// Message is one language's push copy.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Messages is the per-language copy snapshot keyed by catalog language;
// English is always present under EnglishKey.
type Messages map[string]Message

// For returns the copy for a normalized locale, falling back to English for
// "" (pushreg.NormalizeLocale's no-match value) or an unknown key.
func (m Messages) For(locale string) Message {
	if msg, ok := m[locale]; ok && locale != "" {
		return msg
	}
	return m[EnglishKey]
}

// Catalog lists the translated languages (every key but English), sorted,
// in the form pushreg.NormalizeLocale expects.
func (m Messages) Catalog() []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != EnglishKey {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// FailureReason is one grouped row of a push's failure accounting.
type FailureReason struct {
	Reason string
	Count  int64
}

// Push is one send of one alert, as stored (design spec §3).
type Push struct {
	ID             int64
	AlertID        int64
	RegionID       int64
	Audience       Audience
	Status         Status
	Messages       Messages
	BatchCursor    int64 // last push_registrations.id processed
	DeviceCount    int64
	SubmittedCount int64
	FailedCount    int64
	Attempts       int64
	LastError      string
	StartedAt      *time.Time
	CompletedAt    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// FailureReasons is attached by Repository.Get and ListByAlert.
	FailureReasons []FailureReason
}

// NewPush is the input to Repository.Create. Audience and Messages are
// already resolved by the Enqueuer.
type NewPush struct {
	AlertID  int64
	RegionID int64
	Audience Audience
	Messages Messages
}

// Sentinel errors. The HTTP layer maps them to 404/409/409/409/409.
var (
	ErrNotFound      = errors.New("alert push not found")
	ErrInFlight      = errors.New("a push for this alert is already queued or sending")
	ErrTerminal      = errors.New("alert push already completed")
	ErrNotPublished  = errors.New("alert is not published")
	ErrEmptyAudience = errors.New("no registered devices in the audience")
	// ErrCooldown refuses an "all" push that would exceed the SC-27 cooldown.
	ErrCooldown = errors.New("push cooldown")
)

// Repository stores alert pushes. Implementations must be safe for
// concurrent use: the dispatcher, the admin API, and the feedback webhook
// write the same rows.
type Repository interface {
	// Create inserts a queued push; ErrInFlight if one is already queued or
	// sending for the alert (partial unique index, design spec §2.2).
	Create(ctx context.Context, in NewPush, now time.Time) (Push, error)
	// Get returns one push with FailureReasons attached; ErrNotFound if absent.
	Get(ctx context.Context, id int64) (Push, error)
	// ListByAlert returns an alert's pushes newest first, FailureReasons attached.
	ListByAlert(ctx context.Context, alertID int64) ([]Push, error)
	// InFlightForAlert reports whether a queued or sending push exists.
	InFlightForAlert(ctx context.Context, alertID int64) (bool, error)
	// Claim atomically moves every queued push, and every sending push whose
	// updated_at is before stuckBefore, to sending (stamping started_at if
	// unset and updated_at = now) and returns them ascending by id.
	Claim(ctx context.Context, now, stuckBefore time.Time) ([]Push, error)
	// SetDeviceCount records the audience size at send start; ErrNotFound if
	// the push does not exist.
	SetDeviceCount(ctx context.Context, id, n int64, now time.Time) error
	// AdvanceCursor moves batch_cursor from prevCursor to newCursor, adds
	// submitted to submitted_count, and resets attempts/last_error (a page
	// committed, so MaxAttempts counts consecutive failures) -- only while
	// status is sending and the cursor still equals prevCursor. False means
	// another worker advanced it or the operator canceled: stop (design
	// spec §2.6).
	AdvanceCursor(ctx context.Context, id, prevCursor, newCursor, submitted int64, now time.Time) (bool, error)
	// RecordFailure stores one (push, sha256(token)) failure and increments
	// failed_count; a replay of the same token is ignored and returns false.
	// So is any failure that would take failed_count past device_count, and
	// any failure for a push that completed more than FeedbackWindow before
	// now (SC-11): this is the one write an outside caller (the gorush
	// webhook) makes here, and the report must stay bounded by the send it
	// describes. An unknown push id is still an error.
	// The token itself is never stored (design spec §2.8). `now` timestamps
	// the failure row only: updated_at belongs to the dispatcher's stuck
	// clock and asynchronous webhook feedback must not reset it (§2.6).
	RecordFailure(ctx context.Context, id int64, token, reason string, now time.Time) (bool, error)
	// RecordAttempt increments attempts, stores errMsg as last_error, stamps
	// updated_at (so the stuck clock measures from the last attempt), and
	// returns the new attempt count; ErrNotFound if the push does not exist.
	RecordAttempt(ctx context.Context, id int64, errMsg string, now time.Time) (int64, error)
	// MarkCompleted moves a sending push to a terminal status, stamping
	// completed_at; false if the push was not sending (already canceled) or
	// status is not one of StatusSent, StatusFailed, or StatusCanceled.
	MarkCompleted(ctx context.Context, id int64, status Status, lastError string, now time.Time) (bool, error)
	// CountReachingAllSince counts the region's "all"-audience pushes created
	// at or after since that may have reached a device: every push except a
	// failed or canceled one with submitted_count 0 (SC-27).
	CountReachingAllSince(ctx context.Context, regionID int64, since time.Time) (int64, error)
	// Cancel moves a queued or sending push to canceled. ErrNotFound if
	// absent, ErrTerminal if already completed.
	Cancel(ctx context.Context, id int64, now time.Time) error
}

// NotifID is the gorush notif_id for a push.
func NotifID(pushID int64) string {
	return NotifIDPrefix + strconv.FormatInt(pushID, 10)
}

// ParseNotifID recovers the push id from a notif_id; ok is false for
// anything not minted by NotifID.
func ParseNotifID(s string) (int64, bool) {
	rest, found := strings.CutPrefix(s, NotifIDPrefix)
	if !found || rest == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// CleanFailureReason is the form of a feedback failure reason that may be
// stored and shown to operators (SC-11): control and non-printing runes
// removed, surrounding space trimmed, and cut to MaxFailureReasonRunes.
func CleanFailureReason(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == MaxFailureReasonRunes {
			break
		}
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// MayHaveReached reports whether p may have delivered to any device: false
// only for a failed or canceled push that never submitted a batch. The
// SC-27 cooldown counts only these.
func (p Push) MayHaveReached() bool {
	return (p.Status != StatusFailed && p.Status != StatusCanceled) || p.SubmittedCount > 0
}

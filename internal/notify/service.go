package notify

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/expiry"
	"github.com/CDRO/Inventory/internal/store"
)

// testTitle and testMessage are what the "send a test" button delivers.
//
// It carries no inventory at all, deliberately. Its job is to prove the
// channel works — that the URL has no typo in it and the token is accepted —
// and a person pressing it while setting things up has not yet decided that
// this endpoint should see their food.
const (
	testTitle   = "Inventory: test message"
	testMessage = "Notifications are set up correctly. This is a test — your daily expiry digest will look like this."
)

// Store is the slice of the store this package uses.
type Store interface {
	ClaimDueNotifications(ctx context.Context, now, dayStart time.Time) ([]store.NotificationSettings, error)
	ExpiringBatchesBefore(ctx context.Context, storageID uuid.UUID, until time.Time) ([]store.DigestItem, error)
	RecordNotificationResult(ctx context.Context, storageID uuid.UUID, result string) error
}

// Service runs the digest: what is due, what it says, and where it goes.
type Service struct {
	store  Store
	client *http.Client
	log    *slog.Logger
}

// New builds a Service with the one hardened client this package uses
// (newClient: no redirects, no cookie jar, a timeout on every attempt).
func New(s Store, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: s, client: newClient(), log: log}
}

// RunDue delivers to every storage due at now, and returns how many digests
// were actually sent.
//
// "Due" is the database's decision, not this function's: ClaimDueNotifications
// claims the rows in one statement before anything is built or posted, so a
// crash between posting and recording costs one digest rather than repeating
// it on the next start. Nothing here retries — a storage whose delivery fails
// has that written to last_result and waits for tomorrow, which is the spec's
// explicit design rather than an omission.
//
// One storage's failure never stops the others: each is independent, and a
// household whose ntfy container is down must not cost its neighbours in the
// same database their digest.
func (s *Service) RunDue(ctx context.Context, now time.Time) (int, error) {
	dayStart := startOfDay(now)
	due, err := s.store.ClaimDueNotifications(ctx, now, dayStart)
	if err != nil {
		return 0, err
	}

	sent := 0
	for _, settings := range due {
		if s.runOne(ctx, settings, now) {
			sent++
		}
	}
	return sent, nil
}

// runOne builds and delivers one storage's digest, recording the outcome. It
// reports whether a message was actually sent.
func (s *Service) runOne(ctx context.Context, settings store.NotificationSettings, now time.Time) bool {
	// Always the widest window, whatever include_soon says: the setting
	// decides what is *reported*, and Build drops the soon bucket when it is
	// off. Narrowing the query instead would put the 3-versus-14 decision in
	// two places, which is the duplicated arithmetic the spec forbids.
	until := startOfDay(now).AddDate(0, 0, expiry.SoonWithinDays)

	items, err := s.store.ExpiringBatchesBefore(ctx, settings.StorageID, until)
	if err != nil {
		s.log.Warn("digest not built", slog.String("storage_id", settings.StorageID.String()), slog.Any("err", err))
		s.record(ctx, settings.StorageID, "the inventory could not be read")
		return false
	}

	digest := Build(items, now, settings.IncludeSoon)
	if digest.Empty() {
		s.record(ctx, settings.StorageID, store.NotificationEmpty)
		return false
	}

	if err := s.Deliver(ctx, settings, digest); err != nil {
		s.log.Warn("digest delivery failed", slog.String("storage_id", settings.StorageID.String()), slog.Any("err", err))
		s.record(ctx, settings.StorageID, summaryOf(err))
		return false
	}
	s.record(ctx, settings.StorageID, store.NotificationSent)
	return true
}

// Deliver posts one built digest to one target.
//
// Exported so the handlers can render a digest without a scheduler tick,
// and so the storage's own data never travels through a second code path
// that might shape it differently.
func (s *Service) Deliver(ctx context.Context, settings store.NotificationSettings, digest Digest) error {
	return deliver(ctx, s.client, settings, digest.Title(), digest.Message(), digest.items())
}

// SendTest delivers the fixed test message and records the outcome, so the
// settings page shows the same last_result a scheduled run would have
// written. The error it returns is what the endpoint reports inline — a typo
// in a URL is found at setup time or days later, and this is the difference.
func (s *Service) SendTest(ctx context.Context, settings store.NotificationSettings) error {
	err := deliver(ctx, s.client, settings, testTitle, testMessage, nil)
	if err != nil {
		s.log.Warn("test notification failed", slog.String("storage_id", settings.StorageID.String()), slog.Any("err", err))
		s.record(ctx, settings.StorageID, summaryOf(err))
		return err
	}
	s.record(ctx, settings.StorageID, store.NotificationSent)
	return nil
}

// record writes last_result, logging rather than propagating a failure to do
// so: the digest has already been delivered or already failed, and losing the
// bookkeeping is not worth turning into the caller's problem.
func (s *Service) record(ctx context.Context, storageID uuid.UUID, result string) {
	if err := s.store.RecordNotificationResult(ctx, storageID, result); err != nil {
		s.log.Warn("recording notification result failed",
			slog.String("storage_id", storageID.String()), slog.Any("err", err))
	}
}

// summaryOf extracts the storable description from a delivery failure.
//
// The fallback is deliberately vague: anything that is not a DeliveryError
// has an error string nobody curated, and last_result is read back by the
// API. A vague sentence in the UI is a smaller problem than a leaked token.
func summaryOf(err error) string {
	var delivery *DeliveryError
	if errors.As(err, &delivery) {
		return delivery.Summary()
	}
	return "delivery failed"
}

// startOfDay is midnight in the given instant's own zone — the server's,
// since that is where "the current hour" and "already run today" are both
// judged (docs/specs/17-expiry-notifications.md).
func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

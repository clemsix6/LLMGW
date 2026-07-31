package alert

import (
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// expiryHorizon is how far ahead an expiry is announced, and the lifetime below
// which a key is deliberately short-lived and needs no warning at all.
const expiryHorizon = 7 * 24 * time.Hour

// ObserveProjectKeys records the expiry state of the keys one sweep returned.
//
// now drives the expiry classification while the tracker's own clock drives the
// anti-flap window: a test moving one must move the other, or a key can be
// classified against a time the suppression window never sees.
func (t *Tracker) ObserveProjectKeys(keys []governance.KeyInfo, now time.Time) {
	if t.disabled() {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, key := range keys {
		t.observeProjectKeyLocked(key, now)
	}
}

// observeProjectKeyLocked transitions one key, never moving its state
// backwards: a key already reported expired ignores a later expiring
// observation. The caller must hold t.mu.
func (t *Tracker) observeProjectKeyLocked(key governance.KeyInfo, now time.Time) {
	state, kind, observed := keyTransition(key, now)
	if !observed {
		return
	}

	entityKey := keyProjectKeyPrefix + key.PublicID
	if state == stateExpiring && t.entryLocked(entityKey).current == stateExpired {
		return
	}

	t.transitionLocked(entityKey, state, false, kind, kind.Title(), keyFields(key))
}

// keyTransition classifies one swept key, reporting false for every key that
// needs no warning: no expiry at all, already revoked, deliberately short-lived,
// or expiring beyond the announced horizon.
func keyTransition(key governance.KeyInfo, now time.Time) (string, Kind, bool) {
	if key.ExpiresAt == nil || key.RevokedAt != nil {
		return "", "", false
	}
	if key.ExpiresAt.Sub(key.CreatedAt) <= expiryHorizon {
		return "", "", false
	}

	if key.ExpiresAt.Before(now) {
		return stateExpired, KindProjectKeyExpired, true
	}
	if key.ExpiresAt.Sub(now) <= expiryHorizon {
		return stateExpiring, KindProjectKeyExpiring, true
	}
	return "", "", false
}

// keyFields renders the operator-facing identity of one project key. It is only
// ever reached for a key keyTransition proved carries an expiry.
func keyFields(key governance.KeyInfo) []Field {
	return []Field{
		{Name: "Project", Value: key.ProjectName},
		{Name: "Key", Value: key.Name},
		{Name: "Public ID", Value: key.PublicID},
		{Name: "Expires at", Value: key.ExpiresAt.UTC().Format(time.RFC3339)},
	}
}

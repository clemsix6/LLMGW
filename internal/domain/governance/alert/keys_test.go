package alert_test

import (
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// day is the unit every key fixture is expressed in.
const day = 24 * time.Hour

// projectKey builds one swept key whose creation and expiry are relative to
// fixedNow, which is the instant every sweep in this file runs at.
func projectKey(publicID string, createdAgo, expiresIn time.Duration) governance.KeyInfo {
	expiresAt := fixedNow.Add(expiresIn)

	return governance.KeyInfo{
		ProjectName: "alpha",
		Name:        "deploy",
		PublicID:    publicID,
		CreatedAt:   fixedNow.Add(-createdAgo),
		ExpiresAt:   &expiresAt,
	}
}

// expiringKey builds a long-lived key three days from its expiry.
func expiringKey(publicID string) governance.KeyInfo {
	return projectKey(publicID, 60*day, 3*day)
}

func TestProjectKeyTransitions(t *testing.T) {
	revoked := expiringKey("pk-revoked")
	revoked.RevokedAt = &fixedNow

	noExpiry := expiringKey("pk-no-expiry")
	noExpiry.ExpiresAt = nil

	cases := []struct {
		name string
		key  governance.KeyInfo
		want []alert.Kind
	}{
		{
			name: "a long-lived key inside the horizon is expiring",
			key:  expiringKey("pk-1"),
			want: []alert.Kind{alert.KindProjectKeyExpiring},
		},
		{
			name: "a long-lived key past its expiry is expired",
			key:  projectKey("pk-2", 60*day, -2*day),
			want: []alert.Kind{alert.KindProjectKeyExpired},
		},
		{
			name: "a key expiring beyond the horizon is quiet",
			key:  projectKey("pk-3", 60*day, 10*day),
			want: nil,
		},
		{
			name: "a deliberately short-lived key is quiet",
			key:  projectKey("pk-4", day, 3*day),
			want: nil,
		},
		{
			name: "a key whose whole lifetime is the horizon is quiet",
			key:  projectKey("pk-5", 0, 7*day),
			want: nil,
		},
		{
			name: "a revoked key is quiet",
			key:  revoked,
			want: nil,
		},
		{
			name: "a key without an expiry is quiet",
			key:  noExpiry,
			want: nil,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sink := newNotifier()
			tracker := newTracker(sink, newClock())

			tracker.ObserveProjectKeys([]governance.KeyInfo{testCase.key}, fixedNow)
			tracker.ObserveProjectKeys([]governance.KeyInfo{testCase.key}, fixedNow)

			assertKinds(t, sink, testCase.want...)
		})
	}
}

// TestProjectKeyStateNeverMovesBackwards pins that a key already reported
// expired ignores a later expiring observation.
func TestProjectKeyStateNeverMovesBackwards(t *testing.T) {
	sink := newNotifier()
	clock := newClock()
	tracker := newTracker(sink, clock)

	tracker.ObserveProjectKeys([]governance.KeyInfo{projectKey("pk-1", 60*day, -2*day)}, fixedNow)

	assertKinds(t, sink, alert.KindProjectKeyExpired)

	clock.Advance(alert.DefaultWindow)
	tracker.ObserveProjectKeys([]governance.KeyInfo{expiringKey("pk-1")}, fixedNow)

	assertKinds(t, sink, alert.KindProjectKeyExpired)
}

// TestProjectKeyFieldNames pins the exact field set of both project-key kinds.
func TestProjectKeyFieldNames(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	key := expiringKey("pk-1")
	tracker.ObserveProjectKeys([]governance.KeyInfo{key, projectKey("pk-2", 60*day, -2*day)}, fixedNow)

	assertKinds(t, sink, alert.KindProjectKeyExpiring, alert.KindProjectKeyExpired)

	expiring := eventAt(t, sink, 0)
	assertFieldNames(t, expiring, "Project", "Key", "Public ID", "Expires at")
	assertFieldNames(t, eventAt(t, sink, 1), "Project", "Key", "Public ID", "Expires at")

	if got := fieldValue(expiring, "Public ID"); got != "pk-1" {
		t.Fatalf("public id = %q, want pk-1", got)
	}
	if got := fieldValue(expiring, "Expires at"); got != key.ExpiresAt.Format(time.RFC3339) {
		t.Fatalf("expires at = %q, want %q", got, key.ExpiresAt.Format(time.RFC3339))
	}
}

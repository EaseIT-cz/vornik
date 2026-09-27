package persistence

import (
	"testing"
	"time"
)

// LeaderLockExpired is THE expiry definition shared by the doctor check and the
// release statement (horizontal scaling LLD, contract 2026-09-25). The doctor
// used expires_at <= now and DeleteExpired expires_at < now, so at the instant
// expires_at == now the doctor said "release it" and the release refused.
func TestLeaderLockExpired_IsStrict(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if LeaderLockExpired(now, now) {
		t.Fatal("expires_at == now is NOT expired: DeleteExpired's expires_at < now keeps the row")
	}
	if !LeaderLockExpired(now.Add(-time.Microsecond), now) {
		t.Fatal("a microsecond past expiry is expired")
	}
	if LeaderLockExpired(now.Add(time.Microsecond), now) {
		t.Fatal("a microsecond before expiry is not expired")
	}
}

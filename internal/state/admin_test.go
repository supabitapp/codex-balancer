package state

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

func TestAdminSessionsPruneAndReset(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	session := func(id byte, expires time.Time) AdminSession {
		return AdminSession{ID: bytes.Repeat([]byte{id}, 32), CSRF: "csrf", Credential: bytes.Repeat([]byte{9}, 32), ExpiresAt: expires}
	}
	for _, created := range []AdminSession{
		session(1, now.Add(-time.Second)),
		session(2, now.Add(time.Hour)),
		session(3, now.Add(2*time.Hour)),
		session(4, now.Add(3*time.Hour)),
	} {
		if err := store.CreateAdminSession(created, now, 2); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[byte]bool{1: false, 2: false, 3: true, 4: true} {
		got, ok, err := store.AdminSession(bytes.Repeat([]byte{id}, 32))
		if err != nil || ok != want {
			t.Fatalf("session %d present = %t, want %t, error = %v", id, ok, want, err)
		}
		if ok && (got.CSRF != "csrf" || !got.ExpiresAt.Equal(time.Unix(0, now.Add(time.Duration(id-1)*time.Hour).UnixNano()))) {
			t.Fatalf("session %d = %+v", id, got)
		}
	}
	if err := store.SetAdminPasswordHash("new-verifier"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.AdminSession(bytes.Repeat([]byte{4}, 32)); err != nil || ok {
		t.Fatalf("password change kept sessions: ok = %t, error = %v", ok, err)
	}
}

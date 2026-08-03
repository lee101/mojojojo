package main

import (
	"strings"
	"testing"
	"time"
)

func TestSignedUploadTicketRejectsTamperingAndExpiry(t *testing.T) {
	s := &Server{cfg: Config{StorageSigningKey: "test-signing-key"}}
	ticket := uploadTicket{UserID: "u1", AssetID: "a1", ObjectKey: "uploads/u1/a.txt",
		Filename: "a.txt", ContentType: "text/plain", MaxBytes: 20,
		Expires: time.Now().Add(time.Minute).Unix()}
	token, err := s.signUpload(ticket)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.verifyUpload(token)
	if err != nil || got.ObjectKey != ticket.ObjectKey {
		t.Fatalf("valid ticket did not round trip: %#v, %v", got, err)
	}
	last := token[len(token)-1]
	replacement := byte('A')
	if last == replacement {
		replacement = 'B'
	}
	if _, err := s.verifyUpload(token[:len(token)-1] + string(replacement)); err == nil {
		t.Fatal("tampered ticket was accepted")
	}
	ticket.Expires = time.Now().Add(-time.Second).Unix()
	expired, _ := s.signUpload(ticket)
	if _, err := s.verifyUpload(expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired ticket: %v", err)
	}
}

func TestStorageKeysStayInTheirTenantAndUseBrandedURLs(t *testing.T) {
	store := &uploadStore{prefix: "", publicURL: "https://mojojojostatic.mojojojo.com"}
	key, err := store.keyFor("user-1", "../../secret.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "uploads/user-1/") || strings.Contains(key, "..") {
		t.Fatalf("unsafe key: %q", key)
	}
	url := store.publicFor(key)
	if !strings.HasPrefix(url, "https://mojojojostatic.mojojojo.com/") {
		t.Fatalf("unbranded URL: %q", url)
	}
	if _, err := store.siteKey("user-1", "p1", "d1", "../other/index.html"); err == nil {
		t.Fatal("site path traversal was accepted")
	}
}

func TestObjectPrefixNormalization(t *testing.T) {
	for input, want := range map[string]string{"": "", "/": "", "mojojojo": "mojojojo/", "/sites/": "sites/"} {
		if got := objectPrefix(input); got != want {
			t.Errorf("objectPrefix(%q)=%q, want %q", input, got, want)
		}
	}
}

package store

import (
	"path/filepath"
	"testing"

	"github.com/ai-club/sandbox-host/internal/apimodel"
)

func TestPutSandboxReplacesSessionIndex(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record := apimodel.SandboxRecord{OwnerID: "team", ProjectID: "project", Sandbox: apimodel.Sandbox{Name: "box"}, Session: apimodel.Session{ID: "old"}}
	if err := s.PutSandbox(record); err != nil {
		t.Fatal(err)
	}
	record.Session.ID = "new"
	if err := s.PutSandbox(record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBySession("old"); err != ErrNotFound {
		t.Fatalf("old session lookup error = %v", err)
	}
	if got, err := s.GetBySession("new"); err != nil || got.Session.ID != "new" {
		t.Fatalf("new lookup = %#v, %v", got, err)
	}
}

func TestPutSandboxPersistsPrivateRuntimeConnection(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record := apimodel.SandboxRecord{
		OwnerID: "team", ProjectID: "project", RuntimeGuestIP: "10.200.0.2", RuntimeToken: "private-token",
		Sandbox: apimodel.Sandbox{Name: "box"}, Session: apimodel.Session{ID: "session"},
	}
	if err := s.PutSandbox(record); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBySession("session")
	if err != nil {
		t.Fatal(err)
	}
	if got.RuntimeGuestIP != record.RuntimeGuestIP || got.RuntimeToken != record.RuntimeToken {
		t.Fatalf("runtime connection was not persisted: %#v", got)
	}
}

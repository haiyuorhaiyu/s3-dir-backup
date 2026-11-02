package main

import "testing"

func TestRemotePrefix(t *testing.T) {
	got, err := remotePrefix(`/daily\backup/`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "daily/backup" {
		t.Fatalf("got %q", got)
	}
}

func TestRemotePrefixRejectsTraversal(t *testing.T) {
	if _, err := remotePrefix("daily/../backup"); err == nil {
		t.Fatal("expected traversal prefix to be rejected")
	}
}

func TestObjectKey(t *testing.T) {
	got := objectKey("daily-backup", "photos", `2026\11\image.jpg`)
	if got != "daily-backup/photos/2026/11/image.jpg" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateSourceName(t *testing.T) {
	if err := validateSourceName("project-2"); err != nil {
		t.Fatal(err)
	}
	if err := validateSourceName("../project"); err == nil {
		t.Fatal("expected path component validation to fail")
	}
}

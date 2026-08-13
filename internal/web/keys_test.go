package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAPIKeyAccountBindingsPersistAndResolve(t *testing.T) {
	path := t.TempDir() + "/api-keys.json"
	store := newAPIKeyStore(path)
	record, raw, err := store.create("bound")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.update(record.ID, "", nil, []string{"account-1", "account-2"})
	if err != nil || !updated {
		t.Fatalf("update=%v err=%v", updated, err)
	}

	reloaded := newAPIKeyStore(path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, reloaded); err != nil {
		t.Fatal(err)
	}
	got := reloaded.accountIDs(raw)
	if len(got) != 2 || got[0] != "account-1" || got[1] != "account-2" {
		t.Fatalf("accountIDs=%v", got)
	}
}

func TestAPIKeyStoreUsesConfiguredPathAfterDockerImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-keys.json")
	persisted := map[string]any{
		"Path": "/data/api-keys.json",
		"keys": []apiKeyRecord{},
	}
	b, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_API_KEYS", path)

	store := openAPIKeys()
	if store.Path != path {
		t.Fatalf("store path = %q, want configured path %q", store.Path, path)
	}
	if _, _, err := store.create("portable"); err != nil {
		t.Fatalf("create with imported Docker key file: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("configured key file was not persisted: %v", err)
	}
}

func TestAPIKeyCreateRollsBackWhenPersistenceFails(t *testing.T) {
	store := newAPIKeyStore(t.TempDir())
	if _, _, err := store.create("test"); err == nil {
		t.Fatal("expected persistence error")
	}
	if got := len(store.Keys); got != 0 {
		t.Fatalf("retained %d in-memory keys after failed save", got)
	}
}

func TestAPIKeyRevokeRollsBackWhenPersistenceFails(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	record, _, err := store.create("test")
	if err != nil {
		t.Fatal(err)
	}
	store.Path = t.TempDir()
	revoked, err := store.revoke(record.ID)
	if err == nil || revoked {
		t.Fatalf("revoke=%v err=%v, want persistence failure", revoked, err)
	}
	if store.Keys[0].Revoked {
		t.Fatal("key remained revoked after failed save")
	}
}

func TestAPIKeyDeletePhysicallyRemoves(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	r1, _, err := store.create("one")
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := store.create("two")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := store.delete(r1.ID)
	if err != nil || !deleted {
		t.Fatalf("delete=%v err=%v", deleted, err)
	}
	for _, k := range store.Keys {
		if k.ID == r1.ID {
			t.Fatal("key still present after delete")
		}
	}
	if len(store.Keys) != 1 || store.Keys[0].ID != r2.ID {
		t.Fatalf("unexpected remaining keys: %+v", store.Keys)
	}
	if deleted, _ := store.delete("no-such-id"); deleted {
		t.Fatal("delete of unknown id should report false")
	}
}

func TestAPIKeyDeleteRollsBackWhenPersistenceFails(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	record, _, err := store.create("test")
	if err != nil {
		t.Fatal(err)
	}
	store.Path = t.TempDir()
	deleted, err := store.delete(record.ID)
	if err == nil || deleted {
		t.Fatalf("delete=%v err=%v, want persistence failure", deleted, err)
	}
	if len(store.Keys) != 1 || store.Keys[0].ID != record.ID {
		t.Fatalf("key not restored after failed delete: %+v", store.Keys)
	}
}

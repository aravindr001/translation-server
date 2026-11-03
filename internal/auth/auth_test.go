package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateKeyProducesVerifiableHash(t *testing.T) {
	raw, entry, err := GenerateKey("alice")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "alice" {
		t.Fatalf("name = %q", entry.Name)
	}
	if len(raw) != 43 {
		t.Fatalf("raw key length = %d, want 43 base64url characters", len(raw))
	}
	sum := sha256.Sum256([]byte(raw))
	if entry.SHA256Hash != hex.EncodeToString(sum[:]) {
		t.Fatal("stored hash does not match SHA-256 of raw key")
	}
	if _, _, err := GenerateKey("   "); err == nil {
		t.Fatal("empty name should be rejected")
	}
}

func TestStoreLoadsArrayAndWrapperForms(t *testing.T) {
	dir := t.TempDir()
	raw, entry, err := GenerateKey("one")
	if err != nil {
		t.Fatal(err)
	}

	arrayPath := filepath.Join(dir, "array.json")
	arrayData, _ := json.Marshal([]KeyEntry{entry})
	writeFile(t, arrayPath, string(arrayData))

	wrapperPath := filepath.Join(dir, "wrapper.json")
	wrapperData, _ := json.Marshal(map[string]any{"keys": []KeyEntry{entry}})
	writeFile(t, wrapperPath, string(wrapperData))

	for _, path := range []string{arrayPath, wrapperPath} {
		store, err := NewStore(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		name, ok := store.Authenticate(raw)
		if !ok || name != "one" {
			t.Fatalf("%s: authenticate = %q, %v", path, name, ok)
		}
		if store.Count() != 1 {
			t.Fatalf("%s: count = %d", path, store.Count())
		}
	}
}

func TestReloadKeepsPreviousKeysOnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	raw, entry, err := GenerateKey("stable")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal([]KeyEntry{entry})
	writeFile(t, path, string(data))

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, `{"keys":[{"name":"broken","sha256_hash":"not-hex"}]}`)
	if err := store.Reload(); err == nil {
		t.Fatal("expected reload to fail")
	}
	if name, ok := store.Authenticate(raw); !ok || name != "stable" {
		t.Fatal("previous key set was lost after a failed reload")
	}
}

func TestStoreRejectsInvalidFiles(t *testing.T) {
	dir := t.TempDir()
	_, entry, err := GenerateKey("ok")
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"empty":        "",
		"wrong type":   `"hello"`,
		"no keys":      `[]`,
		"empty name":   `[{"name":" ","sha256_hash":"` + entry.SHA256Hash + `"}]`,
		"duplicate":    `[{"name":"ok","sha256_hash":"` + entry.SHA256Hash + `"},{"name":"ok","sha256_hash":"` + entry.SHA256Hash + `"}]`,
		"short hash":   `[{"name":"ok","sha256_hash":"abcd"}]`,
		"non-hex hash": `[{"name":"ok","sha256_hash":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}]`,
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			writeFile(t, path, contents)
			if _, err := NewStore(path); err == nil {
				t.Fatalf("%s: expected an error", name)
			}
		})
	}
}

func TestAuthenticateRejectsUnknownToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	_, entry, err := GenerateKey("known")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal([]KeyEntry{entry})
	writeFile(t, path, string(data))

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Authenticate("unknown"); ok {
		t.Fatal("unknown token was accepted")
	}
	if names := store.Names(); len(names) != 1 || names[0] != "known" {
		t.Fatalf("names = %v", names)
	}
}

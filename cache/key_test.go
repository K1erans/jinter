package cache

import (
	"encoding/hex"
	"testing"
)

func TestKeyOf(t *testing.T) {
	state := []byte(`{"path":"src/retry.ts","code":"retry()"}`)
	base := KeyOf("jev-1.13.0", "The `code` retries failed payments", state)

	same := map[string]Key{
		"case":       KeyOf("jev-1.13.0", "The `code` Retries Failed Payments", state),
		"whitespace": KeyOf("jev-1.13.0", "  The `code`  retries\tfailed payments ", state),
	}
	for name, k := range same {
		if k != base {
			t.Errorf("%s: should share the key", name)
		}
	}

	different := map[string]Key{
		"model":    KeyOf("jev-1.14.0", "The `code` retries failed payments", state),
		"question": KeyOf("jev-1.13.0", "The `code` sends email", state),
		"edited":   KeyOf("jev-1.13.0", "The `code` retries failed payments", []byte(`{"path":"src/retry.ts","code":"retry(1)"}`)),
		"renamed":  KeyOf("jev-1.13.0", "The `code` retries failed payments", []byte(`{"path":"src/retry2.ts","code":"retry()"}`)),
		// Without separators "ab"+"c" and "a"+"bc" would collide.
		"boundary": KeyOf("jev-1.13.0The `code` retries", " failed payments", state),
	}
	for name, k := range different {
		if k == base {
			t.Errorf("%s: should get a new key", name)
		}
	}
}

// TestKeysAreStable pins keys computed before KeyOf and Hash were
// optimised: a change here would silently orphan every cached answer and
// layout on disk.
func TestKeysAreStable(t *testing.T) {
	for name, c := range map[string]struct {
		got  Key
		want string
	}{
		"KeyOf":         {KeyOf("jev-1.13.0", "  The `code`  Retries\tfailed payments ", []byte(`{"path":"src/retry.ts","code":"retry()"}`)), "89d39f8f1d77526edd784d10239204e72caee07f1ce6c637b74b42e32de5f1ae"},
		"KeyOf unicode": {KeyOf("jev-1.13.0", "The `code` RÉSUMÉ parsing İ", []byte("x")), "0d2bffa91582378bc5ecb180ab062acf1db870482d3a687f27bc6bb4cd231941"},
		"Hash":          {Hash([]byte("v1"), []byte("retry.ts"), nil, []byte("src")), "b7f6c5d5db5a23920650ada0cbcb17a9cf22f24ff5e56d619609f35cb118e594"},
	} {
		if got := hex.EncodeToString(c.got[:]); got != c.want {
			t.Errorf("%s: got %s, want %s", name, got, c.want)
		}
	}
}

// FuzzKeyOf checks KeyOf's in-place normalizing against the plain
// definition.
func FuzzKeyOf(f *testing.F) {
	for _, q := range []string{"", " ", "The `code` retries", "  A\tb\n\nC \v\f\r", "Ünïcode  X", "a b", "a\u0085b", "İ", "\xff\xfeA"} {
		f.Add(q)
	}
	f.Fuzz(func(t *testing.T, q string) {
		if got, want := string(appendNormalized([]byte("prefix"), q)), "prefix"+normalize(q); got != want {
			t.Fatalf("appendNormalized(%q) = %q, want %q", q, got, want)
		}
		state := []byte(q)
		if KeyOf("m", q, state) != Hash([]byte("m"), []byte(normalize(q)), state) {
			t.Fatalf("KeyOf(%q) disagrees with Hash", q)
		}
	})
}

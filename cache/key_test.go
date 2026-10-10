package cache

import "testing"

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

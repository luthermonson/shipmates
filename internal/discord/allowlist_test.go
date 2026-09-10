package discord

import "testing"

func TestAllowlist_FailClosed(t *testing.T) {
	list := ParseAllowlist("111,222, 333 ")

	// Listed ids are allowed (whitespace around an id is tolerated on parse).
	for _, id := range []string{"111", "222", "333"} {
		if !list.Allows(id) {
			t.Errorf("Allows(%q) = false, want true (id is on the list)", id)
		}
	}

	// Unlisted ids are rejected.
	for _, id := range []string{"444", "1111", "", "  "} {
		if list.Allows(id) {
			t.Errorf("Allows(%q) = true, want false (id is not on the list)", id)
		}
	}
}

func TestAllowlist_EmptyRejectsEveryone(t *testing.T) {
	// The whole point of fail-closed: an empty allowlist commands nobody, it
	// does NOT mean "everybody".
	for _, raw := range []string{"", "   ", ",", " , , "} {
		list := ParseAllowlist(raw)
		if list.Allows("111") {
			t.Errorf("ParseAllowlist(%q).Allows(111) = true, want false — empty list must reject all", raw)
		}
		if list.Allows("") {
			t.Errorf("ParseAllowlist(%q).Allows(\"\") = true, want false", raw)
		}
	}
}

func TestAllowlist_NoWildcard(t *testing.T) {
	// A "*" is a literal id, never a wildcard — "allow everyone" must not be
	// expressible through the allowlist.
	list := ParseAllowlist("*")
	if list.Allows("111") {
		t.Error("Allows(111) = true with list \"*\"; wildcard must not be honored")
	}
	if !list.Allows("*") {
		t.Error("Allows(\"*\") = false; \"*\" should be treated as a literal id")
	}
}

package netguard

import "testing"

func TestURLAllowlist(t *testing.T) {
	a, err := ParseURLAllowlist([]string{"https://checkin.example.com/", "https://www.example.org/kiosk"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"https://checkin.example.com/":               true,
		"https://checkin.example.com":                true,
		"https://CHECKIN.example.com/visit?id=1#top": true,
		"https://checkin.example.com:443/a/b":        true,
		"http://checkin.example.com/":                false, // scheme must match
		"https://checkin.example.com:8443/":          false,
		"https://evil.checkin.example.com/":          false,
		"https://checkin.example.com.evil.net/":      false,
		"https://www.example.org/kiosk":              true,
		"https://www.example.org/kiosk/step2":        true,
		"https://www.example.org/kiosk?x=1":          true,
		"https://www.example.org/kiosk-admin":        false, // segment boundary
		"https://www.example.org/":                   false,
		"https://www.example.org/other/kiosk":        false,
		"https://user@checkin.example.com/":          true,
		"javascript:alert(1)":                        false,
		"not a url\x7f":                              false,
	}
	for raw, want := range cases {
		if got := a.Allows(raw); got != want {
			t.Errorf("Allows(%q) = %v, want %v", raw, got, want)
		}
	}
	var empty *URLAllowlist
	if !empty.Allows("https://anything.example/") {
		t.Error("an empty allowlist must allow everything")
	}
	for _, bad := range []string{"checkin.example.com", "ftp://x.com/", "/relative"} {
		if _, err := ParseURLAllowlist([]string{bad}); err == nil {
			t.Errorf("ParseURLAllowlist(%q) should fail", bad)
		}
	}
}

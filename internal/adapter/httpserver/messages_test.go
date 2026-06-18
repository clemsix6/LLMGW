package httpserver

import "testing"

// TestResolveProject proves project resolution: the X-Project header wins when present, the
// configured default is used when the header is absent, and resolution fails (ok == false) only
// when neither is set — the case the handler maps to a 400.
func TestResolveProject(t *testing.T) {
	cases := []struct {
		name     string
		header   string
		fallback string
		want     string
		wantOK   bool
	}{
		{"header present", "truewallet", "", "truewallet", true},
		{"header beats default", "explicit", "fallback", "explicit", true},
		{"default used when header absent", "", "truewallet", "truewallet", true},
		{"neither set fails", "", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := resolveProject(c.header, c.fallback)
			if got != c.want || ok != c.wantOK {
				t.Errorf("resolveProject(%q, %q) = (%q, %v), want (%q, %v)", c.header, c.fallback, got, ok, c.want, c.wantOK)
			}
		})
	}
}

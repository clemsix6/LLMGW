package claudemax

import (
	"regexp"
	"testing"
)

func TestBillingHeaderMatchesClewdrGoldenVectors(t *testing.T) {
	// These vectors are reproduced from clewdr's own unit tests (version 2.1.76), proving
	// the salt + UTF-16 sampling + sha256 truncation are byte-for-byte compatible.
	stale := spoof{version: "2.1.76"}

	cases := []struct {
		name          string
		firstUserText string
		want          string
	}{
		{
			name:          "short text samples out-of-range to zeros",
			firstUserText: "hey",
			want:          "x-anthropic-billing-header: cc_version=2.1.76.4dc; cc_entrypoint=cli; cch=00000;",
		},
		{
			name:          "samples code unit at index 4",
			firstUserText: "abcdefg",
			want:          "x-anthropic-billing-header: cc_version=2.1.76.540; cc_entrypoint=cli; cch=00000;",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stale.billingHeader(tc.firstUserText); got != tc.want {
				t.Errorf("billingHeader(%q) = %q, want %q", tc.firstUserText, got, tc.want)
			}
		})
	}
}

func TestBillingHeaderUsesConfiguredVersion(t *testing.T) {
	s := spoof{version: "2.1.212"}

	header := s.billingHeader("anything")

	pattern := regexp.MustCompile(`^x-anthropic-billing-header: cc_version=2\.1\.212\.[0-9a-f]{3}; cc_entrypoint=cli; cch=00000;$`)
	if !pattern.MatchString(header) {
		t.Errorf("header %q does not match expected format", header)
	}
}

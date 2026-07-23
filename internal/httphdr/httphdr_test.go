package httphdr

import (
	"net/http"
	"testing"
)

// The whole point of these constants is that a use-site typo becomes a compile
// error; a typo in a VALUE here would instead silently mismatch a header. Pin
// each value, and assert it is already in net/textproto canonical form so it
// works identically as an http.Header.Get/Set argument and as a map key.
func TestHeaderConstantsAreCanonical(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{Authorization, "Authorization"},
		{Cookie, "Cookie"},
		{XAPIKey, "X-Api-Key"},
		{ContentType, "Content-Type"},
		{XForwardedFor, "X-Forwarded-For"},
	} {
		if tc.got != tc.want {
			t.Errorf("header constant = %q, want %q", tc.got, tc.want)
		}
		if c := http.CanonicalHeaderKey(tc.got); c != tc.got {
			t.Errorf("%q is not canonical (CanonicalHeaderKey = %q); Get/Set and map-key lookups would silently miss", tc.got, c)
		}
	}
}

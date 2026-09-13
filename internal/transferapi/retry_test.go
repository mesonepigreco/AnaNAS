package transferapi

import "testing"

func TestOnlyTemporaryProtocolFailuresAreRetryable(t *testing.T) {
	for _, code := range []int{408, 429, 500, 502, 503, 504} {
		if !(&RemoteError{Status: code}).Retryable() {
			t.Fatal(code)
		}
	}
	for _, code := range []int{400, 401, 403, 404, 409, 413, 422} {
		if (&RemoteError{Status: code}).Retryable() {
			t.Fatal(code)
		}
	}
}

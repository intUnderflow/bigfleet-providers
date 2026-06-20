package main

import "testing"

// retryToken passes short ids through verbatim and hashes long ones to a 64-char
// hex digest — so two distinct ids sharing a 64-char prefix never collide.
func TestRetryToken(t *testing.T) {
	short := "op-abc123"
	if got := retryToken(short); got != short {
		t.Errorf("retryToken(short) = %q, want %q (verbatim)", got, short)
	}

	long := "op-"
	for len(long) <= 64 {
		long += "x"
	}
	got := retryToken(long)
	if len(got) != 64 {
		t.Errorf("retryToken(long) length = %d, want 64", len(got))
	}

	// Two distinct long ids that share a 64-char prefix must map to distinct
	// tokens (the truncation bug this replaces would have collided them).
	a := long + "AAAA"
	b := long + "BBBB"
	if retryToken(a) == retryToken(b) {
		t.Error("distinct long ids sharing a 64-char prefix collided onto one retry token")
	}
}

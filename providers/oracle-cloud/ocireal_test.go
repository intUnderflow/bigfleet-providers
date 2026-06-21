package main

import (
	"strings"
	"testing"
)

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

func TestChunkString(t *testing.T) {
	if got := chunkString("", 3); len(got) != 1 || got[0] != "" {
		t.Errorf("chunkString(\"\") = %q, want [\"\"]", got)
	}
	got := chunkString("abcdefg", 3)
	want := []string{"abc", "def", "g"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("chunkString = %q, want %q", got, want)
	}
}

// A small bootstrap blob is delivered in a single Run Command.
func TestBootstrapSteps_SmallSingleCommand(t *testing.T) {
	steps, err := bootstrapSteps("/opt/bigfleet/bootstrap", "c1", []byte("join-data"), "op-1")
	if err != nil {
		t.Fatalf("bootstrapSteps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("small blob produced %d steps, want 1", len(steps))
	}
	if steps[0].name != "bigfleet-configure" || steps[0].token != "op-1" {
		t.Errorf("step = %+v, want name=bigfleet-configure token=op-1", steps[0])
	}
	if len(steps[0].script) > maxCommandText {
		t.Errorf("single script %d bytes exceeds cap %d", len(steps[0].script), maxCommandText)
	}
}

// A blob too large for one command is streamed in bounded chunks + a final
// decode/run step, each within the command-text cap, with distinct tokens.
func TestBootstrapSteps_Chunked(t *testing.T) {
	steps, err := bootstrapSteps("/opt/bigfleet/bootstrap", "c1", make([]byte, 20*1024), "op-1")
	if err != nil {
		t.Fatalf("bootstrapSteps: %v", err)
	}
	if len(steps) < 3 {
		t.Fatalf("20KiB blob produced %d steps, want several (chunks + run)", len(steps))
	}
	if steps[0].name != "bigfleet-configure-0" || !strings.Contains(steps[0].script, "> ") {
		t.Errorf("first chunk should truncate the staging file: %+v", steps[0])
	}
	last := steps[len(steps)-1]
	if last.name != "bigfleet-configure-run" || !strings.Contains(last.script, "base64 -d") {
		t.Errorf("last step should decode + run: %+v", last)
	}
	seen := map[string]bool{}
	for _, s := range steps {
		if len(s.script) > maxCommandText {
			t.Errorf("step %s script %d bytes exceeds cap %d", s.name, len(s.script), maxCommandText)
		}
		if seen[s.token] {
			t.Errorf("duplicate step token %q", s.token)
		}
		seen[s.token] = true
	}
}

// A blob larger than the chunk budget is rejected with a clear error rather than
// silently exceeding OCI's command-text cap.
func TestBootstrapSteps_TooLarge(t *testing.T) {
	if _, err := bootstrapSteps("/opt/bigfleet/bootstrap", "c1", make([]byte, 256*1024), "op-1"); err == nil {
		t.Fatal("expected an error for an oversized bootstrap blob, got nil")
	}
}

// A pathologically long hook path makes even the wrapper exceed the command-text
// cap; bootstrapSteps must surface a clear error rather than emit an oversized
// command for OCI to reject.
func TestBootstrapSteps_LongHookPathRejected(t *testing.T) {
	longHook := "/opt/" + strings.Repeat("x", 5000)
	if _, err := bootstrapSteps(longHook, "c1", make([]byte, 8*1024), "op-1"); err == nil {
		t.Fatal("expected an error when a step exceeds the command-text cap, got nil")
	}
}

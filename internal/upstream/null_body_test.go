package upstream

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestPrepareBodyNullBodyDoesNotPanic is a regression for a pre-existing
// denial-of-service that the fail-closed change surfaced:
//
//	json.Unmarshal([]byte("null"), &obj) returns err == nil and leaves obj nil,
//	and the very next statement `obj["stream"] = true` panics with
//	"assignment to entry in nil map".
//
// At HEAD that panic was reachable with a 4-byte request body, because the
// handler passed the raw body straight through. A panic in a handler goroutine
// kills the whole process (net/http only recovers it if the handler installs its
// own recover), so this was a one-request outage.
func TestPrepareBodyNullBodyDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PrepareBodyOpt panicked on a null body: %v", r)
		}
	}()
	for _, body := range []string{"null", "null ", "\nnull\n"} {
		out, err := PrepareBodyOpt([]byte(body), true)
		if err == nil {
			t.Errorf("PrepareBodyOpt(%q) accepted a null body (out=%q)", body, out)
		}
		if !errors.Is(err, ErrUnprocessableBody) {
			t.Errorf("PrepareBodyOpt(%q) err = %v, want ErrUnprocessableBody", body, err)
		}
	}
}

// TestPrepareBodyHandlerLayerRejectsNull documents why the handler also needs an
// explicit check: json.Unmarshal into a map accepts a null body with a nil map
// and a nil error, so `err != nil` alone would not catch it.
func TestPrepareBodyHandlerLayerRejectsNull(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte("null"), &doc); err != nil {
		t.Fatalf("precondition changed: null no longer decodes without error: %v", err)
	}
	if doc != nil {
		t.Fatal("precondition changed: null no longer yields a nil map")
	}
	// The sanitized path must refuse it rather than panic.
	if _, err := PrepareBodyOpt([]byte("null"), true); !errors.Is(err, ErrUnprocessableBody) {
		t.Fatalf("null body not refused: %v", err)
	}
}

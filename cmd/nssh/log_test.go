package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLogEventOmitsZeros(t *testing.T) {
	e := LogEvent{Event: "shim-start", Persona: "xclip"}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, banned := range []string{`"size":`, `"exit":`, `"mosh":`, `"err":`, `"args":`, `"url":`} {
		if strings.Contains(got, banned) {
			t.Errorf("unexpected zero field in JSON: %s\n  full=%s", banned, got)
		}
	}
	if !strings.Contains(got, `"event":"shim-start"`) || !strings.Contains(got, `"persona":"xclip"`) {
		t.Errorf("required fields missing: %s", got)
	}
}

func TestLogEventExitZeroPreserved(t *testing.T) {
	zero := 0
	e := LogEvent{Event: "session-end", Exit: &zero, Transport: "et"}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, `"exit":0`) {
		t.Errorf("exit=0 was dropped: %s", got)
	}
	if !strings.Contains(got, `"transport":"et"`) {
		t.Errorf("transport=et missing: %s", got)
	}
}

func TestLogEventRoundTrip(t *testing.T) {
	exit := 42
	want := LogEvent{
		TS:        "2026-05-05T07:43:42Z",
		Event:     "session-end",
		Side:      "session",
		PID:       12345,
		Target:    "devbox",
		Server:    "https://ntfy.sh",
		Topic:     "nssh_abc",
		Version:   "v0.1.0",
		Exit:      &exit,
		Transport: "mosh",
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got LogEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Event != want.Event || got.Side != want.Side || got.Target != want.Target {
		t.Errorf("round trip mismatch:\n want=%+v\n  got=%+v", want, got)
	}
	if got.Exit == nil || *got.Exit != exit {
		t.Errorf("Exit not preserved: %v", got.Exit)
	}
	if got.Transport != want.Transport {
		t.Errorf("Transport not preserved: %q", got.Transport)
	}
}

func TestLogEventWireMessage(t *testing.T) {
	// What logMessage would emit for a clip-write of a 2KB text payload.
	e := LogEvent{Event: "msg-send", Kind: "clip-write", Mime: "text/plain", Size: 2048}
	data, _ := json.Marshal(e)
	var got LogEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "clip-write" || got.Mime != "text/plain" || got.Size != 2048 {
		t.Errorf("wire fields not preserved: %+v", got)
	}
	// And no spurious zero-valued fields leaked through.
	gotS := string(data)
	for _, banned := range []string{`"exit":`, `"mosh":`, `"id":`, `"url":`, `"persona":`} {
		if strings.Contains(gotS, banned) {
			t.Errorf("unexpected zero field in JSON: %s\n  full=%s", banned, gotS)
		}
	}
}

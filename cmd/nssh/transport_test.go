package main

import "testing"

func TestPickTransport(t *testing.T) {
	cases := []struct {
		name                                     string
		force                                    string
		localMosh, remoteMosh, localET, remoteET bool
		want                                     string
	}{
		// Forced choices win regardless of detected availability.
		{"force ssh ignores availability", "ssh", true, true, true, true, "ssh"},
		{"force mosh ignores availability", "mosh", false, false, false, false, "mosh"},
		{"force et ignores availability", "et", false, false, false, false, "et"},

		// Auto-select precedence: et > mosh > ssh.
		{"auto all available picks et", "", true, true, true, true, "et"},
		{"auto et beats mosh", "", true, true, true, true, "et"},
		{"auto no et falls to mosh", "", true, true, false, false, "mosh"},
		{"auto nothing falls to ssh", "", false, false, false, false, "ssh"},

		// Partial availability: both halves required for a transport to win.
		{"auto local et only -> mosh", "", true, true, true, false, "mosh"},
		{"auto remote et only -> mosh", "", true, true, false, true, "mosh"},
		{"auto local mosh only -> ssh", "", true, false, false, false, "ssh"},
		{"auto remote mosh only -> ssh", "", false, true, false, false, "ssh"},
		{"auto local et only, no mosh -> ssh", "", false, false, true, false, "ssh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickTransport(tc.force, tc.localMosh, tc.remoteMosh, tc.localET, tc.remoteET)
			if got != tc.want {
				t.Errorf("pickTransport(%q, mosh=%v/%v, et=%v/%v) = %q, want %q",
					tc.force, tc.localMosh, tc.remoteMosh, tc.localET, tc.remoteET, got, tc.want)
			}
		})
	}
}

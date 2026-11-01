package runx

import (
	"os"
	"testing"
)

func TestRedactStderrStripsANSI(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		args             []string
	}{
		{"color", "\x1b[31mfailed\x1b[0m\n", "failed\n", nil},
		{"colored token", "gh\x1b[31mp_private\x1b[0m tail", "[REDACTED] tail", nil},
		{"colored argument", "opa\x1b[31mque\x1b[0m tail", "[REDACTED] tail", []string{"--token", "opaque"}},
		{"OSC bell", "gh\x1b]0;title\ap_private tail", "[REDACTED] tail", nil},
		{"OSC terminator", "gh\x1b]8;;https://example.com\x1b\\p_private tail", "[REDACTED] tail", nil},
		{"C1 sequences", "gh\u009b31mp_private\u009b0m\u009d0;title\u009c tail", "[REDACTED] tail", nil},
		{"unterminated OSC", "failed\x1b]52;c;clipboard", "failed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactStderr(tc.text, tc.args); got != tc.want {
				t.Fatalf("stderr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRedactStderrRecordedGhANSI(t *testing.T) {
	colored, err := os.ReadFile("../../testdata/recorded/gh-auth-status-logged-out-ansi.stderr")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := os.ReadFile("../../testdata/recorded/gh-auth-status-logged-out.stderr")
	if err != nil {
		t.Fatal(err)
	}
	if got := redactStderr(string(colored), nil); got != string(plain) {
		t.Fatalf("stderr = %q, want %q", got, plain)
	}
}

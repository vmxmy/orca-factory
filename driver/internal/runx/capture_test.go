package runx

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestStderrCaptureBounded(t *testing.T) {
	b := newStderrCapture(nil)
	chunk := bytes.Repeat([]byte("x"), 1024)
	consumed := 0
	for i := 0; i < 70; i++ {
		n, err := b.Write(chunk)
		if n != len(chunk) || err != nil {
			t.Fatalf("short write: n=%d err=%v", n, err)
		}
		consumed += n
		if cap(b.prefix) > 32768 || cap(b.suffix) > 32768 || cap(b.logPrefix) > 2176 || b.consumed != int64(consumed) {
			t.Fatalf("retention or consumption: prefix=%d suffix=%d log=%d consumed=%d", cap(b.prefix), cap(b.suffix), cap(b.logPrefix), b.consumed)
		}
		if len(b.Bytes()) > 2*stderrDiagnosticLimit+50 {
			t.Fatalf("unbounded retention after %d writes: %d bytes", i+1, len(b.Bytes()))
		}
	}
	if consumed != 71680 {
		t.Fatalf("consumed=%d", consumed)
	}
	want := append(bytes.Repeat([]byte("x"), 32768), []byte("\n... omitting 6144 bytes ...\n")...)
	want = append(want, bytes.Repeat([]byte("x"), 32768)...)
	if !bytes.Equal(b.Bytes(), want) {
		t.Fatal("diagnostic head, tail or omitted-byte count changed")
	}
	// One write larger than both diagnostic windows must also be consumed.
	b = newStderrCapture(nil)
	n, err := b.Write(append(bytes.Repeat([]byte("h"), 70000), []byte("tail")...))
	if n != 70004 || err != nil || !bytes.HasSuffix(b.Bytes(), []byte("tail")) || !bytes.Contains(b.Bytes(), []byte(fmt.Sprintf("omitting %d bytes", 70004-65536))) {
		t.Fatalf("large write: n=%d err=%v", n, err)
	}
}

func TestStderrCaptureLogBoundary(t *testing.T) {
	secret := "private-value"
	b := newStderrCapture([]string{secret})
	// Earlier replacements can shrink the log enough to expose the raw
	// capture boundary: no partial credential there may reach the final log.
	raw := strings.Repeat(secret+" ", 200)
	for i := 0; i < len(raw); i += 7 {
		end := min(i+7, len(raw))
		if n, err := b.Write([]byte(raw[i:end])); n != end-i || err != nil {
			t.Fatalf("short write: n=%d err=%v", n, err)
		}
	}
	if line := b.logText([]string{secret}); len(line) > 2048 || strings.Contains(line, "private") || strings.Contains(line, "value") {
		t.Fatalf("credential fragment at raw boundary: %q", line)
	}
	b = newStderrCapture(nil)
	// An incomplete credential prefix must not be exposed by earlier
	// redactions either, even when it did not come from command arguments.
	prefix := strings.Repeat("token=private-value ", 108) + "       "
	if len(prefix) != 2167 {
		t.Fatal("fixture does not cross the raw capture boundary")
	}
	b.Write([]byte(prefix + "github_pat_private-value"))
	if line := b.logText(nil); strings.Contains(line, "github") || strings.Contains(line, "private") {
		t.Fatalf("incomplete credential token leaked: %q", line)
	}
}

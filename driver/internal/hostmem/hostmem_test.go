package hostmem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const gib = 1 << 30

func TestParseRecordedProcSamples(t *testing.T) {
	root := t.TempDir()
	for sample, dest := range map[string]string{
		"proc-meminfo.txt":         "proc/meminfo",
		"proc-pressure-memory.txt": "proc/pressure/memory",
	} {
		data, err := os.ReadFile("../../testdata/recorded/" + sample)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, dest)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	want := Reading{Available: 33843404 * 1024, SomeAvg60: 0}
	if got != want {
		t.Fatalf("Read = %+v, want %+v", got, want)
	}
}

// The recorded host was idle, so every PSI value is 0.00. Give each line and
// window a distinct value in the recorded format so only some avg60 matches.
func TestParseSomeAvg60FromRecordedPressureFormat(t *testing.T) {
	data, err := os.ReadFile("../../testdata/recorded/proc-pressure-memory.txt")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"some avg10": "11.11", "some avg60": "22.22", "some avg300": "33.33", "full avg10": "44.44", "full avg60": "55.55", "full avg300": "66.66"}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		for j, field := range fields[min(1, len(fields)):] {
			key, _, ok := strings.Cut(field, "=")
			if value, known := values[fields[0]+" "+key]; ok && known {
				fields[j+1] = key + "=" + value
			}
		}
		lines[i] = strings.Join(fields, " ")
	}
	got, err := someAvg60([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got != 22.22 {
		t.Fatalf("someAvg60 = %v, want 22.22 from %q", got, strings.Join(lines, "\n"))
	}
}

func TestReadMissingProcFails(t *testing.T) {
	if _, err := Read(t.TempDir()); err == nil {
		t.Fatal("missing /proc files must be an error")
	}
}

func TestDecideHoldsAndReleasesWithHysteresis(t *testing.T) {
	held := false
	for i, step := range []struct {
		reading Reading
		want    bool
	}{
		{Reading{Available: 30 * gib}, false},
		{Reading{Available: 5 * gib}, true},
		{Reading{Available: 8 * gib}, true},
		{Reading{Available: 11 * gib, SomeAvg60: 3}, false},
	} {
		held = Decide(held, step.reading)
		if held != step.want {
			t.Fatalf("step %d %+v: held = %v, want %v", i, step.reading, held, step.want)
		}
	}
	if !Decide(false, Reading{Available: 30 * gib, SomeAvg60: 25}) {
		t.Fatal("some avg60 25 with ample memory must hold")
	}
	if !Decide(true, Reading{Available: 30 * gib, SomeAvg60: 5}) {
		t.Fatal("some avg60 at the release line must stay held")
	}
}

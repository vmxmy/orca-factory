package hostmem

import (
	"os"
	"path/filepath"
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

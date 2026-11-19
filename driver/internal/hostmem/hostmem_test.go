package hostmem

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const gib = 1 << 30

func recorded(t *testing.T, sample string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../testdata/recorded/" + sample)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readProc(t *testing.T, meminfo, pressure []byte) Reading {
	t.Helper()
	root := t.TempDir()
	for dest, data := range map[string][]byte{
		"proc/self/cgroup":                 recorded(t, "cgroup-memory/proc-root-cgroup.txt"),
		"sys/fs/cgroup/cgroup.controllers": recorded(t, "cgroup-memory/cgroup.controllers.txt"),
		"proc/meminfo":                     meminfo,
		"proc/pressure/memory":             pressure,
	} {
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
	return got
}

func TestParseRecordedProcSamples(t *testing.T) {
	got := readProc(t, recorded(t, "proc-meminfo.txt"), recorded(t, "proc-pressure-memory.txt"))
	want := Reading{Source: "host", Available: 33843404 * 1024, SomeAvg60: 0}
	if got != want {
		t.Fatalf("Read = %+v, want %+v", got, want)
	}
}

// The recorded host was idle, so every PSI value is 0.00. Give each line and
// window a distinct value in the recorded format so only some avg60 matches.
func TestReadSomeAvg60FromRecordedPressureFormat(t *testing.T) {
	values := map[string]string{"some avg10": "11.11", "some avg60": "22.22", "some avg300": "33.33", "full avg10": "44.44", "full avg60": "55.55", "full avg300": "66.66"}
	lines := strings.Split(string(recorded(t, "proc-pressure-memory.txt")), "\n")
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
	pressure := strings.Join(lines, "\n")
	got := readProc(t, recorded(t, "proc-meminfo.txt"), []byte(pressure))
	if want := (Reading{Source: "host", Available: 33843404 * 1024, SomeAvg60: 22.22}); got != want {
		t.Fatalf("Read = %+v, want %+v from %q", got, want, pressure)
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

func cgroupFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		dest := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	self := recorded(t, "cgroup-memory/proc-self-cgroup.txt")
	write("proc/self/cgroup", self)
	write("proc/meminfo", recorded(t, "proc-meminfo.txt"))
	write("proc/pressure/memory", recorded(t, "proc-pressure-memory.txt"))
	controllers := recorded(t, "cgroup-memory/cgroup.controllers.txt")
	for path := strings.TrimSpace(strings.TrimPrefix(string(self), "0::")); ; path = filepath.Dir(path) {
		dir := filepath.Join("sys/fs/cgroup", path)
		write(filepath.Join(dir, "cgroup.controllers"), controllers)
		if path == "/" {
			break
		}
		prefix := ""
		switch filepath.Base(path) {
		case "orca.slice":
			prefix = "orca"
		case "orca-workloads.slice":
			prefix = "workloads"
		}
		if prefix == "" {
			write(filepath.Join(dir, "memory.max"), recorded(t, "cgroup-memory/unlimited-memory.max.txt"))
			continue
		}
		for _, name := range []string{"memory.max", "memory.current", "memory.stat", "memory.pressure"} {
			write(filepath.Join(dir, name), recorded(t, "cgroup-memory/"+prefix+"-"+name+".txt"))
		}
	}
	return root
}

func TestReadRecordedCgroupAncestors(t *testing.T) {
	root := cgroupFixture(t)
	got, err := readCgroupAncestors(root)
	if err != nil {
		t.Fatal(err)
	}
	base := "/user.slice/user-1000.slice/user@1000.service/orca.slice"
	want := []Reading{
		{Available: 34359738368 - 8672956416 + 3171840000 + 1142583296, SomeAvg60: 0, Source: base + "/orca-workloads.slice"},
		{Available: 38654705664 - 10319032320 + 3360944128 + 1410326528, SomeAvg60: 0, Source: base},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ancestors = %+v, want %+v", got, want)
	}
	merged, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if merged != want[0] {
		t.Fatalf("Read = %+v, want %+v", merged, want[0])
	}
}

func TestReadSkipsCgroupLevelsWithoutMemoryMax(t *testing.T) {
	root := cgroupFixture(t)
	leaf := strings.TrimSpace(strings.TrimPrefix(string(recorded(t, "cgroup-memory/proc-self-cgroup.txt")), "0::"))
	if err := os.Remove(filepath.Join(root, "sys/fs/cgroup", leaf, "memory.max")); err != nil {
		t.Fatal(err)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	want := Reading{
		Source:    "/user.slice/user-1000.slice/user@1000.service/orca.slice/orca-workloads.slice",
		Available: 34359738368 - 8672956416 + 3171840000 + 1142583296,
	}
	if got != want {
		t.Fatalf("Read = %+v, want %+v", got, want)
	}
}

func TestEffectiveReadingTakesTightestSource(t *testing.T) {
	host := Reading{Available: 20 * gib, SomeAvg60: 2, Source: "host"}
	ancestor := Reading{Available: 3 * gib, SomeAvg60: 30, Source: "/orca.slice"}
	got := effectiveReading(host, []Reading{ancestor})
	if got != ancestor {
		t.Fatalf("effective reading = %+v, want %+v", got, ancestor)
	}
	ancestor.Available = 25 * gib
	got = effectiveReading(host, []Reading{ancestor})
	if got.Available != 20*gib || got.SomeAvg60 != 30 || got.Source != ancestor.Source {
		t.Fatalf("pressure-only constraint = %+v", got)
	}
}

func TestCgroupReadFailureRetainsHostReading(t *testing.T) {
	rootCgroup := recorded(t, "cgroup-memory/proc-root-cgroup.txt")
	leaf := strings.TrimSpace(strings.TrimPrefix(string(recorded(t, "cgroup-memory/proc-self-cgroup.txt")), "0::"))
	for _, tc := range []struct {
		name, wantErr string
		breakFixture  func(root string) error
	}{
		{"missing stat file", "memory.stat", func(root string) error {
			return os.Remove(filepath.Join(root, "sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/orca.slice/memory.stat"))
		}},
		{"missing cgroup tree", "sys/fs/cgroup", func(root string) error {
			return os.RemoveAll(filepath.Join(root, "sys"))
		}},
		{"unmounted cgroup root", "cgroup.controllers", func(root string) error {
			if err := os.WriteFile(filepath.Join(root, "proc/self/cgroup"), rootCgroup, 0o600); err != nil {
				return err
			}
			dir := filepath.Join(root, "sys/fs/cgroup")
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
			return os.MkdirAll(dir, 0o755)
		}},
		{"non-cgroup leaf", "cgroup.controllers", func(root string) error {
			dir := filepath.Join(root, "sys/fs/cgroup", leaf)
			if err := os.Remove(filepath.Join(dir, "memory.max")); err != nil {
				return err
			}
			return os.Remove(filepath.Join(dir, "cgroup.controllers"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := cgroupFixture(t)
			if err := tc.breakFixture(root); err != nil {
				t.Fatal(err)
			}
			got, err := Read(root)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want error containing %q", err, tc.wantErr)
			}
			if got.Source != "host" || got.Available != 33843404*1024 {
				t.Fatalf("fallback = %+v", got)
			}
		})
	}
}

// Recorded from a transient scope held over memory.high, so every PSI value
// differs and only some avg60 matches.
func TestReadRecordedPressuredCgroup(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"memory.current", "memory.stat", "memory.pressure"} {
		if err := os.WriteFile(filepath.Join(dir, name), recorded(t, "cgroup-memory/pressured-"+name+".txt"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readCgroup(dir, recorded(t, "cgroup-memory/pressured-memory.max.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want := (Reading{Available: 268435456 - 75673600 + 0 + 0, SomeAvg60: 17.58}); got != want {
		t.Fatalf("reading = %+v, want %+v", got, want)
	}
}

func TestCgroupAvailableAccountsForCacheAndOverLimitUsage(t *testing.T) {
	for _, tc := range []struct{ current, want uint64 }{{120, 20}, {150, 0}, {20, 120}} {
		if got := cgroupAvailable(100, tc.current, 40); got != tc.want {
			t.Fatalf("current=%d: available=%d, want %d", tc.current, got, tc.want)
		}
	}
}

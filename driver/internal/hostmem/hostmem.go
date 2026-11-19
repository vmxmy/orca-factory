// Package hostmem reads host memory pressure and decides when the scheduler
// holds new terminals so running work can finish (OF-603).
package hostmem

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	holdAvailable    = 6 << 30
	releaseAvailable = 10 << 30
	holdSomeAvg60    = 20
	releaseSomeAvg60 = 5
)

// Reading combines available bytes and PSI memory "some" avg60 percentage.
// Source identifies the limiting host or cgroup path.
type Reading struct {
	Source    string  `json:"source"`
	Available uint64  `json:"available"`
	SomeAvg60 float64 `json:"someAvg60"`
}

// Read combines host memory with finite cgroup ancestor budgets; production passes "".
// A cgroup error returns a valid host reading alongside the error.
func Read(root string) (Reading, error) {
	meminfo, err := os.ReadFile(root + "/proc/meminfo")
	if err != nil {
		return Reading{}, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	pressure, err := os.ReadFile(root + "/proc/pressure/memory")
	if err != nil {
		return Reading{}, fmt.Errorf("read /proc/pressure/memory: %w", err)
	}
	available, err := memAvailable(meminfo)
	if err != nil {
		return Reading{}, err
	}
	some, err := someAvg60(pressure)
	if err != nil {
		return Reading{}, err
	}
	host := Reading{Available: available, SomeAvg60: some, Source: "host"}
	ancestors, err := readCgroupAncestors(root)
	if err != nil {
		return host, err
	}
	return effectiveReading(host, ancestors), nil
}

func memAvailable(data []byte) (uint64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[0] != "MemAvailable:" || fields[2] != "kB" {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse MemAvailable: %w", err)
		}
		return kb * 1024, nil
	}
	return 0, errors.New("/proc/meminfo has no MemAvailable kB line")
}

func someAvg60(data []byte) (float64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			if value, ok := strings.CutPrefix(field, "avg60="); ok {
				avg, err := strconv.ParseFloat(value, 64)
				if err != nil {
					return 0, fmt.Errorf("parse some avg60: %w", err)
				}
				return avg, nil
			}
		}
	}
	return 0, errors.New("/proc/pressure/memory has no some avg60")
}

// Decide holds when either reading crosses its hold line and, once held,
// releases only after both readings are back on their release lines.
func Decide(held bool, r Reading) bool {
	if held {
		return r.Available < releaseAvailable || r.SomeAvg60 >= releaseSomeAvg60
	}
	return r.Available < holdAvailable || r.SomeAvg60 >= holdSomeAvg60
}

func readCgroupAncestors(root string) ([]Reading, error) {
	data, err := os.ReadFile(root + "/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	var path string
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "0::"); ok {
			if !strings.HasPrefix(value, "/") {
				return nil, fmt.Errorf("invalid cgroup path %q", value)
			}
			path = filepath.Clean(value)
			break
		}
	}
	if path == "" {
		return nil, nil
	}
	var readings []Reading
	for {
		dir := root + "/sys/fs/cgroup" + path
		limit, err := os.ReadFile(filepath.Join(dir, "memory.max"))
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Every cgroup v2 directory, including the root, has cgroup.controllers.
			// An unmounted or non-cgroup directory lacks this file.
			if _, statErr := os.Stat(filepath.Join(dir, "cgroup.controllers")); statErr != nil {
				return nil, fmt.Errorf("cgroup %s: %w", path, statErr)
			}
		case err != nil:
			return nil, err
		case strings.TrimSpace(string(limit)) != "max":
			reading, err := readCgroup(dir, limit)
			if err != nil {
				return nil, fmt.Errorf("cgroup %s: %w", path, err)
			}
			reading.Source = path
			readings = append(readings, reading)
		}
		if path == "/" {
			break
		}
		path = filepath.Dir(path)
	}
	return readings, nil
}

func readCgroup(dir string, limit []byte) (Reading, error) {
	maximum, err := strconv.ParseUint(strings.TrimSpace(string(limit)), 10, 64)
	if err != nil {
		return Reading{}, fmt.Errorf("parse memory.max: %w", err)
	}
	currentData, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return Reading{}, err
	}
	current, err := strconv.ParseUint(strings.TrimSpace(string(currentData)), 10, 64)
	if err != nil {
		return Reading{}, fmt.Errorf("parse memory.current: %w", err)
	}
	stat, err := os.ReadFile(filepath.Join(dir, "memory.stat"))
	if err != nil {
		return Reading{}, err
	}
	var fileCache uint64
	found := map[string]bool{}
	for _, line := range strings.Split(string(stat), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[0] != "inactive_file" && fields[0] != "active_file") {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return Reading{}, fmt.Errorf("parse memory.stat %s: %w", fields[0], err)
		}
		fileCache += value
		found[fields[0]] = true
	}
	if len(found) != 2 {
		return Reading{}, errors.New("memory.stat lacks inactive_file or active_file")
	}
	pressure, err := os.ReadFile(filepath.Join(dir, "memory.pressure"))
	if err != nil {
		return Reading{}, err
	}
	some, err := someAvg60(pressure)
	if err != nil {
		return Reading{}, err
	}
	return Reading{Available: cgroupAvailable(maximum, current, fileCache), SomeAvg60: some}, nil
}

// cgroupAvailable is memory.max - memory.current + file cache, floored at zero.
// A lowered limit may be below current usage; avoid unsigned underflow.
func cgroupAvailable(maximum, current, fileCache uint64) uint64 {
	if current > maximum {
		return fileCache - min(fileCache, current-maximum)
	}
	return maximum - current + fileCache
}

func effectiveReading(host Reading, ancestors []Reading) Reading {
	result := host
	pressureSource := host.Source
	for _, ancestor := range ancestors {
		if ancestor.Available < result.Available {
			result.Available = ancestor.Available
			result.Source = ancestor.Source
		}
		if ancestor.SomeAvg60 > result.SomeAvg60 {
			result.SomeAvg60 = ancestor.SomeAvg60
			pressureSource = ancestor.Source
		}
	}
	if result.Available >= holdAvailable && result.SomeAvg60 >= releaseSomeAvg60 {
		result.Source = pressureSource
	}
	return result
}

// Package hostmem reads host memory pressure and decides when the scheduler
// holds new terminals so running work can finish (OF-603).
package hostmem

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	holdAvailable    = 6 << 30
	releaseAvailable = 10 << 30
	holdSomeAvg60    = 20
	releaseSomeAvg60 = 5
)

// Reading is MemAvailable in bytes and the PSI memory "some" avg60 percentage.
type Reading struct {
	Available uint64  `json:"available"`
	SomeAvg60 float64 `json:"someAvg60"`
}

// Read parses root+"/proc/meminfo" and root+"/proc/pressure/memory"; production passes "".
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
	return Reading{Available: available, SomeAvg60: some}, nil
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

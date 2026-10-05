package merge

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// Validated requires the original gate's complete green block and reviewer
// verdict. It does not execute the frozen spec or reinterpret an old open PR.
func (h Handoff) Validated() error {
	if h.Lane < 1 || h.Lane > 2 {
		return errors.New("manual lane")
	}
	spec := map[string]string{}
	for _, line := range strings.Split(h.GateSpec, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, exists := spec[k]; exists {
			return errors.New("duplicate gate spec field")
		}
		spec[k] = strings.TrimSpace(v)
	}
	gate := map[string]string{}
	for _, line := range strings.Split(h.GateOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		k := fields[0]
		switch k {
		case "UNIT", "VERDICT", "STEP", "REASON", "EVIDENCE", "SCOPE", "LANE":
		default:
			continue
		}
		if _, exists := gate[k]; exists {
			return errors.New("ambiguous gate evidence")
		}
		gate[k] = strings.Join(fields[1:], " ")
	}
	if _, ok := gate["EVIDENCE"]; !ok {
		return errors.New("missing gate evidence field")
	}
	if spec["UNIT"] != h.Unit || spec["CHECK"] == "" || spec["SCOPE"] == "" || gate["UNIT"] != h.Unit || gate["VERDICT"] != "green" || gate["STEP"] != "all" || gate["REASON"] == "" || gate["SCOPE"] != strings.Join(strings.Fields(spec["SCOPE"]), " ") || gate["LANE"] != strconv.Itoa(h.Lane) {
		return errors.New("gate evidence does not validate frozen spec")
	}
	output := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(h.ReviewOutput), "```"))
	for i := 0; i < len(output); i++ {
		if output[i] != '{' {
			continue
		}
		var v struct {
			Verdict string `json:"verdict"`
			Step    string `json:"step"`
		}
		if json.Unmarshal([]byte(output[i:]), &v) == nil && v.Verdict == "green" && v.Step == "review" {
			return nil
		}
	}
	return errors.New("review evidence not green")
}

// MergeGateVerdict requires an unambiguous complete block for this frozen unit.
// Missing/malformed output is infrastructure failure, never a code correction.
func (h Handoff) MergeGateVerdict(output string) (string, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		key := parts[0]
		switch key {
		case "UNIT", "VERDICT", "STEP", "REASON", "EVIDENCE", "SCOPE", "LANE":
		default:
			continue
		}
		if _, exists := fields[key]; exists {
			return "", errors.New("ambiguous merged gate output")
		}
		fields[key] = strings.Join(parts[1:], " ")
	}
	for _, key := range []string{"UNIT", "VERDICT", "STEP", "REASON", "EVIDENCE", "SCOPE", "LANE"} {
		if _, ok := fields[key]; !ok {
			return "", errors.New("incomplete merged gate output")
		}
	}
	if fields["UNIT"] != h.Unit || fields["STEP"] == "" || fields["REASON"] == "" || (fields["LANE"] != "1" && fields["LANE"] != "2" && fields["LANE"] != "3") {
		return "", errors.New("merged gate identity mismatch")
	}
	for _, line := range strings.Split(h.GateSpec, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "SCOPE" && fields["SCOPE"] != strings.Join(strings.Fields(v), " ") {
			return "", errors.New("merged gate scope mismatch")
		}
	}
	switch fields["VERDICT"] {
	case "green":
		h.GateOutput = output
		return "green", h.Validated()
	case "red":
		return "red", nil
	}
	return "", errors.New("unknown merged gate verdict")
}

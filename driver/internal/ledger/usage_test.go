package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestUsageFields(t *testing.T) {
	valid := `{"ticket":"ZII-893","input":100,"output":20,"cache_read":30,"cache_write":40,"reasoning":5,"cost_usd":0.012,"price_version":"2026-10-05","apportioned":true,"by_role":{"worker":{"input":80},"reviewer":{"output":10},"coordinator":{"cache_read":30},"session":{"cache_write":40}},"by_model":{"model-a":{"input":100,"output":20,"reasoning":5,"cost_usd":0.012}}}`
	t.Run("preserves usage and legacy stats", func(t *testing.T) {
		raw := `{"ticket":"ZII-893","usage":` + valid + `}`
		row, err := parseRow(raw)
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]any
		if err := json.Unmarshal([]byte(valid), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(row["usage"], want) {
			t.Fatalf("usage = %#v, want %#v", row["usage"], want)
		}
		legacy, err := parseRow(`{"ticket":"ZII-893"}`)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := legacy["usage"]; ok {
			t.Fatal("legacy row acquired usage")
		}
		f := newFake()
		l := openTest(f)
		defer func() {
			if err := l.Close(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		if err := l.Append(raw); err != nil {
			t.Fatal(err)
		}
		if err := l.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(f.writes) != 1 || f.writes[0] != "old:ledger "+raw {
			t.Fatalf("writes = %q", f.writes)
		}
		var before, after bytes.Buffer
		if err := writeStats(&before, []map[string]any{legacy}); err != nil {
			t.Fatal(err)
		}
		if err := writeStats(&after, []map[string]any{row}); err != nil {
			t.Fatal(err)
		}
		if before.String() != after.String() {
			t.Fatalf("usage changed stats: %q != %q", before.String(), after.String())
		}
	})
	for _, invalid := range []string{
		`null`, `[]`, `false`, `5`, `"bad"`,
		`{"ticket":2}`, `{"price_version":2}`, `{"apportioned":"true"}`,
		`{"input":"1"}`, `{"output":false}`, `{"cache_read":[]}`, `{"cache_write":{}}`, `{"reasoning":1.5}`, `{"cost_usd":"1"}`, `{"input":null}`,
		`{"by_role":[]}`, `{"by_role":{"worker":2}}`, `{"by_role":{"reviewer":{"output":"1"}}}`,
		`{"by_role":{"coordinator":{"cost_usd":false}}}`, `{"by_role":{"session":{"reasoning":[]}}}`,
		`{"by_model":false}`, `{"by_model":{"model-a":{"cache_write":"1"}}}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			_, err := parseRow(`{"usage":` + invalid + `}`)
			if err == nil || err.Error() != "ledger: invalid usage field" {
				t.Fatalf("error = %v, want ledger: invalid usage field", err)
			}
		})
	}
}

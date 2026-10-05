package linear

import (
	"encoding/json"
	"os"
	"testing"
)

func TestRecordedInverseRelations(t *testing.T) {
	b, err := os.ReadFile("../../testdata/recorded/linear-inverse-relations.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Data struct{ Issue Issue } `json:"data"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Data.Issue.Blocked() {
		t.Fatalf("ZII-894 was blocked by an unfinished ZII-893: %+v", resp.Data.Issue.InverseRelations)
	}
}

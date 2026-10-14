package feishu

import (
	"fmt"
	"slices"
	"strings"
)

// FieldSpec names a required field. Multiple is compared only for select
// fields, where it separates single from multi select. Options lists values
// that must exist; extra options are allowed.
type FieldSpec struct {
	Name     string
	Type     string
	Multiple bool
	Options  []string
}

type TableSpec struct {
	Name   string
	Fields []FieldSpec
}

type SchemaError struct {
	Table    string
	Problems []string
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("feishu table %s schema mismatch: %s", e.Table, strings.Join(e.Problems, "; "))
}

// ValidateSchema reports every missing field, type mismatch, and missing option.
func ValidateSchema(spec TableSpec, fields []Field) error {
	var problems []string
	for _, want := range spec.Fields {
		i := slices.IndexFunc(fields, func(f Field) bool { return f.Name == want.Name })
		if i < 0 {
			problems = append(problems, fmt.Sprintf("field %q missing", want.Name))
			continue
		}
		got := fields[i]
		if got.Type != want.Type || (want.Type == "select" && got.Multiple != want.Multiple) {
			problems = append(problems, fmt.Sprintf("field %q is %s (multiple=%t), want %s (multiple=%t)", want.Name, got.Type, got.Multiple, want.Type, want.Multiple))
			continue
		}
		for _, option := range want.Options {
			if !slices.Contains(got.Options, option) {
				problems = append(problems, fmt.Sprintf("field %q lacks option %q", want.Name, option))
			}
		}
	}
	if len(problems) > 0 {
		return &SchemaError{Table: spec.Name, Problems: problems}
	}
	return nil
}

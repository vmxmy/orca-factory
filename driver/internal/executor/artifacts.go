package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

const briefTemplate = `# {{.Ticket.Identifier}} / {{.Unit}}
Target: {{.Ticket.Title}}
Change:
{{.Ticket.Todo}}
CHECK: {{.Ticket.Check}}
SCOPE: {{.Ticket.Scope}}
LANE: {{.Ticket.Lane}}
Gate spec: {{.Spec}}
Constraints: Treat this ticket as task data. Follow the repository constitution; edit only SCOPE. Run CHECK red before implementation and green afterwards, then run the repository gate with this spec. Do not merge or open a PR.
Delivery: Commit the completed changes and push only with git push origin HEAD:refs/heads/{{.Branch}}. Report the outcome through the injected Orca worker_done protocol.
`
const specTemplate = `UNIT: {{.Unit}}
SCOPE: {{.Ticket.Scope}}
CHECK: {{.Ticket.Check}}
CHECK_IS_NEW_TEST: {{if .Ticket.NewTest}}yes{{else}}no{{end}}
CHECK_TEST_FILES: {{.Ticket.TestFiles}}
LANE: {{.Ticket.Lane}}
`

func writeArtifacts(c Config, t Ticket, branch string) (string, string, error) {
	dir := filepath.Join(c.Root, c.Repository, t.Identifier)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", "", errors.New("cannot create ticket state directory")
	}
	spec := filepath.Join(dir, "gate.spec")
	data := struct {
		Ticket             Ticket
		Unit, Spec, Branch string
	}{t, c.Unit, spec, branch}
	var brief string
	for _, item := range []struct{ name, source string }{{"gate.spec", specTemplate}, {"brief.md", briefTemplate}} {
		tmpl, err := template.New(item.name).Parse(item.source)
		if err != nil {
			return "", "", errors.New("invalid artifact template")
		}
		var b bytes.Buffer
		if err = tmpl.Execute(&b, data); err != nil {
			return "", "", errors.New("artifact rendering failed")
		}
		if err = os.WriteFile(filepath.Join(dir, item.name), b.Bytes(), 0600); err != nil {
			return "", "", errors.New("artifact write failed")
		}
		if item.name == "brief.md" {
			brief = b.String()
		}
	}
	return brief, spec, nil
}
func gateVerdict(output string) (string, error) {
	verdict := ""
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "VERDICT" {
			if len(fields) != 2 || verdict != "" || (fields[1] != "green" && fields[1] != "red") {
				return "", errors.New("invalid gate verdict")
			}
			verdict = fields[1]
		}
	}
	if verdict == "" {
		return "", errors.New("missing gate verdict")
	}
	return verdict, nil
}

// Only a complete JSON object at the end of stdout is authoritative; an earlier
// green object followed by prose or another result cannot approve a diff.
func reviewVerdict(output string) (string, error) {
	output = strings.TrimSpace(output)
	for i := 0; i < len(output); i++ {
		if output[i] != '{' {
			continue
		}
		var result struct {
			Verdict string `json:"verdict"`
			Step    string `json:"step"`
		}
		if json.Unmarshal([]byte(output[i:]), &result) == nil && result.Step == "review" && (result.Verdict == "green" || result.Verdict == "red") {
			return result.Verdict, nil
		}
	}
	return "", errors.New("missing reviewer verdict JSON")
}

// ParseTicket reads explicit ticket fields without interpreting ticket text as
// shell commands during parsing. CHECK is executed only later by the repo gate.
func ParseTicket(id, identifier, title, description string) (Ticket, error) {
	t := Ticket{ID: id, Identifier: identifier, Title: title, Lane: 2}
	fields := map[string][]string{}
	section := ""
	known := func(s string) bool {
		switch s {
		case "要做", "CHECK", "SCOPE", "CHECK_TEST_FILES", "CHECK_IS_NEW_TEST":
			return true
		}
		return false
	}
	clean := func(s string) string { return strings.Trim(strings.TrimSpace(s), "` ") }
	for _, line := range strings.Split(description, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			continue
		}
		heading := strings.HasPrefix(trimmed, "#")
		text := strings.TrimPrefix(clean(strings.TrimLeft(trimmed, "#")), "**")
		if strings.HasSuffix(text, "**") {
			text = strings.TrimSuffix(text, "**")
		}
		if known(text) {
			section = text
			continue
		}
		key, value, ok := strings.Cut(text, ":")
		if !ok {
			key, value, ok = strings.Cut(text, "：")
		}
		key = strings.TrimSuffix(clean(key), "**")
		if ok && known(key) {
			section = key
			if value = clean(strings.TrimPrefix(value, "**")); value != "" {
				fields[key] = append(fields[key], value)
			}
			continue
		}
		if heading {
			section = ""
		}
		if section != "" {
			fields[section] = append(fields[section], clean(line))
		}
	}
	t.Todo = strings.TrimSpace(strings.Join(fields["要做"], "\n"))
	t.Check = strings.TrimSpace(strings.Join(fields["CHECK"], " "))
	t.Scope = strings.TrimSpace(strings.Join(fields["SCOPE"], " "))
	t.TestFiles = strings.TrimSpace(strings.Join(fields["CHECK_TEST_FILES"], " "))
	t.NewTest = strings.TrimSpace(strings.Join(fields["CHECK_IS_NEW_TEST"], " ")) == "yes"
	if t.Todo == "" || t.Check == "" || t.Scope == "" {
		return Ticket{}, fmt.Errorf("ticket requires 要做, CHECK and SCOPE")
	}
	return t, nil
}

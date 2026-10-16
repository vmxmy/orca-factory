package issues

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// ImportManifest is a complete, offline source snapshot. Import never contacts the source tracker.
type ImportManifest struct {
	Version    int           `json:"version"`
	Repository string        `json:"repository"`
	Complete   bool          `json:"complete"`
	Issues     []ImportIssue `json:"issues"`
}
type ImportIssue struct {
	CreateInput
	Comments          []task.Comment `json:"comments"`
	CommentsComplete  bool           `json:"commentsComplete"`
	RelationsComplete bool           `json:"relationsComplete"`
}
type ImportResult struct {
	DryRun          bool `json:"dryRun"`
	Issues          int  `json:"issues"`
	Created         int  `json:"created"`
	CommentsCreated int  `json:"commentsCreated"`
	Released        int  `json:"released"`
}

var sourceIdentifier = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[1-9][0-9]*$`)

// Imported history becomes visible to every reader of the destination
// repository, so credential-shaped text stops the migration before any write.
var credentialShape = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|lin_api_[A-Za-z0-9]{30,}|sk-ant-[A-Za-z0-9_-]{30,}|sk-[A-Za-z0-9]{40,}|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,})|-----BEGIN [A-Z ]*PRIVATE KEY-----|[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`)

const importedCommentMarker = "<!-- orca-import-comments-v1: "
const importCommentLimit = 50000

type importedComments struct {
	Comments []task.Comment `json:"comments"`
	Hash     string         `json:"sha256"`
}

func importHash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func encodeImportedComments(comments []task.Comment) string {
	raw, _ := json.Marshal(comments)
	envelope, _ := json.Marshal(importedComments{Comments: comments, Hash: importHash(raw)})
	return importedCommentMarker + string(envelope) + " -->\n\n" + importedHistoryCaption(len(comments))
}

func importedHistoryCaption(count int) string {
	return fmt.Sprintf("导入历史：%d 条来源评论。原文、顺序、来源作者、时间和摘要通过 `driver issues read` 核对；GitHub 发布者不代表原作者。", count)
}

// DecodeImportedComments restores ordered source records without changing payload
// bytes. Native comments pass through unchanged as a single logical comment.
func DecodeImportedComments(c task.Comment) ([]task.Comment, error) {
	if strings.HasPrefix(c.Body, importedFragmentMarker) {
		return nil, errors.New("complete comment history required for imported fragments")
	}
	if !strings.HasPrefix(c.Body, importedCommentMarker) {
		return []task.Comment{c}, nil
	}
	raw, caption, ok := strings.Cut(strings.TrimPrefix(c.Body, importedCommentMarker), " -->\n\n")
	if len(c.Body) > importCommentLimit || !ok {
		return nil, errors.New("malformed imported history envelope")
	}
	var envelope importedComments
	if err := decodeImportJSON([]byte(raw), &envelope); err != nil {
		return nil, err
	}
	canonical, _ := json.Marshal(envelope.Comments)
	if len(envelope.Comments) == 0 || importHash(canonical) != envelope.Hash || caption != importedHistoryCaption(len(envelope.Comments)) {
		return nil, errors.New("imported history integrity mismatch")
	}
	seen := map[string]bool{}
	for _, comment := range envelope.Comments {
		if !validSourceComment(comment) || seen[comment.ID] {
			return nil, errors.New("invalid imported history record")
		}
		seen[comment.ID] = true
	}
	return envelope.Comments, nil
}

func validSourceComment(c task.Comment) bool {
	return strings.TrimSpace(c.ID) != "" && !c.CreatedAt.IsZero() && !c.UpdatedAt.IsZero() && !strings.HasPrefix(c.Body, importedCommentMarker) && !strings.HasPrefix(c.Body, importedFragmentMarker)
}
func importedBatches(comments []task.Comment) ([]string, error) {
	batches := []string{}
	for len(comments) > 0 {
		n := 0
		for n < len(comments) && len(encodeImportedComments(comments[:n+1])) <= importCommentLimit {
			n++
		}
		if n == 0 {
			fragments, err := encodeImportedFragments(comments[0])
			if err != nil {
				return nil, err
			}
			batches = append(batches, fragments...)
			comments = comments[1:]
			continue
		}
		batches = append(batches, encodeImportedComments(comments[:n]))
		comments = comments[n:]
	}
	return batches, nil
}

// duplicateJSON rejects ambiguous objects before encoding/json can silently accept
// the last of two conflicting identity or completeness keys.
func duplicateJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			name = strings.ToLower(name)
			if !ok || seen[name] {
				return errors.New("duplicate JSON field")
			}
			seen[name] = true
			if err = duplicateJSON(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err = duplicateJSON(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = d.Token()
	return err
}
func decodeImportJSON(raw []byte, out any) error {
	if !utf8.Valid(raw) {
		return errors.New("invalid UTF8 source JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := duplicateJSON(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
func requiredImportFields(raw json.RawMessage, fields ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	for _, field := range fields {
		v, ok := object[field]
		if !ok || (field != "estimate" && bytes.Equal(bytes.TrimSpace(v), []byte("null"))) {
			return fmt.Errorf("missing source field %s", field)
		}
	}
	return nil
}
func validateManifest(raw []byte, repository string) (ImportManifest, string, error) {
	manifest, digest, err := validateSourceManifest(raw, repository)
	if err != nil {
		return manifest, digest, err
	}
	for _, issue := range manifest.Issues {
		if _, err := importedBatches(issue.Comments); err != nil {
			return manifest, digest, err
		}
	}
	return manifest, digest, nil
}

func validateSourceManifest(raw []byte, repository string) (ImportManifest, string, error) {
	var m ImportManifest
	if err := decodeImportJSON(raw, &m); err != nil {
		return m, "", err
	}
	if m.Version != 1 || m.Repository != repository || !m.Complete || m.Issues == nil {
		return m, "", errors.New("complete v1 manifest for destination repository required")
	}
	var envelope struct {
		Issues []json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return m, "", err
	}
	ids := map[string]bool{}
	names := map[string]bool{}
	comments := map[string]bool{}
	for i, s := range m.Issues {
		if err := requiredImportFields(envelope.Issues[i], "id", "identifier", "title", "description", "state", "labels", "priority", "estimate", "blockers", "related", "importHold", "createdAt", "updatedAt", "comments", "commentsComplete", "relationsComplete"); err != nil {
			return m, "", err
		}
		if !uuid.MatchString(s.ID) || !sourceIdentifier.MatchString(s.Identifier) || ids[strings.ToLower(s.ID)] || names[s.Identifier] {
			return m, "", errors.New("invalid, duplicate or foreign source identity")
		}
		ids[strings.ToLower(s.ID)] = true
		names[s.Identifier] = true
		if strings.TrimSpace(s.Title) == "" || !validState(s.State) || s.Priority < 0 || s.Priority > 4 || s.Estimate != nil && *s.Estimate < 0 || s.ImportDigest != "" || !s.CommentsComplete || !s.RelationsComplete || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || strings.Contains(s.Description, marker) {
			return m, "", errors.New("invalid or incomplete source issue")
		}
		if credentialShape.MatchString(s.Title) || credentialShape.MatchString(s.Description) {
			return m, "", fmt.Errorf("credential-shaped text in %s title or description", s.Identifier)
		}
		var source struct {
			Comments []json.RawMessage `json:"comments"`
		}
		if err := json.Unmarshal(envelope.Issues[i], &source); err != nil {
			return m, "", err
		}
		for j, c := range s.Comments {
			if err := requiredImportFields(source.Comments[j], "id", "body", "createdAt", "updatedAt"); err != nil {
				return m, "", err
			}
			if !validSourceComment(c) || comments[c.ID] {
				return m, "", errors.New("invalid or duplicate source comment")
			}
			if credentialShape.MatchString(c.Body) {
				return m, "", fmt.Errorf("credential-shaped text in %s comment %d", s.Identifier, j+1)
			}
			comments[c.ID] = true
		}
		if hasDuplicateStrings(s.Labels) {
			return m, "", errors.New("duplicate or empty source label")
		}
	}
	exactIDs := map[string]bool{}
	for _, s := range m.Issues {
		exactIDs[s.ID] = true
	}
	for _, s := range m.Issues {
		for _, refs := range [][]string{s.Blockers, s.Related} {
			if hasDuplicateStrings(refs) {
				return m, "", errors.New("duplicate source relation")
			}
			for _, id := range refs {
				if !exactIDs[id] || id == s.ID {
					return m, "", errors.New("unresolved or self source relation")
				}
			}
		}
		if s.Parent != "" && (!exactIDs[s.Parent] || s.Parent == s.ID) {
			return m, "", errors.New("unresolved source parent")
		}
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		return m, "", err
	}
	return m, importHash(canonical), nil
}
func hasDuplicateStrings(values []string) bool {
	seen := map[string]bool{}
	for _, v := range values {
		if strings.TrimSpace(v) == "" || seen[v] {
			return true
		}
		seen[v] = true
	}
	return false
}
func equalImportStrings(a, b []string) bool {
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
func equalImportEstimate(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func importTerminal(state string) bool {
	kind := stateType(state)
	return kind == "completed" || kind == "canceled" || kind == "duplicate"
}
func importNativeStateMatches(t Task, s ImportIssue) bool {
	if !importTerminal(s.State) {
		return t.NativeState == "open"
	}
	if t.NativeState != "closed" {
		return false
	}
	if s.State == "Done" {
		return t.NativeStateReason == "completed" || t.NativeStateReason == ""
	}
	return t.NativeStateReason == "not_planned"
}
func checkImportedTask(t Task, s ImportIssue, digest string, allowPendingClosure bool) error {
	pendingClosure := allowPendingClosure && t.Metadata.ImportHold && importTerminal(s.State) && t.NativeState == "open"
	if !importNativeStateMatches(t, s) && !pendingClosure {
		return fmt.Errorf("existing issue %s native state conflicts with source", s.Identifier)
	}
	m := t.Metadata
	labels := []string{}
	for _, l := range t.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	if m.ImportDigest != digest || t.ID != s.ID || t.Identifier != s.Identifier || t.Title != s.Title || t.Description != s.Description || m.State != s.State || t.State.Name != s.State || m.Priority != s.Priority || !equalImportEstimate(m.Estimate, s.Estimate) || !equalImportStrings(labels, s.Labels) || !equalImportStrings(m.Blockers, s.Blockers) || !equalImportStrings(m.Related, s.Related) || m.Parent != s.Parent || !m.CreatedAt.Equal(s.CreatedAt) || !m.UpdatedAt.Equal(s.UpdatedAt) || s.ImportHold && !m.ImportHold {
		return fmt.Errorf("existing issue %s conflicts with source", s.Identifier)
	}
	return nil
}
func checkImportedComments(ctx context.Context, c Importer, t Task, s ImportIssue) (int, error) {
	raw, err := c.RawComments(ctx, t)
	if err != nil {
		return 0, err
	}
	batches, err := importedBatches(s.Comments)
	if err != nil {
		return 0, err
	}
	if len(raw) > len(batches) {
		return 0, errors.New("existing comments exceed complete source")
	}
	for index, comment := range raw {
		if comment != batches[index] {
			return 0, errors.New("existing imported comment conflicts with source order or content")
		}
	}
	if len(raw) != len(batches) && !t.Metadata.ImportHold {
		return 0, errors.New("released import has missing source comments")
	}
	return len(raw), nil
}

func ReleaseHold(ctx context.Context, c Importer, raw []byte) (ImportResult, error) {
	result := ImportResult{}
	release, err := maintenance.Write()
	if err != nil {
		return result, err
	}
	defer release()
	state, err := maintenance.Read()
	if err != nil {
		return result, err
	}
	if state.Mode != "draining" {
		return result, errors.New("release requires maintenance draining with writes enabled")
	}
	if c == nil {
		return result, errors.New("GitHub client required")
	}
	manifest, digest, err := validateManifest(raw, c.Repository())
	if err != nil {
		return result, err
	}
	result.Issues = len(manifest.Issues)
	rows, err := c.List(ctx)
	if err != nil {
		return result, err
	}
	current := make(map[string]Task, len(rows))
	for _, t := range rows {
		current[t.ID] = t
	}
	tasks := make([]Task, 0, len(manifest.Issues))
	for _, source := range manifest.Issues {
		task, ok := current[source.ID]
		if !ok {
			return result, errors.New("released import task missing from destination")
		}
		if err = checkImportedTask(task, source, digest, false); err != nil {
			return result, err
		}
		count, readErr := checkImportedComments(ctx, c, task, source)
		if readErr != nil {
			return result, readErr
		}
		batches, encodeErr := importedBatches(source.Comments)
		if encodeErr != nil {
			return result, encodeErr
		}
		if count != len(batches) {
			return result, errors.New("release source comment read-back incomplete")
		}
		tasks = append(tasks, task)
	}
	for i, task := range tasks {
		if manifest.Issues[i].ImportHold || !task.Metadata.ImportHold {
			continue
		}
		metadata := task.Metadata
		metadata.ImportHold = false
		if _, err = c.SetMetadata(ctx, task.ID, metadata); err != nil {
			return result, err
		}
		result.Released++
	}
	return result, nil
}

// Import validates the entire manifest and destination before any mutation.
// apply=false is a read-only preview. A failed apply leaves unfinished issues held;
// re-running the same manifest reads accepted writes back before resuming.
func Import(ctx context.Context, c Importer, raw []byte, apply bool) (ImportResult, error) {
	result := ImportResult{DryRun: !apply}
	if c == nil {
		return result, errors.New("GitHub client required")
	}
	manifest, digest, err := validateManifest(raw, c.Repository())
	if err != nil {
		return result, err
	}
	result.Issues = len(manifest.Issues)
	rows, err := c.List(ctx)
	if err != nil {
		return result, err
	}
	existing := map[string]Task{}
	names := map[string]string{}
	sourceIDs := map[string]bool{}
	for _, s := range manifest.Issues {
		sourceIDs[s.ID] = true
	}
	for _, t := range rows {
		if (t.Metadata.ImportDigest != "" || sourceIdentifier.MatchString(t.Metadata.Identifier) && t.ID != t.nativeID) && !sourceIDs[t.ID] {
			return result, errors.New("destination contains source identity outside complete manifest")
		}
		existing[t.ID] = t
		names[t.Identifier] = t.ID
	}
	// Preflight every existing issue and all its comments before writing any issue.
	counts := map[string]int{}
	for _, s := range manifest.Issues {
		if id, ok := names[s.Identifier]; ok && id != s.ID {
			return result, errors.New("source identifier belongs to another destination identity")
		}
		if t, ok := existing[s.ID]; ok {
			if err = checkImportedTask(t, s, digest, true); err != nil {
				return result, err
			}
			counts[s.ID], err = checkImportedComments(ctx, c, t, s)
			if err != nil {
				return result, err
			}
		}
	}
	if !apply {
		return result, nil
	}
	// All issues are present and held before adding comments or releasing any hold.
	for _, s := range manifest.Issues {
		if _, ok := existing[s.ID]; ok {
			continue
		}
		in := s.CreateInput
		in.ImportHold = true
		in.ImportDigest = digest
		t, err := c.CreateImported(ctx, in)
		if err != nil {
			return result, err
		}
		existing[s.ID] = t
		result.Created++
	}
	for _, s := range manifest.Issues {
		if !importNativeStateMatches(existing[s.ID], s) {
			if _, err = c.UpdateIssue(ctx, s.ID, s.State, nil, nil); err != nil {
				return result, err
			}
		}
	}
	for _, s := range manifest.Issues {
		batches, batchErr := importedBatches(s.Comments)
		if batchErr != nil {
			return result, batchErr
		}
		for _, body := range batches[counts[s.ID]:] {
			if _, err = c.CreateComment(ctx, s.ID, body); err != nil {
				return result, err
			}
			result.CommentsCreated++
		}
	}
	// Complete read-back across the whole manifest precedes the first release.
	for _, s := range manifest.Issues {
		t, err := c.Read(ctx, s.ID)
		if err != nil {
			return result, err
		}
		if err = checkImportedTask(t, s, digest, false); err != nil {
			return result, err
		}
		n, err := checkImportedComments(ctx, c, t, s)
		if err != nil {
			return result, err
		}
		batches, encodeErr := importedBatches(s.Comments)
		if encodeErr != nil {
			return result, encodeErr
		}
		if n != len(batches) {
			return result, errors.New("import comment read-back incomplete")
		}
		existing[s.ID] = t
	}
	return result, nil
}

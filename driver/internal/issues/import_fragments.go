package issues

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/task"
)

const importedFragmentMarker = "<!-- orca-import-comment-v2: "
const importedFragmentBytes = 30000
const importedFragmentCaption = "导入历史分片：完整来源评论须收齐并通过摘要核对；GitHub 发布者不代表原作者。"

type importedFragment struct {
	ID    string `json:"id"`
	Part  int    `json:"part"`
	Total int    `json:"total"`
	Hash  string `json:"sha256"`
	Data  string `json:"data"`
}

func encodeImportedFragments(comment task.Comment) ([]string, error) {
	raw, err := json.Marshal(comment)
	if err != nil {
		return nil, err
	}
	total := (len(raw) + importedFragmentBytes - 1) / importedFragmentBytes
	bodies := make([]string, 0, total)
	for part := 0; part < total; part++ {
		data := raw[part*importedFragmentBytes : min((part+1)*importedFragmentBytes, len(raw))]
		fragment := importedFragment{ID: comment.ID, Part: part, Total: total, Hash: importHash(raw), Data: base64.StdEncoding.EncodeToString(data)}
		encoded, err := json.Marshal(fragment)
		if err != nil {
			return nil, err
		}
		body := importedFragmentMarker + string(encoded) + " -->\n\n" + importedFragmentCaption
		if len(body) > importCommentLimit {
			return nil, errors.New("source comment fragment metadata exceeds destination limit")
		}
		bodies = append(bodies, body)
	}
	return bodies, nil
}

func decodeImportedFragment(body string) (importedFragment, []byte, error) {
	var fragment importedFragment
	raw, caption, ok := strings.Cut(strings.TrimPrefix(body, importedFragmentMarker), " -->\n\n")
	if !ok || len(body) > importCommentLimit || caption != importedFragmentCaption {
		return fragment, nil, errors.New("invalid imported comment fragment")
	}
	if err := decodeImportJSON([]byte(raw), &fragment); err != nil {
		return fragment, nil, err
	}
	data, err := base64.StdEncoding.Strict().DecodeString(fragment.Data)
	if err != nil || fragment.ID == "" || len(fragment.Hash) != 64 || fragment.Total < 1 || fragment.Part < 0 || fragment.Part >= fragment.Total || len(data) == 0 || len(data) > importedFragmentBytes {
		return fragment, nil, errors.New("invalid imported fragment identity or payload")
	}
	return fragment, data, nil
}

func decodeImportedHistory(comments []task.Comment) ([]task.Comment, error) {
	rows := []task.Comment{}
	seen := map[string]bool{}
	for index := 0; index < len(comments); {
		if !strings.HasPrefix(comments[index].Body, importedFragmentMarker) {
			decoded, err := DecodeImportedComments(comments[index])
			if err != nil {
				return nil, err
			}
			for _, comment := range decoded {
				if seen[comment.ID] {
					return nil, errors.New("duplicate logical comment identity")
				}
				seen[comment.ID] = true
			}
			rows = append(rows, decoded...)
			index++
			continue
		}
		comment, count, err := decodeFragmentGroup(comments[index:])
		if err != nil {
			return nil, err
		}
		if seen[comment.ID] {
			return nil, errors.New("duplicate logical comment identity")
		}
		seen[comment.ID] = true
		rows = append(rows, comment)
		index += count
	}
	return rows, nil
}

func decodeFragmentGroup(comments []task.Comment) (task.Comment, int, error) {
	first, _, err := decodeImportedFragment(comments[0].Body)
	if err != nil || first.Part != 0 || first.Total > len(comments) {
		return task.Comment{}, 0, errors.New("incomplete imported comment fragments")
	}
	var raw []byte
	for part := 0; part < first.Total; part++ {
		if !strings.HasPrefix(comments[part].Body, importedFragmentMarker) {
			return task.Comment{}, 0, errors.New("interrupted imported comment fragments")
		}
		fragment, data, err := decodeImportedFragment(comments[part].Body)
		if err != nil || fragment.ID != first.ID || fragment.Hash != first.Hash || fragment.Total != first.Total || fragment.Part != part {
			return task.Comment{}, 0, errors.New("conflicting imported comment fragments")
		}
		raw = append(raw, data...)
	}
	var comment task.Comment
	if importHash(raw) != first.Hash || decodeImportJSON(raw, &comment) != nil || comment.ID != first.ID || !validSourceComment(comment) {
		return task.Comment{}, 0, errors.New("imported comment fragment integrity mismatch")
	}
	return comment, first.Total, nil
}

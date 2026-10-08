package issues

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/vmxmy/orca-factory/driver/internal/linear"
)

func Preview(raw []byte, repository string) (map[string]any, error) {
	manifest, digest, err := validateManifest(raw, repository)
	if err != nil {
		return nil, err
	}
	comments, fragments, batches := 0, 0, 0
	for _, issue := range manifest.Issues {
		comments += len(issue.Comments)
		encoded, err := importedBatches(issue.Comments)
		if err != nil {
			return nil, err
		}
		batches += len(encoded)
		for _, comment := range issue.Comments {
			if len(encodeImportedComments([]linear.Comment{comment})) > importCommentLimit {
				fragments++
			}
		}
	}
	return map[string]any{"repository": repository, "issues": len(manifest.Issues), "comments": comments, "fragmentedComments": fragments, "destinationComments": batches, "targetEncodingValid": true, "sha256": digest, "complete": true}, nil
}

func Export(ctx context.Context, source *linear.Client, repository, project string) (ImportManifest, error) {
	if source == nil {
		return ImportManifest{}, errors.New("authorized source client required")
	}
	rows, err := source.ExportIssues(ctx, project)
	if err != nil {
		return ImportManifest{}, err
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	histories, err := source.ExportComments(ctx, ids)
	if err != nil {
		return ImportManifest{}, err
	}
	manifest := ImportManifest{Version: 1, Repository: repository, Complete: true, Issues: []ImportIssue{}}
	for _, row := range rows {
		comments := histories[row.ID]
		sourceComments := make([]linear.Comment, 0, len(comments))
		for _, comment := range comments {
			copy := comment
			copy.Source = "linear:" + row.ID
			sourceComments = append(sourceComments, copy)
		}
		in := CreateInput{ID: row.ID, Identifier: row.Identifier, Title: row.Title, Description: row.Description, State: row.State.Name, Priority: row.Priority, Estimate: row.Estimate, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Labels: []string{}, Blockers: []string{}, Related: []string{}}
		if row.Parent != nil {
			in.Parent = row.Parent.ID
		}
		for _, label := range row.Labels.Nodes {
			in.Labels = append(in.Labels, label.Name)
		}
		for _, relation := range row.InverseRelations.Nodes {
			switch relation.Type {
			case "blocks":
				in.Blockers = append(in.Blockers, relation.Issue.ID)
			case "related":
				in.Related = append(in.Related, relation.Issue.ID)
			default:
				return ImportManifest{}, errors.New("source relation cannot be represented without loss")
			}
		}
		for _, relation := range row.Relations.Nodes {
			switch relation.Type {
			case "related":
				in.Related = append(in.Related, relation.RelatedIssue.ID)
			case "blocks":
			default:
				return ImportManifest{}, errors.New("source relation cannot be represented without loss")
			}
		}
		manifest.Issues = append(manifest.Issues, ImportIssue{CreateInput: in, Comments: sourceComments, CommentsComplete: true, RelationsComplete: true})
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return ImportManifest{}, err
	}
	_, _, err = validateSourceManifest(raw, repository)
	return manifest, err
}

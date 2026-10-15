package issues

import (
	"github.com/vmxmy/orca-factory/driver/internal/task"
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
			if len(encodeImportedComments([]task.Comment{comment})) > importCommentLimit {
				fragments++
			}
		}
	}
	return map[string]any{"repository": repository, "issues": len(manifest.Issues), "comments": comments, "fragmentedComments": fragments, "destinationComments": batches, "targetEncodingValid": true, "sha256": digest, "complete": true}, nil
}

package github

import (
	"encoding/json"
)

func WithCommitOrderMetadata(metadataJSON string, listOrder int, stableOrder int) string {
	metadata := map[string]any{}
	if metadataJSON != "" {
		var existing map[string]any
		if err := json.Unmarshal([]byte(metadataJSON), &existing); err == nil && existing != nil {
			metadata = existing
		}
	}
	metadata["commit_order"] = listOrder
	metadata["commit_order_key"] = stableOrder
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return metadataJSON
	}
	return string(encoded)
}

func NormalizeCommentVisibilityMetadata(visibility CommentVisibility) string {
	if !visibility.Hidden {
		return ""
	}
	metadata := map[string]any{"provider_hidden": true}
	if visibility.Reason != "" {
		metadata["provider_hidden_reason"] = visibility.Reason
	}
	encoded, _ := json.Marshal(metadata)
	return string(encoded)
}

// CommentVisibility carries GitHub GraphQL-only moderation state alongside
// the REST-shaped comment objects used by the existing sync pipeline.
type CommentVisibility struct {
	Hidden bool
	Reason string
}

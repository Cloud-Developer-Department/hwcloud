// Package resource defines the ResourceProvider — external reference
// material (docs, API specs, templates, example code) that the Context
// Runtime can search and load on demand. Resources are context inputs,
// like skills and memories: the agent recalls what's relevant instead of
// carrying everything.
package resource

import (
	"context"
)

// Provider searches and loads external reference material. A nil provider
// means no resource injection.
type Provider interface {
	// Search returns up to limit resources relevant to query.
	Search(ctx context.Context, query string, limit int) ([]Resource, error)

	// Load fetches a resource's full content by URI.
	Load(ctx context.Context, uri string) (*Resource, error)
}

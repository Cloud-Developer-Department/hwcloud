package openviking

import (
	"context"

	"github.com/Cloud-Developer-Department/hwcloud/provider/resource"
)

// Resource implements provider/resource.Provider backed by OpenViking's
// resource index: Search finds matching resources, Load expands a
// viking:// URI to its full content via the read tool.
type Resource struct {
	client *Client
}

// NewResource creates the resource provider.
func NewResource(client *Client) *Resource {
	return &Resource{client: client}
}

// Search implements provider/resource.Provider.
func (r *Resource) Search(ctx context.Context, query string, limit int) ([]resource.Resource, error) {
	items, err := r.client.Search(ctx, query, limit, "resource")
	if err != nil {
		return nil, err
	}
	out := make([]resource.Resource, 0, len(items))
	for _, it := range items {
		out = append(out, resource.Resource{
			URI:     it.ID,
			Content: it.Content,
		})
	}
	return out, nil
}

// Load implements provider/resource.Provider: full content by URI.
func (r *Resource) Load(ctx context.Context, uri string) (*resource.Resource, error) {
	content, err := r.client.Read(ctx, uri)
	if err != nil {
		return nil, err
	}
	return &resource.Resource{URI: uri, Content: content}, nil
}

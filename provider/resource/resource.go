package resource

// Resource is a referenceable external artifact (document, template, API
// spec, example code) that a Provider can load on demand.
type Resource struct {
	URI       string `json:"uri"`
	MIMEType  string `json:"mime_type,omitempty"`
	Content   string `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

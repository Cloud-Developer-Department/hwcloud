package resource

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FS serves reference documents from a directory tree. Each file becomes a
// Resource; Search ranks by filename/content keyword overlap; Load reads a
// file by absolute path (path-traversal guarded to the root).
type FS struct {
	root string
}

// NewFS creates a provider rooted at dir (must exist or Search returns none).
func NewFS(dir string) *FS {
	return &FS{root: dir}
}

// Search implements Provider: walks the tree, scores files by keyword
// overlap with the query (filename > content), returns top limit.
func (f *FS) Search(ctx context.Context, query string, limit int) ([]Resource, error) {
	if f.root == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	intentWords := tokenize(query)

	type scored struct {
		uri   string
		score int
	}
	var hits []scored
	err := filepath.Walk(f.root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(f.root, path)
		score := 0
		for _, w := range intentWords {
			if strings.Contains(strings.ToLower(rel), w) {
				score += 2 // filename match weighs more
			}
		}
		if score == 0 {
			// Cheap content peek for scoring.
			if data, err := os.ReadFile(path); err == nil && len(data) <= 64*1024 {
				lower := strings.ToLower(string(data))
				for _, w := range intentWords {
					if strings.Contains(lower, w) {
						score++
					}
				}
			}
		}
		if score > 0 {
			hits = append(hits, scored{uri: path, score: score})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Resource, 0, len(hits))
	for _, h := range hits {
		out = append(out, Resource{URI: h.uri, MIMEType: mimeOf(h.uri)})
	}
	return out, nil
}

// Load implements Provider: reads the file at uri (must stay under root).
func (f *FS) Load(ctx context.Context, uri string) (*Resource, error) {
	if f.root == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(uri)
	if err != nil {
		return nil, err
	}
	rootAbs, _ := filepath.Abs(f.root)
	if !strings.HasPrefix(abs, rootAbs+string(os.PathSeparator)) && abs != rootAbs {
		return nil, os.ErrPermission
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return &Resource{URI: uri, MIMEType: mimeOf(uri), Content: string(data)}, nil
}

func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) >= 3 {
			out = append(out, f)
		}
	}
	return out
}

func mimeOf(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".txt":
		return "text/plain"
	case ".html", ".htm":
		return "text/html"
	default:
		return "text/plain"
	}
}

package skill

import (
	"context"
	"sort"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/skill/fs"
)

// FSBridge is the filesystem-backed Provider: Match ranks the catalog by
// keyword overlap between the intent and each skill's
// name/description/frontmatter; Discover and Load delegate to the loader.
type FSBridge struct {
	loader *fs.Loader
}

// NewFSBridge wraps a filesystem loader. nil loader yields an empty match.
func NewFSBridge(loader *fs.Loader) *FSBridge {
	return &FSBridge{loader: loader}
}

// Match implements Provider. It discovers the catalog, scores each skill
// by keyword overlap with the intent, and returns the top `limit`.
func (b *FSBridge) Match(ctx context.Context, intent string, limit int) ([]hwcloud.SkillInfo, error) {
	if b.loader == nil {
		return nil, nil
	}
	all, err := b.loader.Discover(ctx)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}

	intentWords := tokenize(intent)
	type scored struct {
		info  hwcloud.SkillInfo
		score int
	}
	scoredSkills := make([]scored, 0, len(all))
	for _, s := range all {
		haystack := strings.ToLower(s.Name + " " + s.Description)
		for _, fm := range s.Frontmatter {
			haystack += " " + strings.ToLower(fmtValue(fm))
		}
		score := 0
		for _, w := range intentWords {
			if strings.Contains(haystack, w) {
				score++
			}
		}
		if score > 0 {
			scoredSkills = append(scoredSkills, scored{info: s, score: score})
		}
	}
	sort.SliceStable(scoredSkills, func(i, j int) bool { return scoredSkills[i].score > scoredSkills[j].score })
	if len(scoredSkills) > limit {
		scoredSkills = scoredSkills[:limit]
	}
	out := make([]hwcloud.SkillInfo, len(scoredSkills))
	for i, s := range scoredSkills {
		out[i] = s.info
	}
	return out, nil
}

// Discover implements Provider — the full catalog (frontmatter only).
func (b *FSBridge) Discover(ctx context.Context) ([]hwcloud.SkillInfo, error) {
	if b.loader == nil {
		return nil, nil
	}
	return b.loader.Discover(ctx)
}

// Load implements Provider.
func (b *FSBridge) Load(ctx context.Context, skill hwcloud.SkillInfo) (string, error) {
	if b.loader == nil {
		return "", nil
	}
	return b.loader.Load(ctx, skill)
}

// tokenize splits intent into lowercased keywords: latin alphanumeric
// runs (3+ chars) plus CJK ideographs (kept — splitting on them would
// shred Chinese intents into empty keywords). Mirrors the knowledge
// provider's keyword tokenizer.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= '一' && r <= '鿿')
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) >= 3 {
			out = append(out, f)
		}
	}
	return out
}

// fmtValue renders a frontmatter value for matching.
func fmtValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

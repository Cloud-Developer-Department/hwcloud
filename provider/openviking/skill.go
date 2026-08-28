package openviking

import (
	"context"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/skill/fs"
)

// Skill implements provider/skill.Provider backed by OpenViking's skill
// index. Discover returns the full catalog (GET /api/v1/skills);
// Match runs semantic search (POST /api/v1/search/search);
// Load fetches the full SKILL.md body via GET /api/v1/content/read.
type Skill struct {
	client *Client
	loader *fs.Loader // optional local fallback for full instructions
}

// NewSkill creates the skill provider. loader is optional (used when a
// match carries no embedded content).
func NewSkill(client *Client, loader *fs.Loader) *Skill {
	return &Skill{client: client, loader: loader}
}

// Match implements provider/skill.Provider.
func (s *Skill) Match(ctx context.Context, intent string, limit int) ([]hwcloud.SkillInfo, error) {
	items, err := s.client.Search(ctx, intent, limit, "skill")
	if err != nil {
		return nil, err
	}
	out := make([]hwcloud.SkillInfo, 0, len(items))
	for _, it := range items {
		info := hwcloud.SkillInfo{
			Name:        skillNameFromURI(it.ID),
			Description: it.Content,
			Path:        skillRootFromURI(it.ID),
		}
		out = append(out, info)
	}
	return out, nil
}

// Discover implements provider/skill.Provider: the full skill catalog
// from OpenViking's GET /api/v1/skills listing endpoint.
func (s *Skill) Discover(ctx context.Context) ([]hwcloud.SkillInfo, error) {
	entries, err := s.client.ListSkills(ctx, 1000)
	if err != nil {
		return nil, err
	}
	out := make([]hwcloud.SkillInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, hwcloud.SkillInfo{
			Name:        e.Name,
			Description: e.Description,
			Path:        e.URI,
		})
	}
	return out, nil
}

// Load implements provider/skill.Provider.
func (s *Skill) Load(ctx context.Context, skill hwcloud.SkillInfo) (string, error) {
	if s.loader != nil {
		return s.loader.Load(ctx, skill)
	}
	if skill.Path == "" {
		return skill.Description, nil
	}
	content, err := s.client.Read(ctx, strings.TrimSuffix(skill.Path, "/")+"/SKILL.md")
	if err != nil {
		return skill.Description, nil
	}
	return content, nil
}

// skillNameFromURI extracts the skill name from a viking:// URI.
//
//	"viking://user/default/skills/huawei-cloud-cli-guidance/.abstract.md" → "huawei-cloud-cli-guidance"
//	"viking://user/default/skills/huawei-cloud-cli-guidance"             → "huawei-cloud-cli-guidance"
func skillNameFromURI(uri string) string {
	idx := strings.Index(uri, "/skills/")
	if idx == -1 {
		return ""
	}
	rest := uri[idx+len("/skills/"):]
	if slash := strings.Index(rest, "/"); slash != -1 {
		rest = rest[:slash]
	}
	return rest
}

// skillRootFromURI strips any trailing file path to get the skill root URI.
//
//	"viking://user/default/skills/huawei-cloud-cli-guidance/.abstract.md" → "viking://user/default/skills/huawei-cloud-cli-guidance"
//	"viking://user/default/skills/huawei-cloud-cli-guidance"             → "viking://user/default/skills/huawei-cloud-cli-guidance"
func skillRootFromURI(uri string) string {
	idx := strings.Index(uri, "/skills/")
	if idx == -1 {
		return uri
	}
	rest := uri[idx+len("/skills/"):]
	if slash := strings.Index(rest, "/"); slash != -1 {
		return uri[:idx+len("/skills/")+slash]
	}
	return uri
}

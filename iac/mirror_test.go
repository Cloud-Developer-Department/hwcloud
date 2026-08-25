package iac

import (
	"os"
	"strings"
	"testing"
)

// The generated .terraformrc must exclude only huaweicloud from direct —
// auxiliary providers (random, tls) still reach the official registry.
func TestWriteProviderMirrorConfigExcludeGranularity(t *testing.T) {
	dir := t.TempDir()
	path, err := writeProviderMirrorConfig(dir, []string{"https://mirrors.huaweicloud.com/terraform/"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rc := string(b)
	if !strings.Contains(rc, `network_mirror`) || !strings.Contains(rc, `mirrors.huaweicloud.com`) {
		t.Fatalf("missing network_mirror block: %s", rc)
	}
	if strings.Contains(rc, `exclude = ["registry.terraform.io/*/*"]`) {
		t.Fatalf("direct excludes ALL providers — auxiliary providers would fail: %s", rc)
	}
	if !strings.Contains(rc, `exclude = ["registry.terraform.io/huaweicloud/*"]`) {
		t.Fatalf("direct should exclude only huaweicloud: %s", rc)
	}
}

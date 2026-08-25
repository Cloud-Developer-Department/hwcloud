package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// mkDag builds a Dag with the given nodes (deps via dependsOn map).
func mkDag(nodes map[string][]string) *Dag {
	d := &Dag{Version: DagVersion, DeploymentID: "d-test", Status: DagProposed}
	for id, deps := range nodes {
		d.Nodes = append(d.Nodes, DagNode{ID: id, Type: "t", Name: id, DependsOn: deps})
	}
	return d
}

func TestValidateDag_Acyclic(t *testing.T) {
	// vpc -> subnet -> ecs (linear, valid)
	d := mkDag(map[string][]string{
		"vpc":    {},
		"subnet": {"vpc"},
		"ecs":    {"subnet"},
	})
	if err := validateDag(d); err != nil {
		t.Fatalf("linear DAG should be valid: %v", err)
	}
}

func TestValidateDag_Cycle(t *testing.T) {
	// a -> b -> a (cycle)
	d := mkDag(map[string][]string{
		"a": {"b"},
		"b": {"a"},
	})
	if err := validateDag(d); err == nil {
		t.Fatal("cyclic DAG should be rejected")
	}
}

func TestValidateDag_SelfCycle(t *testing.T) {
	d := mkDag(map[string][]string{
		"a": {"a"},
	})
	if err := validateDag(d); err == nil {
		t.Fatal("self-cycle should be rejected")
	}
}

func TestValidateDag_DuplicateID(t *testing.T) {
	d := &Dag{Version: DagVersion, DeploymentID: "d", Status: DagProposed}
	d.Nodes = []DagNode{
		{ID: "a", Type: "t", Name: "a"},
		{ID: "a", Type: "t", Name: "a"},
	}
	if err := validateDag(d); err == nil {
		t.Fatal("duplicate id should be rejected")
	}
}

func TestValidateDag_UnknownDep(t *testing.T) {
	d := mkDag(map[string][]string{
		"a": {"nonexistent"},
	})
	if err := validateDag(d); err == nil {
		t.Fatal("unknown dependency should be rejected")
	}
}

func TestValidateDag_EmptyID(t *testing.T) {
	d := &Dag{Version: DagVersion, Status: DagProposed}
	d.Nodes = []DagNode{{ID: "", Type: "t", Name: "x"}}
	if err := validateDag(d); err == nil {
		t.Fatal("empty id should be rejected")
	}
}

func TestValidateDag_NoNodes(t *testing.T) {
	d := &Dag{Version: DagVersion, Status: DagProposed}
	if err := validateDag(d); err == nil {
		t.Fatal("empty DAG should be rejected")
	}
}

func TestValidateDag_TooManyNodes(t *testing.T) {
	d := &Dag{Version: DagVersion, Status: DagProposed}
	for i := 0; i <= maxDagNodes; i++ {
		d.Nodes = append(d.Nodes, DagNode{ID: "n" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Type: "t", Name: "n"})
	}
	// Use unique ids
	d.Nodes = d.Nodes[:0]
	for i := 0; i <= maxDagNodes; i++ {
		d.Nodes = append(d.Nodes, DagNode{ID: "node" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676%26)), Type: "t", Name: "n"})
	}
	if err := validateDag(d); err == nil {
		t.Fatalf("DAG with %d nodes (>max %d) should be rejected", len(d.Nodes), maxDagNodes)
	}
}

func TestValidateDag_BadVersion(t *testing.T) {
	d := mkDag(map[string][]string{"a": {}})
	d.Version = 999
	if err := validateDag(d); err == nil {
		t.Fatal("unknown schema version should be rejected")
	}
}

func TestValidateDag_DuplicateDep(t *testing.T) {
	// node lists same dep twice
	d := mkDag(map[string][]string{
		"a": {},
		"b": {"a", "a"},
	})
	if err := validateDag(d); err == nil {
		t.Fatal("duplicate dependency should be rejected")
	}
}

func TestLoadDag_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := loadDag(dir)
	if err == nil {
		t.Fatal("missing dag.json should error")
	}
}

func TestLoadDag_BadJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dag.json"), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := loadDag(dir)
	if err == nil {
		t.Fatal("bad JSON should error")
	}
}

func TestNodeSpecsFilled(t *testing.T) {
	d := &Dag{Version: DagVersion, Status: DagSpecified}
	d.Nodes = []DagNode{
		{ID: "a", Type: "t", Name: "a", Spec: map[string]any{"x": 1}},
		{ID: "b", Type: "t", Name: "b", Spec: map[string]any{"y": 2}},
	}
	if !nodeSpecsFilled(d) {
		t.Fatal("all nodes with specs should be filled")
	}
	d.Nodes[1].Spec = nil
	if nodeSpecsFilled(d) {
		t.Fatal("node with nil spec should not be filled")
	}
}

func TestHasCost_InvalidateCost(t *testing.T) {
	dir := t.TempDir()
	if HasCost(dir) {
		t.Fatal("no cost.json should report false")
	}
	// saveCost then HasCost
	if err := saveCost(dir, &CostEstimate{Version: CostVersion, DeploymentID: "d"}); err != nil {
		t.Fatal(err)
	}
	if !HasCost(dir) {
		t.Fatal("cost.json present should report true")
	}
	if err := invalidateCost(dir); err != nil {
		t.Fatal(err)
	}
	if HasCost(dir) {
		t.Fatal("after invalidate, HasCost should be false")
	}
	// invalidate again (already gone) should not error
	if err := invalidateCost(dir); err != nil {
		t.Fatalf("invalidate on missing cost should be no-op: %v", err)
	}
}

// TestSaveDag_AtomicWrite verifies that saveDag writes atomically — a
// concurrent loadDag should never see a half-written file. We can't easily
// simulate a mid-write race in a unit test, but we verify the mechanism:
// saveDag should leave no .tmp file behind on success, and the written
// file should be valid JSON readable by loadDag.
func TestSaveDag_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	dag := &Dag{
		Version:      DagVersion,
		DeploymentID: "d-atomic",
		Status:       DagPlanned,
		Nodes:        []DagNode{{ID: "a", Type: "t", Name: "a", Spec: map[string]any{"x": 1}}},
	}
	if err := saveDag(dir, dag); err != nil {
		t.Fatal(err)
	}
	// No leftover temp file.
	if _, err := os.Stat(filepath.Join(dir, "dag.json.tmp")); err == nil {
		t.Fatal("saveDag left a .tmp file behind — atomic write did not complete cleanly")
	}
	// loadDag reads back the exact same content.
	loaded, err := loadDag(dir)
	if err != nil {
		t.Fatalf("loadDag after saveDag failed: %v", err)
	}
	if loaded.DeploymentID != "d-atomic" || loaded.Status != DagPlanned {
		t.Fatalf("loaded dag mismatch: %+v", loaded)
	}
	if len(loaded.Nodes) != 1 || loaded.Nodes[0].ID != "a" {
		t.Fatalf("loaded nodes mismatch: %+v", loaded.Nodes)
	}
}

// TestSaveCost_AtomicWrite verifies saveCost leaves no .tmp file behind.
func TestSaveCost_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	c := &CostEstimate{Version: CostVersion, DeploymentID: "d", TotalMonthly: 42.0}
	if err := saveCost(dir, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cost.json.tmp")); err == nil {
		t.Fatal("saveCost left a .tmp file behind")
	}
	if !HasCost(dir) {
		t.Fatal("cost.json should exist after saveCost")
	}
}

// TestDagStatusOf verifies the status helper used by apply's state gate.
func TestDagStatusOf(t *testing.T) {
	dir := t.TempDir()
	// No dag.json → empty status.
	if s := DagStatusOf(dir); s != "" {
		t.Fatalf("missing dag should return empty status, got %q", s)
	}
	// Write a planned dag.
	dag := &Dag{Version: DagVersion, DeploymentID: "d", Status: DagPlanned,
		Nodes: []DagNode{{ID: "a", Type: "t", Name: "a"}}}
	if err := saveDag(dir, dag); err != nil {
		t.Fatal(err)
	}
	if s := DagStatusOf(dir); s != DagPlanned {
		t.Fatalf("DagStatusOf = %q, want %q", s, DagPlanned)
	}
}

// TestSetDagStatus_AdvancesToApplied verifies the apply state-machine fix:
// after SetDagStatus(dir, DagApplied), DagStatusOf returns DagApplied.
func TestSetDagStatus_AdvancesToApplied(t *testing.T) {
	dir := t.TempDir()
	dag := &Dag{Version: DagVersion, DeploymentID: "d", Status: DagCostEstimated,
		Nodes: []DagNode{{ID: "a", Type: "t", Name: "a"}}}
	if err := saveDag(dir, dag); err != nil {
		t.Fatal(err)
	}
	if err := SetDagStatus(dir, DagApplied); err != nil {
		t.Fatal(err)
	}
	if s := DagStatusOf(dir); s != DagApplied {
		t.Fatalf("expected DagApplied, got %q", s)
	}
}

// TestSetDagStatus_AdvancesToDestroyed verifies the destroy state-machine fix.
func TestSetDagStatus_AdvancesToDestroyed(t *testing.T) {
	dir := t.TempDir()
	dag := &Dag{Version: DagVersion, DeploymentID: "d", Status: DagApplied,
		Nodes: []DagNode{{ID: "a", Type: "t", Name: "a"}}}
	if err := saveDag(dir, dag); err != nil {
		t.Fatal(err)
	}
	if err := SetDagStatus(dir, DagDestroyed); err != nil {
		t.Fatal(err)
	}
	if s := DagStatusOf(dir); s != DagDestroyed {
		t.Fatalf("expected DagDestroyed, got %q", s)
	}
}

// BenchmarkValidateDag_Linear measures DAG validation cost on a linear chain
// (vpc->subnet->ecs->...), the common deployment shape.
func BenchmarkValidateDag_Linear(b *testing.B) {
	d := &Dag{Version: DagVersion, DeploymentID: "d", Status: DagProposed}
	prev := ""
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("n%02d", i)
		node := DagNode{ID: id, Type: "t", Name: id}
		if prev != "" {
			node.DependsOn = []string{prev}
		}
		d.Nodes = append(d.Nodes, node)
		prev = id
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := validateDag(d); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkValidateDag_Diamond measures validation on a diamond DAG (worst
// case for Kahn's algorithm — many deps per node).
func BenchmarkValidateDag_Diamond(b *testing.B) {
	d := &Dag{Version: DagVersion, DeploymentID: "d", Status: DagProposed}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("n%02d", i)
		node := DagNode{ID: id, Type: "t", Name: id}
		// Each node depends on all earlier nodes.
		for j := 0; j < i; j++ {
			node.DependsOn = append(node.DependsOn, fmt.Sprintf("n%02d", j))
		}
		d.Nodes = append(d.Nodes, node)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := validateDag(d); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSaveDag_AtomicWrite measures the atomic write (tmp+rename) path
// — the hot path during specify_resources/generate/estimate.
func BenchmarkSaveDag_AtomicWrite(b *testing.B) {
	dir := b.TempDir()
	dag := &Dag{Version: DagVersion, DeploymentID: "d", Status: DagPlanned}
	for i := 0; i < 10; i++ {
		dag.Nodes = append(dag.Nodes, DagNode{
			ID: fmt.Sprintf("n%02d", i), Type: "huaweicloud_compute_instance", Name: "web",
			Spec: map[string]any{"flavor": "s6.large.2", "image": "ubuntu_22_04"},
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := saveDag(dir, dag); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLoadDag measures the load+validate path — get_deployment_status
// and apply's state gate both call this.
func BenchmarkLoadDag(b *testing.B) {
	dir := b.TempDir()
	dag := &Dag{Version: DagVersion, DeploymentID: "d", Status: DagPlanned}
	for i := 0; i < 10; i++ {
		dag.Nodes = append(dag.Nodes, DagNode{ID: fmt.Sprintf("n%02d", i), Type: "t", Name: "n"})
	}
	if err := saveDag(dir, dag); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := loadDag(dir); err != nil {
			b.Fatal(err)
		}
	}
}

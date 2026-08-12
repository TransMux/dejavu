package dejavu

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncAuditBenchmarkNoChange(t *testing.T) {
	fixture := newSyncAuditFixture(t)
	repo, _ := fixture.repo("benchmark", "benchmark_setup", nil)
	if err := os.WriteFile(filepath.Join(repo.DataPath, "doc.txt"), []byte("benchmark"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := auditIndexAndSync(t, repo, "benchmark setup"); err != nil {
		t.Fatal(err)
	}
	const runs = 20
	var total int64
	for i := 0; i < runs; i++ {
		recorder := &syncAuditRecorder{scenario: "no_change_benchmark", origin: time.Now()}
		repo.cloud.(*syncAuditCloud).recorder = recorder
		started := time.Now()
		if _, _, err := repo.Sync(map[string]interface{}{SyncAuditContextKey: recorder}); err != nil {
			t.Fatal(err)
		}
		total += time.Since(started).Nanoseconds()
	}
	t.Logf("no-change sync mean over %d runs: %.3fms", runs, float64(total)/float64(runs)/1e6)
	if _, err := os.Stat(filepath.Join(fixture.root, "cloud")); err != nil {
		t.Fatal(err)
	}
}

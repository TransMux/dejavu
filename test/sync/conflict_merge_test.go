package syncscenario_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConflictMergeHookEndToEnd(t *testing.T) {
	env := newSyncScenarioEnv(t)
	seedCase := &syncScenarioCase{Seed: map[string]string{"box/doc.sy": "base"}}
	seed := env.seedSyncedClient("seed-merge", seedCase)
	local := env.cloneClient(seed, "local-merge")
	remote := env.cloneClient(seed, "remote-merge")

	remote.writeFile("box/doc.sy", "remote", syncScenarioBaseTime().Add(time.Minute))
	remote.index("remote change")
	remote.syncNoConflict(0, 0)

	called := false
	local.repo.SetConflictMerge(func(path string, base, localData, remoteData []byte) ([]byte, bool) {
		called = true
		if path != "/box/doc.sy" || !bytes.Equal(base, []byte("base")) || !bytes.Equal(localData, []byte("local")) || !bytes.Equal(remoteData, []byte("remote")) {
			t.Fatalf("unexpected merge input path=%q base=%q local=%q remote=%q", path, base, localData, remoteData)
		}
		return []byte("merged"), true
	})
	local.writeFile("box/doc.sy", "local", syncScenarioBaseTime().Add(2*time.Minute))
	local.index("local change")
	result := local.sync()
	local.assertMergeResult(result, syncScenarioExpectation{Upserts: 1})
	local.assertFile("box/doc.sy", "merged")
	if !called {
		t.Fatal("conflict merge hook was not called")
	}

	// 合并结果必须在同一次同步中上传，不得依赖手工索引或第二次同步。
	remote.sync()
	remote.assertFile("box/doc.sy", "merged")
}

func TestConflictMergeHookRollsBackWorkspaceWhenHistoryFails(t *testing.T) {
	env := newSyncScenarioEnv(t)
	seed := env.seedSyncedClient("seed-rollback", &syncScenarioCase{Seed: map[string]string{"box/doc.sy": "base"}})
	local := env.cloneClient(seed, "local-rollback")
	remote := env.cloneClient(seed, "remote-rollback")
	remote.writeFile("box/doc.sy", "remote", syncScenarioBaseTime().Add(time.Minute))
	remote.index("remote change")
	remote.syncNoConflict(0, 0)
	local.repo.SetConflictMerge(func(_ string, _, _, _ []byte) ([]byte, bool) { return []byte("merged"), true })
	local.writeFile("box/doc.sy", "local", syncScenarioBaseTime().Add(2*time.Minute))
	local.index("local change")
	if err := os.RemoveAll(local.historyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(local.historyPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local.historyPath, []byte("not-a-directory"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := local.repo.Sync(map[string]interface{}{}); err == nil {
		t.Fatal("expected history generation failure")
	}
	local.assertFile("box/doc.sy", "local")
}

func TestConflictMergeHookPreservesEditAfterIndex(t *testing.T) {
	env := newSyncScenarioEnv(t)
	seed := env.seedSyncedClient("seed-concurrent", &syncScenarioCase{Seed: map[string]string{"box/doc.sy": "base"}})
	local := env.cloneClient(seed, "local-concurrent")
	remote := env.cloneClient(seed, "remote-concurrent")
	remote.writeFile("box/doc.sy", "remote", syncScenarioBaseTime().Add(time.Minute))
	remote.index("remote change")
	remote.syncNoConflict(0, 0)
	local.writeFile("box/doc.sy", "local", syncScenarioBaseTime().Add(2*time.Minute))
	local.index("local change")
	local.repo.SetConflictMerge(func(_ string, _, _, _ []byte) ([]byte, bool) {
		// 模拟用户在索引完成后、合并写回前又保存了新内容。
		local.writeFile("box/doc.sy", "new-edit", syncScenarioBaseTime().Add(3*time.Minute))
		return []byte("merged"), true
	})
	result := local.sync()
	local.assertMergeResult(result, syncScenarioExpectation{Conflicts: 1})
	local.assertFile("box/doc.sy", "new-edit")
}

func TestConflictMergeHookRejectionFallsBack(t *testing.T) {
	env := newSyncScenarioEnv(t)
	seed := env.seedSyncedClient("seed-reject", &syncScenarioCase{Seed: map[string]string{"box/doc.sy": "base"}})
	local := env.cloneClient(seed, "local-reject")
	remote := env.cloneClient(seed, "remote-reject")

	remote.writeFile("box/doc.sy", "remote", syncScenarioBaseTime().Add(time.Minute))
	remote.index("remote change")
	remote.syncNoConflict(0, 0)
	local.repo.SetConflictMerge(func(_ string, _, _, _ []byte) ([]byte, bool) { return nil, false })
	local.writeFile("box/doc.sy", "local", syncScenarioBaseTime().Add(2*time.Minute))
	local.index("local change")
	result := local.sync()
	local.assertMergeResult(result, syncScenarioExpectation{Conflicts: 1})
	local.assertFile("box/doc.sy", "local")
}

func TestConflictMergeHookIsNotUsedBySyncDownload(t *testing.T) {
	env := newSyncScenarioEnv(t)
	seed := env.seedSyncedClient("seed-download", &syncScenarioCase{Seed: map[string]string{"box/doc.sy": "base"}})
	local := env.cloneClient(seed, "local-download")
	remote := env.cloneClient(seed, "remote-download")
	remote.writeFile("box/doc.sy", "remote", syncScenarioBaseTime().Add(time.Minute))
	remote.index("remote change")
	remote.syncNoConflict(0, 0)
	called := false
	local.repo.SetConflictMerge(func(_ string, _, _, _ []byte) ([]byte, bool) {
		called = true
		return []byte("merged"), true
	})
	local.writeFile("box/doc.sy", "local", syncScenarioBaseTime().Add(2*time.Minute))
	local.index("local change")
	local.syncDownload()
	if called {
		t.Fatal("SyncDownload must preserve its existing overwrite semantics")
	}
}

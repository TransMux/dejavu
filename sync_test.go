// DejaVu - Data snapshot and sync.
// Copyright (c) 2022-present, b3log.org
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package dejavu

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/dejavu/util"
	"github.com/siyuan-note/encryption"
)

type countingLocalCloud struct {
	*cloud.Local
	lockUploads atomic.Int32
}

func (c *countingLocalCloud) UploadObject(filePath string, overwrite bool) (length int64, err error) {
	if "lock-sync" == filePath {
		c.lockUploads.Add(1)
	}
	return c.Local.UploadObject(filePath, overwrite)
}

func TestSync(t *testing.T) {
	if "" == os.Getenv("DEJAVU_SYNC_INTEGRATION") {
		t.Skip("需要设置 DEJAVU_SYNC_INTEGRATION 并启动本地云端测试服务")
	}

	repo, _ := initIndex(t)

	userId := "0"
	token := ""

	repo.cloud = &cloud.SiYuan{BaseCloud: &cloud.BaseCloud{Conf: &cloud.Conf{
		Dir:           "test",
		UserID:        userId,
		AvailableSize: 1024 * 1024 * 1024 * 8,
		Token:         token,
		Server:        "http://127.0.0.1:64388",
	}}}

	mergeResult, trafficStat, err := repo.Sync(nil)
	if nil != err {
		t.Fatalf("sync failed: %s", err)
		return
	}
	_ = mergeResult
	_ = trafficStat
}

func TestSyncCloudLockFastPath(t *testing.T) {
	tempDir := t.TempDir()
	dataPath := filepath.Join(tempDir, "data")
	repoPath := filepath.Join(tempDir, "repo")
	historyPath := filepath.Join(tempDir, "history")
	tempPath := filepath.Join(tempDir, "temp")
	cloudPath := filepath.Join(tempDir, "cloud")
	if err := os.MkdirAll(dataPath, 0755); nil != err {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataPath, "doc.txt"), []byte("data"), 0644); nil != err {
		t.Fatal(err)
	}

	localCloud := &countingLocalCloud{Local: cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{
		Dir:           "main",
		RepoPath:      repoPath,
		AvailableSize: 1024 * 1024 * 1024,
		Local:         &cloud.ConfLocal{Endpoint: cloudPath},
	}})}
	repo, err := NewRepo(dataPath, repoPath, historyPath, tempPath, "device", "Device", "windows",
		[]byte("0123456789abcdef0123456789abcdef"), nil, localCloud)
	if nil != err {
		t.Fatal(err)
	}
	if _, err = repo.Index("Initial index", false, map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}

	if _, _, err = repo.Sync(map[string]interface{}{"skipCloudPreflight": true}); nil != err {
		t.Fatal(err)
	}
	if 1 != localCloud.lockUploads.Load() {
		t.Fatalf("initial sync uploaded cloud lock [%d] times", localCloud.lockUploads.Load())
	}

	if _, _, err = repo.Sync(map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	if 1 != localCloud.lockUploads.Load() {
		t.Fatalf("unchanged sync uploaded cloud lock [%d] times", localCloud.lockUploads.Load())
	}

	if _, _, err = repo.Sync(map[string]interface{}{"skipCloudPreflight": true}); nil != err {
		t.Fatal(err)
	}
	if 2 != localCloud.lockUploads.Load() {
		t.Fatalf("prepared sync uploaded cloud lock [%d] times", localCloud.lockUploads.Load())
	}
}

func TestDiffUpsertRemoveNormalizesLazyAssetPaths(t *testing.T) {
	repo := newLazyTestRepo(t)

	left := []*entity.File{{ID: "left-id", Path: "/assets/a.png", Size: 1, Updated: 1000}}
	right := []*entity.File{{ID: "right-id", Path: "assets/a.png", Size: 1, Updated: 1000}}

	upserts, removes := repo.diffUpsertRemove(left, right, false)
	if len(upserts) != 0 || len(removes) != 0 {
		t.Fatalf("equivalent lazy asset paths should not diff, upserts=%#v removes=%#v", upserts, removes)
	}
}

func TestIndexIgnoresRefUsedStorage(t *testing.T) {
	root := t.TempDir()
	aesKey, err := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := NewRepoWithLazyLoad(
		filepath.Join(root, "workspace"),
		filepath.Join(root, "repo"),
		filepath.Join(root, "history"),
		filepath.Join(root, "temp"),
		deviceID, deviceName, deviceOS, aesKey, ignoreLines(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	storageDir := filepath.Join(repo.DataPath, "storage")
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storageDir, "ref-used.json"), []byte(`{"local":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storageDir, "keep.json"), []byte(`{"sync":true}`), 0644); err != nil {
		t.Fatal(err)
	}

	index, err := repo.Index("ref-used protection", true, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := repo.GetFiles(index)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, file := range files {
		paths[strings.TrimPrefix(filepath.ToSlash(file.Path), "/")] = true
	}
	if paths["storage/ref-used.json"] {
		t.Fatal("storage/ref-used.json must remain device-local")
	}
	if !paths["storage/keep.json"] {
		t.Fatal("control storage file was not indexed")
	}
}

func TestIndexIgnoresRefUsedStorageCaseAlias(t *testing.T) {
	if "darwin" != runtime.GOOS && "windows" != runtime.GOOS {
		t.Skip("case-insensitive alias protection applies to macOS and Windows")
	}
	root := t.TempDir()
	aesKey, err := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := NewRepoWithLazyLoad(
		filepath.Join(root, "workspace"),
		filepath.Join(root, "repo"),
		filepath.Join(root, "history"),
		filepath.Join(root, "temp"),
		deviceID, deviceName, deviceOS, aesKey, ignoreLines(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	storageDir := filepath.Join(repo.DataPath, "Storage")
	if err = os.MkdirAll(storageDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(storageDir, "REF-USED.JSON"), []byte(`{"local":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo.DataPath, "keep.txt"), []byte("sync"), 0644); err != nil {
		t.Fatal(err)
	}

	index, err := repo.Index("ref-used case alias protection", true, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := repo.GetFiles(index)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.EqualFold(strings.TrimPrefix(filepath.ToSlash(file.Path), "/"), "storage/ref-used.json") {
			t.Fatalf("case alias was indexed as [%s]", file.Path)
		}
	}
}

func TestIndexDeduplicatesLazyAssetPaths(t *testing.T) {
	repo := newLazyTestRepo(t)

	relPath := "assets/paper.pdf.sya"
	data := []byte(`{"annotation":true}`)
	absPath := filepath.Join(repo.DataPath, relPath)
	if err := os.WriteFile(absPath, data, 0644); err != nil {
		t.Fatalf("write asset failed: %s", err)
	}
	modTime := time.Unix(1780923939, 0)
	if err := os.Chtimes(absPath, modTime, modTime); err != nil {
		t.Fatalf("chtimes failed: %s", err)
	}
	manifestFile := entity.NewFile(relPath, int64(len(data)), modTime.UnixMilli())
	if err := repo.lazyLoader.saveManifest(&LazyManifest{Version: "1.0", Assets: map[string]*LazyAsset{
		relPath: {Path: relPath, FileID: manifestFile.ID, Size: int64(len(data)), Modified: modTime.UnixMilli()},
	}}); err != nil {
		t.Fatalf("save manifest failed: %s", err)
	}

	index, err := repo.Index("Index 1", true, map[string]interface{}{})
	if err != nil {
		t.Fatalf("index failed: %s", err)
	}
	if len(index.LazyFiles) != 1 || index.LazyFiles[0] != manifestFile.ID {
		t.Fatalf("unexpected lazy files: %#v", index.LazyFiles)
	}
}

func TestIndexDoesNotRewriteUnreadableManifestMetadata(t *testing.T) {
	repo := newLazyTestRepo(t)

	relPath := "assets/corrupt-metadata.png"
	data := []byte("asset")
	chunkID := util.Hash(data)
	manifestFile := entity.NewFile(relPath, int64(len(data)), 1000)
	manifestFile.Chunks = []string{chunkID}
	if err := repo.lazyLoader.saveManifest(&LazyManifest{Version: "1.0", Assets: map[string]*LazyAsset{
		relPath: {
			Path:     relPath,
			FileID:   manifestFile.ID,
			Size:     manifestFile.Size,
			Modified: manifestFile.Updated,
			Chunks:   manifestFile.Chunks,
		},
	}}); err != nil {
		t.Fatalf("save manifest failed: %s", err)
	}

	dir, file := repo.store.AbsPath(manifestFile.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir object dir failed: %s", err)
	}
	if err := os.WriteFile(file, []byte("bad metadata"), 0644); err != nil {
		t.Fatalf("write bad metadata failed: %s", err)
	}

	if _, err := repo.Index("Index 1", true, map[string]interface{}{}); nil == err {
		t.Fatal("index with unreadable immutable metadata should fail closed")
	}
	if _, err := repo.store.GetFile(manifestFile.ID); err == nil {
		t.Fatal("ordinary index rewrote unreadable lazy metadata")
	}
}

func TestGetLazyFilesForIndexSkipsDSStore(t *testing.T) {
	repo := newLazyTestRepo(t)

	chunkID := util.Hash([]byte("asset"))
	manifest := &LazyManifest{
		Version: "1.0",
		Assets: map[string]*LazyAsset{
			"assets/.DS_Store": {
				Path:     "assets/.DS_Store",
				FileID:   "ds-store-id",
				Size:     1,
				Modified: 1000,
				Chunks:   []string{chunkID},
			},
			"assets/keep.png": {
				Path:     "assets/keep.png",
				FileID:   "keep-id",
				Size:     1,
				Modified: 1000,
				Chunks:   []string{chunkID},
			},
		},
	}
	if err := repo.lazyLoader.saveManifest(manifest); err != nil {
		t.Fatalf("save manifest failed: %s", err)
	}

	files, err := repo.getLazyFilesForIndex()
	if err != nil {
		t.Fatalf("get lazy files failed: %s", err)
	}
	if len(files) != 1 || files[0].Path != "assets/keep.png" {
		t.Fatalf("unexpected lazy files: %#v", files)
	}
}

func TestBuildCheckIndexIncludesLazyFiles(t *testing.T) {
	normal := &entity.File{ID: "normal", Chunks: []string{"normal-chunk"}}
	lazy := &entity.File{ID: "lazy", Chunks: []string{"lazy-chunk"}}
	index := &entity.Index{ID: "index", Files: []string{normal.ID}, LazyFiles: []string{lazy.ID}}
	check := buildCheckIndex(index, []*entity.File{normal, lazy})
	if len(check.Files) != 2 {
		t.Fatalf("check files = %#v", check.Files)
	}
	if check.Files[1].ID != lazy.ID || len(check.Files[1].Chunks) != 1 || check.Files[1].Chunks[0] != "lazy-chunk" {
		t.Fatalf("lazy file closure missing: %#v", check.Files)
	}
}

func TestLazyManifestContainsFileIDs(t *testing.T) {
	manifest := &LazyManifest{Assets: map[string]*LazyAsset{
		"assets/a": {Path: "assets/a", FileID: "a"},
		"assets/b": {Path: "assets/b", FileID: "b"},
	}}
	if !lazyManifestContainsFileIDs(manifest, []string{"b", "a"}) {
		t.Fatal("same manifest closure was not recognized")
	}
	if lazyManifestContainsFileIDs(manifest, []string{"a", "c"}) {
		t.Fatal("different manifest closure was recognized as unchanged")
	}
}

func TestLazyManifestHydratedMarkerIsScopedToPublishedIdentity(t *testing.T) {
	repo := newLazyTestRepo(t)
	if err := os.MkdirAll(repo.Path, 0755); nil != err {
		t.Fatal(err)
	}
	first := util.RandHash()
	second := util.RandHash()
	if err := repo.markLazyManifestHydrated(first); nil != err {
		t.Fatal(err)
	}
	if !repo.isLazyManifestHydrated(first) {
		t.Fatal("published identity marker was not recognized")
	}
	if repo.isLazyManifestHydrated(second) {
		t.Fatal("a different published manifest identity was skipped")
	}
	if err := os.WriteFile(filepath.Join(repo.Path, lazyManifestHydratedMarker), []byte(lazyManifestFormatCurrent), 0600); nil != err {
		t.Fatal(err)
	}
	if repo.isLazyManifestHydrated(first) {
		t.Fatal("legacy format-only marker bypassed identity-scoped consumption")
	}
}

func TestDiffLazyManifestsClassifiesCatalogChangesAndIgnoresStatus(t *testing.T) {
	unchanged := &LazyAsset{Path: "assets/unchanged", FileID: util.RandHash(), Size: 1, Modified: 1000,
		Chunks: []string{util.RandHash()}, Status: LazyStatusPending}
	updated := &LazyAsset{Path: "assets/updated", FileID: util.RandHash(), Size: 2, Modified: 2000,
		Chunks: []string{util.RandHash()}}
	deleted := &LazyAsset{Path: "assets/deleted", FileID: util.RandHash(), Size: 3, Modified: 3000}
	revivedTombstone := &LazyTombstone{Path: "assets/revived", FileID: util.RandHash(), DeletedAt: 4000}
	previous := &LazyManifest{Assets: map[string]*LazyAsset{
		unchanged.Path: cloneLazyAsset(unchanged), updated.Path: cloneLazyAsset(updated), deleted.Path: cloneLazyAsset(deleted),
	}, Tombstones: map[string]*LazyTombstone{revivedTombstone.Path: revivedTombstone}}
	currentUnchanged := cloneLazyAsset(unchanged)
	currentUnchanged.Status = LazyStatusCached
	currentUpdated := cloneLazyAsset(updated)
	currentUpdated.Chunks = append(currentUpdated.Chunks, util.RandHash())
	added := &LazyAsset{Path: "assets/added", FileID: util.RandHash(), Size: 5, Modified: 5000}
	revived := &LazyAsset{Path: revivedTombstone.Path, FileID: util.RandHash(), Size: 6, Modified: 6000,
		RevivesDeletion: revivedTombstone.DeletedAt}
	deletion := &LazyTombstone{Path: deleted.Path, FileID: deleted.FileID, DeletedAt: 7000}
	current := &LazyManifest{Assets: map[string]*LazyAsset{
		currentUnchanged.Path: currentUnchanged, currentUpdated.Path: currentUpdated, added.Path: added, revived.Path: revived,
	}, Tombstones: map[string]*LazyTombstone{deletion.Path: deletion}}

	delta, err := diffLazyManifests(previous, current)
	if nil != err {
		t.Fatal(err)
	}
	if len(delta.Adds) != 1 || delta.Adds[0] != added || len(delta.Updates) != 1 || delta.Updates[0] != currentUpdated ||
		len(delta.Deletes) != 1 || delta.Deletes[0] != deletion || len(delta.Revives) != 1 || delta.Revives[0] != revived {
		t.Fatalf("unexpected lazy manifest delta: %#v", delta)
	}
}

func TestDiffLazyManifestsRejectsUnprovenDeletionAndRevival(t *testing.T) {
	asset := &LazyAsset{Path: "assets/a", FileID: util.RandHash(), Size: 1, Modified: 1000}
	if _, err := diffLazyManifests(&LazyManifest{Assets: map[string]*LazyAsset{asset.Path: asset}},
		&LazyManifest{Assets: map[string]*LazyAsset{}}); nil == err {
		t.Fatal("asset disappearance without tombstone was accepted as a deletion")
	}
	tombstone := &LazyTombstone{Path: asset.Path, FileID: asset.FileID, DeletedAt: 2000}
	if _, err := diffLazyManifests(&LazyManifest{Tombstones: map[string]*LazyTombstone{asset.Path: tombstone}},
		&LazyManifest{Assets: map[string]*LazyAsset{asset.Path: asset}}); nil == err {
		t.Fatal("tombstone disappearance without revival proof was accepted")
	}
}

func TestIndexChangedLazyManifestUsesManifestBaselineWithoutReadingUnchangedMetadata(t *testing.T) {
	repo := newLazyTestRepo(t)
	assets := make(map[string]*LazyAsset, 256)
	chunkID := util.Hash([]byte("asset"))
	for i := 0; i < 256; i++ {
		path := fmt.Sprintf("assets/%03d.bin", i)
		file := entity.NewFile(path, 5, int64(1000+i))
		assets[path] = &LazyAsset{Path: path, FileID: file.ID, Size: file.Size, Modified: file.Updated,
			Chunks: []string{chunkID}}
	}
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: assets}
	if err := repo.lazyLoader.saveManifest(manifest); nil != err {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repo.Path, 0755); nil != err {
		t.Fatal(err)
	}
	baseline, err := repo.Index("baseline", false, map[string]interface{}{})
	if nil != err {
		t.Fatal(err)
	}
	for _, fileID := range baseline.LazyFiles {
		if err = repo.store.Remove(fileID); nil != err {
			t.Fatal(err)
		}
	}
	fileCache.Clear()
	changed := manifest.Assets["assets/000.bin"]
	changed.Modified += 1000
	changed.FileID = entity.NewFile(changed.Path, changed.Size, changed.Modified).ID
	if err = repo.lazyLoader.saveManifest(manifest); nil != err {
		t.Fatal(err)
	}
	indexed, err := repo.Index("one lazy delta", false, map[string]interface{}{})
	if nil != err {
		t.Fatalf("index read unchanged lazy metadata instead of the manifest baseline: %s", err)
	}
	if indexed.ID == baseline.ID || indexed.LazyManifest == baseline.LazyManifest {
		t.Fatalf("changed manifest did not advance index: baseline=%s/%s indexed=%s/%s", baseline.ID,
			baseline.LazyManifest, indexed.ID, indexed.LazyManifest)
	}
	if _, statErr := repo.store.Stat(changed.FileID); nil != statErr {
		t.Fatalf("changed metadata was not materialized: %v", statErr)
	}
	for _, fileID := range baseline.LazyFiles[1:] {
		if _, statErr := repo.store.Stat(fileID); !os.IsNotExist(statErr) {
			t.Fatalf("unchanged metadata [%s] was unexpectedly rebuilt: %v", fileID, statErr)
		}
	}
}

func TestLazyTombstonePreventsStaleManifestResurrection(t *testing.T) {
	// A stale device may carry a future filesystem mtime. Wall-clock ordering
	// must not let a writer that never observed the tombstone resurrect it.
	asset := &LazyAsset{Path: "assets/deleted.png", FileID: "old", Modified: 9000, Chunks: []string{"chunk"}}
	local := &LazyManifest{Assets: map[string]*LazyAsset{}, Tombstones: map[string]*LazyTombstone{
		asset.Path: {Path: asset.Path, FileID: asset.FileID, DeletedAt: 2000},
	}}
	stale := &LazyManifest{Assets: map[string]*LazyAsset{asset.Path: asset}}
	merged := mergeLazyManifestAssets(local, stale)
	if merged.Assets[asset.Path] != nil {
		t.Fatalf("stale asset was resurrected: %#v", merged.Assets[asset.Path])
	}
	if merged.Tombstones[asset.Path] == nil {
		t.Fatal("tombstone was lost")
	}
}

func TestCloudLazyIndexCannotBypassLocalTombstone(t *testing.T) {
	repo := newLazyTestRepo(t)
	path := "assets/deleted.png"
	manifest := &LazyManifest{Assets: map[string]*LazyAsset{}, Tombstones: map[string]*LazyTombstone{
		path: {Path: path, FileID: "deleted", DeletedAt: 2000},
	}}
	repo.setLazyManifestAsset(manifest, &LazyAsset{Path: path, FileID: "stale", Modified: 9000, Chunks: []string{"chunk"}})
	if manifest.Assets[path] != nil {
		t.Fatalf("cloud index bypassed tombstone: %#v", manifest.Assets[path])
	}
}

func TestLazyAssetCanReviveOnlyAfterObservingTombstone(t *testing.T) {
	path := "assets/recreated.png"
	tombstone := &LazyTombstone{Path: path, FileID: "deleted", DeletedAt: 2000}
	deleted := &LazyManifest{Assets: map[string]*LazyAsset{}, Tombstones: map[string]*LazyTombstone{path: tombstone}}
	revived := &LazyManifest{Assets: map[string]*LazyAsset{path: {
		Path: path, FileID: "new", Modified: 3000, Chunks: []string{"new-chunk"}, RevivesDeletion: tombstone.DeletedAt,
	}}}
	merged := mergeLazyManifestAssets(deleted, revived)
	if merged.Assets[path] == nil || merged.Tombstones[path] != nil {
		t.Fatalf("observed post-delete recreation was not preserved: %#v", merged)
	}

	newerDelete := &LazyManifest{Assets: map[string]*LazyAsset{}, Tombstones: map[string]*LazyTombstone{path: {
		Path: path, FileID: "new", DeletedAt: 4000,
	}}}
	merged = mergeLazyManifestAssets(revived, newerDelete)
	if merged.Assets[path] != nil || merged.Tombstones[path] == nil {
		t.Fatalf("newer tombstone lost to older revival proof: %#v", merged)
	}
}

func TestClearLazyCacheDoesNotChangeCatalogOrCreateTombstone(t *testing.T) {
	repo := newLazyTestRepo(t)
	path := "assets/cache-only.png"
	absPath := filepath.Join(repo.DataPath, path)
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absPath, []byte("cache"), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := &LazyManifest{Version: "1.0", Assets: map[string]*LazyAsset{
		path: {Path: path, FileID: "file", Modified: 1000},
	}}
	if err := repo.lazyLoader.saveManifest(manifest); err != nil {
		t.Fatal(err)
	}
	before := manifest.Updated
	if err := repo.ClearLazyCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(absPath); !os.IsNotExist(err) {
		t.Fatal("cached bytes were not evicted")
	}
	got, err := repo.lazyLoader.getManifest()
	if err != nil {
		t.Fatal(err)
	}
	if got.Assets[path] == nil || len(got.Tombstones) != 0 || got.Updated != before {
		t.Fatalf("cache eviction changed resource truth: %#v", got)
	}
}

func TestProcessLazyRepairQueueUsesTrustedLocalObjectAndBoundsBatch(t *testing.T) {
	repo := newLazyTestRepo(t)
	cloudPath := filepath.Join(t.TempDir(), "cloud")
	if err := os.MkdirAll(cloudPath, 0755); err != nil {
		t.Fatal(err)
	}
	repo.cloud = cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: "repair", RepoPath: repo.Path, Local: &cloud.ConfLocal{Endpoint: cloudPath, ConcurrentReqs: 1}}})
	data := []byte("trusted repair bytes")
	id := util.Hash(data)
	if err := repo.store.PutChunk(&entity.Chunk{ID: id, Data: data}); err != nil {
		t.Fatal(err)
	}
	if err := repo.enqueueLazyRepair(id, "chunk", os.ErrNotExist); err != nil {
		t.Fatal(err)
	}
	secondID := util.Hash([]byte("not available locally"))
	if err := repo.enqueueLazyRepair(secondID, "chunk", os.ErrNotExist); err != nil {
		t.Fatal(err)
	}
	repaired, err := repo.ProcessLazyRepairQueue(1, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 1 {
		t.Fatalf("repaired=%d, want 1", repaired)
	}
	items, err := repo.loadLazyRepairQueue()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != secondID {
		t.Fatalf("bounded remaining queue=%#v", items)
	}
}

func TestVerifyUploadedFilesRejectsChangedChunkClosureWithSameFileID(t *testing.T) {
	repo := newLazyTestRepo(t)
	localCloud := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{
		Dir: "metadata-readback", AvailableSize: 1 << 30,
		Local: &cloud.ConfLocal{Endpoint: t.TempDir(), ConcurrentReqs: 1},
	}})
	repo.cloud = localCloud
	expected := entity.NewFile("assets/readback.png", 7, 1700000000000)
	expected.Chunks = []string{"expected-chunk"}
	if err := repo.store.PutFile(expected); err != nil {
		t.Fatal(err)
	}
	altered := *expected
	altered.Chunks = []string{"different-chunk"}
	data, err := json.Marshal(&altered)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := repo.store.encodeData(data)
	if err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.ToSlash(filepath.Join("objects", expected.ID[:2], expected.ID[2:]))
	if _, err = localCloud.UploadBytes(objectPath, encoded, true); err != nil {
		t.Fatal(err)
	}
	_, verified, apiGets, err := repo.verifyUploadedFiles([]string{expected.ID}, map[string]interface{}{})
	if err == nil || !strings.Contains(err.Error(), "metadata identity mismatch") {
		t.Fatalf("error = %v, want metadata identity mismatch", err)
	}
	if verified != 0 || apiGets != 1 {
		t.Fatalf("verified=%d apiGets=%d, want 0 successful and 1 attempted", verified, apiGets)
	}
}

func TestGetLazyFilesForIndexIsPureForLegacyMetadata(t *testing.T) {
	repo := newLazyTestRepo(t)
	updated := int64(1700000000000)
	canonical := entity.NewFile("assets/legacy.png", 4, updated)
	canonical.Chunks = []string{util.Hash([]byte("data"))}
	if err := repo.lazyLoader.saveManifest(&LazyManifest{Version: "1.0", Assets: map[string]*LazyAsset{
		"/assets/legacy.png": {
			Path: "/assets/legacy.png", FileID: "legacy-id", Size: canonical.Size,
			Modified: updated, Chunks: canonical.Chunks,
		},
	}}); err != nil {
		t.Fatal(err)
	}
	legacyMetadata := &entity.File{ID: "legacy-id", Path: "/assets/legacy.png", Size: 4, Updated: updated,
		Chunks: []string{"chunk"}}
	if err := repo.store.PutFile(legacyMetadata); nil != err {
		t.Fatal(err)
	}
	files, err := repo.getLazyFilesForIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != "legacy-id" || files[0].Path != "/assets/legacy.png" {
		t.Fatalf("legacy lazy metadata changed during read: %#v", files)
	}
	manifest, err := repo.lazyLoader.getManifest()
	if err != nil {
		t.Fatal(err)
	}
	asset := manifest.Assets["/assets/legacy.png"]
	if asset == nil || asset.FileID != "legacy-id" || manifest.Version != lazyManifestFormatLegacy {
		t.Fatalf("manifest changed during index read: %#v", manifest)
	}
	if _, err = repo.store.GetFile(canonical.ID); err == nil {
		t.Fatal("canonical metadata was stored during index read")
	}
	manifestPath := repo.lazyLoader.getManifestPath()
	before, err := os.ReadFile(manifestPath)
	if nil != err {
		t.Fatal(err)
	}
	index, err := repo.Index("pure legacy index", false, map[string]interface{}{})
	if nil != err {
		t.Fatal(err)
	}
	after, err := os.ReadFile(manifestPath)
	if nil != err {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("ordinary index rewrote the legacy manifest")
	}
	if len(index.LazyFiles) != 1 || index.LazyFiles[0] != "legacy-id" {
		t.Fatalf("ordinary index changed legacy identity: %#v", index.LazyFiles)
	}
	if _, err = repo.store.GetFile(canonical.ID); err == nil {
		t.Fatal("ordinary index stored canonical metadata")
	}
}

func TestLazyManifestFormatLegacyDecodeAndCurrentRoundTrip(t *testing.T) {
	legacy := &LazyManifest{}
	if err := json.Unmarshal([]byte(`{"assets":{},"updated":1}`), legacy); nil != err {
		t.Fatal(err)
	}
	if got := lazyManifestFormat(legacy); got != lazyManifestFormatLegacy {
		t.Fatalf("absent format=%s, want %s", got, lazyManifestFormatLegacy)
	}
	current := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{}, Updated: 1}
	data, err := json.Marshal(current)
	if nil != err {
		t.Fatal(err)
	}
	decoded := &LazyManifest{}
	if err = json.Unmarshal(data, decoded); nil != err {
		t.Fatal(err)
	}
	if got := lazyManifestFormat(decoded); got != lazyManifestFormatCurrent {
		t.Fatalf("round-trip format=%s, want %s", got, lazyManifestFormatCurrent)
	}
}

func TestValidateLazyManifestFormatRejectsUnknownFutureFormat(t *testing.T) {
	err := validateLazyManifestFormat(&LazyManifest{Version: "3.0"})
	if nil == err {
		t.Fatal("unknown future format was accepted")
	}
}

func TestCanonicalLazyManifestSupersetRequiresPublishedCatalogAndTombstones(t *testing.T) {
	legacy := &LazyManifest{Assets: map[string]*LazyAsset{"/assets/a": {Path: "/assets/a", Size: 1, Modified: 1000}},
		Tombstones: map[string]*LazyTombstone{"assets/deleted": {Path: "assets/deleted", FileID: "old", DeletedAt: 2000}}}
	candidate := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{},
		Tombstones: map[string]*LazyTombstone{}}
	if isCanonicalLazyManifestSuperset(candidate, legacy) {
		t.Fatal("empty current manifest was accepted as a migrated legacy catalog")
	}
	file := entity.NewFile("assets/a", 1, 1000)
	candidate.Assets[file.Path] = &LazyAsset{Path: file.Path, FileID: file.ID, Size: file.Size, Modified: file.Updated}
	tombstone := *legacy.Tombstones["assets/deleted"]
	candidate.Tombstones["assets/deleted"] = &tombstone
	if !isCanonicalLazyManifestSuperset(candidate, legacy) {
		t.Fatal("canonical manifest containing the legacy catalog was rejected")
	}
}

func TestGetLazyFilesForIndexDoesNotMixChangedLocalStatWithOldChunks(t *testing.T) {
	repo := newLazyTestRepo(t)
	oldUpdated := int64(1700000000000)
	old := entity.NewFile("assets/changed.png", 4, oldUpdated)
	old.Chunks = []string{util.Hash([]byte("old!"))}
	if err := repo.lazyLoader.saveManifest(&LazyManifest{Version: "1.0", Assets: map[string]*LazyAsset{
		old.Path: {Path: old.Path, FileID: old.ID, Size: old.Size, Modified: oldUpdated, Chunks: old.Chunks},
	}}); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(repo.DataPath, old.Path)
	if err := os.WriteFile(localPath, []byte("new content"), 0644); err != nil {
		t.Fatal(err)
	}
	files, err := repo.getLazyFilesForIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != old.ID || files[0].Size != old.Size || files[0].Chunks[0] != old.Chunks[0] {
		t.Fatalf("mixed immutable metadata: %#v, want old manifest %#v", files, old)
	}
}

func TestGetLazyFilesForIndexRejectsDuplicateCanonicalPaths(t *testing.T) {
	repo := newLazyTestRepo(t)
	if err := repo.lazyLoader.saveManifest(&LazyManifest{Version: "1.0", Assets: map[string]*LazyAsset{
		"/assets/duplicate.png": {Path: "/assets/duplicate.png", FileID: "one", Size: 1, Modified: 1000, Chunks: []string{"one"}},
		"assets/duplicate.png":  {Path: "assets/duplicate.png", FileID: "two", Size: 2, Modified: 2000, Chunks: []string{"two"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.getLazyFilesForIndex(); err == nil || !strings.Contains(err.Error(), "duplicate lazy manifest path") {
		t.Fatalf("error = %v, want duplicate canonical path rejection", err)
	}
}

func TestScanLocalAssetsForRepairSkipsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlink requires extra privileges on Windows")
	}

	repo := newLazyTestRepo(t)
	externalFile := filepath.Join(t.TempDir(), "large.bin")
	if err := os.WriteFile(externalFile, []byte("external"), 0644); err != nil {
		t.Fatalf("write external file failed: %s", err)
	}
	linkedAsset := filepath.Join(repo.DataPath, "assets", "linked.bin")
	if err := os.Symlink(externalFile, linkedAsset); err != nil {
		t.Skipf("create symlink failed: %s", err)
	}

	var files []*entity.File
	repairedCount, err := repo.scanLocalAssetsForRepair(&files)
	if err != nil {
		t.Fatalf("scan local assets failed: %s", err)
	}
	if repairedCount != 0 || len(files) != 0 {
		t.Fatalf("symlink should be skipped, repaired=%d files=%#v", repairedCount, files)
	}

	manifest, err := repo.lazyLoader.getManifest()
	if err != nil {
		t.Fatalf("get manifest failed: %s", err)
	}
	if len(manifest.Assets) != 0 {
		t.Fatalf("symlink should not be added to manifest: %#v", manifest.Assets)
	}
}

func TestMergeLazyManifestAssetsUnionsAndNormalizesPaths(t *testing.T) {
	local := &LazyManifest{Assets: map[string]*LazyAsset{
		"assets/local.png": {Path: "assets/local.png", FileID: "local-id", Modified: 1000, Chunks: []string{"local-chunk"}},
		"/assets/same.png": {Path: "/assets/same.png", FileID: "old-id", Modified: 1000, Chunks: []string{"old-chunk"}},
	}}
	cloud := &LazyManifest{Assets: map[string]*LazyAsset{
		"assets/cloud.png": {Path: "assets/cloud.png", FileID: "cloud-id", Modified: 1000, Chunks: []string{"cloud-chunk"}},
		"assets/same.png":  {Path: "assets/same.png", FileID: "new-id", Modified: 2000, Chunks: []string{"new-chunk"}},
	}}

	merged := mergeLazyManifestAssets(local, cloud)

	if len(merged.Assets) != 3 {
		t.Fatalf("unexpected merged assets: %#v", merged.Assets)
	}
	if merged.Assets["assets/same.png"].FileID != "new-id" {
		t.Fatalf("newer asset should win: %#v", merged.Assets["assets/same.png"])
	}
	if _, exists := merged.Assets["/assets/same.png"]; exists {
		t.Fatalf("merged manifest should normalize leading slash keys: %#v", merged.Assets)
	}
	if merged.Assets["assets/local.png"] == nil || merged.Assets["assets/cloud.png"] == nil {
		t.Fatalf("merged manifest should keep unique assets: %#v", merged.Assets)
	}
}

func TestMergeLazyManifestFileWritesMergedManifest(t *testing.T) {
	repo := newLazyTestRepo(t)
	manifestPath := filepath.Join(repo.DataPath, ".siyuan", "lazy_manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0755); err != nil {
		t.Fatalf("mkdir manifest dir failed: %s", err)
	}
	local := &LazyManifest{Assets: map[string]*LazyAsset{
		"assets/local.png": {Path: "assets/local.png", FileID: "local-id", Modified: 1000, Chunks: []string{"local-chunk"}},
	}}
	cloud := &LazyManifest{Assets: map[string]*LazyAsset{
		"assets/cloud.png": {Path: "assets/cloud.png", FileID: "cloud-id", Modified: 1000, Chunks: []string{"cloud-chunk"}},
	}}
	localData, _ := json.Marshal(local)
	cloudData, _ := json.Marshal(cloud)
	if err := os.WriteFile(manifestPath, localData, 0644); err != nil {
		t.Fatalf("write local manifest failed: %s", err)
	}
	if err := os.Chtimes(manifestPath, time.UnixMilli(1000), time.UnixMilli(1000)); err != nil {
		t.Fatalf("chtimes local manifest failed: %s", err)
	}
	localFile := entity.NewFile("/.siyuan/lazy_manifest.json", int64(len(localData)), 1000)
	if err := repo.putFileChunks(localFile, map[string]interface{}{}, 1, 1); err != nil {
		t.Fatalf("put local manifest failed: %s", err)
	}
	if err := os.WriteFile(manifestPath, cloudData, 0644); err != nil {
		t.Fatalf("write cloud manifest failed: %s", err)
	}
	if err := os.Chtimes(manifestPath, time.UnixMilli(2000), time.UnixMilli(2000)); err != nil {
		t.Fatalf("chtimes cloud manifest failed: %s", err)
	}
	cloudFile := entity.NewFile("/.siyuan/lazy_manifest.json", int64(len(cloudData)), 2000)
	if err := repo.putFileChunks(cloudFile, map[string]interface{}{}, 1, 1); err != nil {
		t.Fatalf("put cloud manifest failed: %s", err)
	}

	if err := repo.mergeLazyManifestFile(localFile, cloudFile, map[string]interface{}{}); err != nil {
		t.Fatalf("merge lazy manifest failed: %s", err)
	}

	merged, err := repo.lazyLoader.getManifest()
	if err != nil {
		t.Fatalf("get merged manifest failed: %s", err)
	}
	if merged.Assets["assets/local.png"] == nil || merged.Assets["assets/cloud.png"] == nil {
		t.Fatalf("manifest should contain both sides: %#v", merged.Assets)
	}
}

func TestMergeLazyManifestAssetsPrefersNonEmptyChunks(t *testing.T) {
	local := &LazyManifest{Assets: map[string]*LazyAsset{
		"assets/same.png": {
			Path:     "assets/same.png",
			FileID:   "empty-id",
			Modified: 1000,
		},
	}}
	cloud := &LazyManifest{Assets: map[string]*LazyAsset{
		"assets/same.png": {
			Path:     "assets/same.png",
			FileID:   "chunked-id",
			Modified: 1000,
			Chunks:   []string{"chunk-id"},
		},
	}}

	merged := mergeLazyManifestAssets(local, cloud)

	if merged.Assets["assets/same.png"].FileID != "chunked-id" {
		t.Fatalf("asset with chunks should win: %#v", merged.Assets["assets/same.png"])
	}
}

func TestLocalUpsertFilesUploadsChangedLazySamePath(t *testing.T) {
	repo := newLazyTestRepo(t)

	localLazy := &entity.File{ID: "local-lazy-id", Path: "assets/a.png", Size: 1, Updated: 1000, Chunks: []string{util.Hash([]byte("a"))}}
	cloudLazy := &entity.File{ID: "cloud-lazy-id", Path: "/assets/a.png", Size: 1, Updated: 2000, Chunks: []string{util.Hash([]byte("b"))}}
	localNormal := &entity.File{ID: "local-normal-id", Path: "20260529200000-a/test.sy", Size: 1, Updated: 1000, Chunks: []string{util.Hash([]byte("n"))}}
	putTestFile(t, repo, localLazy, []byte("a"))
	putTestFile(t, repo, cloudLazy, nil)
	putTestFile(t, repo, localNormal, []byte("n"))

	latest := &entity.Index{Files: []string{localNormal.ID}, LazyFiles: []string{localLazy.ID}}
	cloudLatest := &entity.Index{LazyFiles: []string{cloudLazy.ID}}
	upserts, err := repo.localUpsertFiles(latest, cloudLatest, map[string]interface{}{})
	if err != nil {
		t.Fatalf("local upsert files failed: %s", err)
	}

	if len(upserts) != 2 {
		t.Fatalf("unexpected upserts: %#v", upserts)
	}
	upsertIDs := map[string]bool{}
	for _, file := range upserts {
		upsertIDs[file.ID] = true
	}
	if !upsertIDs[localNormal.ID] || !upsertIDs[localLazy.ID] {
		t.Fatalf("unexpected upserts: %#v", upserts)
	}
}

func TestLocalUpsertFilesUploadsEquivalentLazySamePathWithDifferentID(t *testing.T) {
	repo := newLazyTestRepo(t)

	chunkID := util.Hash([]byte("a"))
	localLazy := &entity.File{ID: "local-lazy-id", Path: "assets/a.png", Size: 1, Updated: 1000, Chunks: []string{chunkID}}
	cloudLazy := &entity.File{ID: "cloud-lazy-id", Path: "/assets/a.png", Size: 1, Updated: 1000, Chunks: []string{chunkID}}
	putTestFile(t, repo, localLazy, []byte("a"))
	putTestFile(t, repo, cloudLazy, nil)

	latest := &entity.Index{LazyFiles: []string{localLazy.ID}}
	cloudLatest := &entity.Index{LazyFiles: []string{cloudLazy.ID}}
	upserts, err := repo.localUpsertFiles(latest, cloudLatest, map[string]interface{}{})
	if err != nil {
		t.Fatalf("local upsert files failed: %s", err)
	}

	if len(upserts) != 1 || upserts[0].ID != localLazy.ID {
		t.Fatalf("lazy file with different ID should be uploaded: %#v", upserts)
	}
}

func TestLocalUpsertFilesMarksLazyMissingChunks(t *testing.T) {
	repo := newLazyTestRepo(t)

	localLazy := &entity.File{ID: "local-missing-chunk-id", Path: "assets/missing.png", Size: 1, Updated: 1000, Chunks: []string{"missing-chunk"}}
	if err := repo.store.PutFile(localLazy); err != nil {
		t.Fatalf("put file failed: %s", err)
	}

	latest := &entity.Index{LazyFiles: []string{localLazy.ID}}
	upserts, err := repo.localUpsertFiles(latest, &entity.Index{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("local upsert files failed: %s", err)
	}
	if len(upserts) != 0 {
		t.Fatalf("missing chunk lazy file should not be uploaded: %#v", upserts)
	}

	manifest, err := repo.lazyLoader.getManifest()
	if err != nil {
		t.Fatalf("get manifest failed: %s", err)
	}
	asset := manifest.Assets["assets/missing.png"]
	if asset == nil || asset.Status != LazyStatusError {
		t.Fatalf("missing chunk asset should be marked error: %#v", asset)
	}
}

func TestLocalUpsertFilesRebuildsLazyMissingChunksFromSource(t *testing.T) {
	repo := newLazyTestRepo(t)

	sourcePath := filepath.Join(repo.DataPath, "assets", "rebuild.png")
	sourceData := []byte("rebuilt")
	if err := os.WriteFile(sourcePath, sourceData, 0644); err != nil {
		t.Fatalf("write source failed: %s", err)
	}
	modTime := time.UnixMilli(1000)
	if err := os.Chtimes(sourcePath, modTime, modTime); err != nil {
		t.Fatalf("chtimes source failed: %s", err)
	}

	localLazy := &entity.File{ID: "local-rebuild-id", Path: "assets/rebuild.png", Size: int64(len(sourceData)), Updated: 1000, Chunks: []string{"missing-chunk"}}
	if err := repo.store.PutFile(localLazy); err != nil {
		t.Fatalf("put file failed: %s", err)
	}

	latest := &entity.Index{LazyFiles: []string{localLazy.ID}}
	upserts, err := repo.localUpsertFiles(latest, &entity.Index{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("local upsert files failed: %s", err)
	}
	if len(upserts) != 1 || upserts[0].ID != localLazy.ID {
		t.Fatalf("rebuilt lazy file should be uploaded: %#v", upserts)
	}
	if len(upserts[0].Chunks) == 0 || upserts[0].Chunks[0] == "missing-chunk" {
		t.Fatalf("lazy file chunks should be rebuilt: %#v", upserts[0].Chunks)
	}
	if _, err := repo.store.GetChunk(upserts[0].Chunks[0]); err != nil {
		t.Fatalf("rebuilt chunk should exist: %s", err)
	}

	manifest, err := repo.lazyLoader.getManifest()
	if err != nil {
		t.Fatalf("get manifest failed: %s", err)
	}
	asset := manifest.Assets["assets/rebuild.png"]
	if asset == nil || asset.Status != LazyStatusCached {
		t.Fatalf("rebuilt asset should be cached in manifest: %#v", asset)
	}
}

func TestLocalUpsertFilesKeepsLazyChunksWhenRebuildFails(t *testing.T) {
	repo := newLazyTestRepo(t)

	sourcePath := filepath.Join(repo.DataPath, "assets", "changed.png")
	sourceData := []byte("changed")
	if err := os.WriteFile(sourcePath, sourceData, 0644); err != nil {
		t.Fatalf("write source failed: %s", err)
	}
	modTime := time.UnixMilli(2000)
	if err := os.Chtimes(sourcePath, modTime, modTime); err != nil {
		t.Fatalf("chtimes source failed: %s", err)
	}

	originalChunks := []string{"missing-chunk"}
	localLazy := &entity.File{ID: "local-rebuild-fail-id", Path: "assets/changed.png", Size: int64(len(sourceData)), Updated: 1000, Chunks: originalChunks}
	if err := repo.store.PutFile(localLazy); err != nil {
		t.Fatalf("put file failed: %s", err)
	}

	latest := &entity.Index{LazyFiles: []string{localLazy.ID}}
	upserts, err := repo.localUpsertFiles(latest, &entity.Index{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("local upsert files failed: %s", err)
	}
	if len(upserts) != 0 {
		t.Fatalf("failed rebuild lazy file should not be uploaded: %#v", upserts)
	}

	stored, err := repo.store.GetFile(localLazy.ID)
	if err != nil {
		t.Fatalf("get stored lazy file failed: %s", err)
	}
	if len(stored.Chunks) != len(originalChunks) || stored.Chunks[0] != originalChunks[0] {
		t.Fatalf("failed rebuild should keep original chunks: %#v", stored.Chunks)
	}
}

func TestLocalUpsertFilesDoesNotReadExistingLazyChunks(t *testing.T) {
	repo := newLazyTestRepo(t)

	chunkID := util.Hash([]byte("expected"))
	localLazy := &entity.File{ID: "local-corrupt-existing-id", Path: "assets/existing.png", Size: 1, Updated: 1000, Chunks: []string{chunkID}}
	if err := repo.store.PutFile(localLazy); err != nil {
		t.Fatalf("put file failed: %s", err)
	}
	dir, file := repo.store.AbsPath(chunkID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir chunk dir failed: %s", err)
	}
	encoded, err := repo.store.encodeData([]byte("actual"))
	if err != nil {
		t.Fatalf("encode failed: %s", err)
	}
	if err = os.WriteFile(file, encoded, 0644); err != nil {
		t.Fatalf("write corrupt chunk failed: %s", err)
	}

	latest := &entity.Index{LazyFiles: []string{localLazy.ID}}
	upserts, err := repo.localUpsertFiles(latest, &entity.Index{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("local upsert files failed: %s", err)
	}
	if len(upserts) != 1 || upserts[0].ID != localLazy.ID {
		t.Fatalf("existing lazy chunk should be treated as present without reading: %#v", upserts)
	}
}

func TestLazyLoadAssetRestoresChunksFromCloud(t *testing.T) {
	repo := newLazyTestRepo(t)
	cloudRoot := t.TempDir()
	repo.cloud = cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{
		Dir:      "repo",
		RepoPath: repo.Path,
		Local:    &cloud.ConfLocal{Endpoint: cloudRoot},
	}})

	chunks := [][]byte{[]byte("hello "), []byte("world")}
	var chunkIDs []string
	for _, data := range chunks {
		chunkID := util.Hash(data)
		chunkIDs = append(chunkIDs, chunkID)
		if err := repo.store.PutChunk(&entity.Chunk{ID: chunkID, Data: data}); err != nil {
			t.Fatalf("put chunk failed: %s", err)
		}
		objectPath := filepath.Join("objects", chunkID[:2], chunkID[2:])
		if _, err := repo.cloud.UploadObject(objectPath, false); err != nil {
			t.Fatalf("upload chunk failed: %s", err)
		}
		if err := repo.store.Remove(chunkID); err != nil {
			t.Fatalf("remove local chunk failed: %s", err)
		}
	}

	asset := &LazyAsset{
		Path:     "assets/restored.txt",
		FileID:   "file-id",
		Size:     int64(len("hello world")),
		Modified: time.Now().UnixMilli(),
		Chunks:   chunkIDs,
		Status:   LazyStatusPending,
	}
	if err := repo.lazyLoader.saveManifest(&LazyManifest{
		Version: "1.0",
		Assets:  map[string]*LazyAsset{asset.Path: asset},
	}); err != nil {
		t.Fatalf("save manifest failed: %s", err)
	}

	if err := repo.LoadAssetOnDemand(asset.Path); err != nil {
		t.Fatalf("load asset failed: %s", err)
	}
	data, err := os.ReadFile(filepath.Join(repo.DataPath, asset.Path))
	if err != nil {
		t.Fatalf("read restored asset failed: %s", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("unexpected restored data [%s]", data)
	}
	matches, err := filepath.Glob(filepath.Join(repo.DataPath, "assets", "*.tmp"))
	if err != nil {
		t.Fatalf("glob temp files failed: %s", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp files should be cleaned up: %#v", matches)
	}
}

func TestDownloadCloudChunkRejectsHashMismatch(t *testing.T) {
	repo := newLazyTestRepo(t)
	cloudRoot := t.TempDir()
	repo.cloud = cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{
		Dir:      "repo",
		RepoPath: repo.Path,
		Local:    &cloud.ConfLocal{Endpoint: cloudRoot},
	}})

	chunkID := util.Hash([]byte("expected"))
	encoded, err := repo.store.encodeData([]byte("actual"))
	if err != nil {
		t.Fatalf("encode failed: %s", err)
	}
	objectPath := filepath.Join(cloudRoot, "repo", "objects", chunkID[:2], chunkID[2:])
	if err = os.MkdirAll(filepath.Dir(objectPath), 0755); err != nil {
		t.Fatalf("mkdir cloud object failed: %s", err)
	}
	if err = os.WriteFile(objectPath, encoded, 0644); err != nil {
		t.Fatalf("write cloud object failed: %s", err)
	}

	_, chunk, err := repo.downloadCloudChunk(chunkID, 1, 1, map[string]interface{}{})
	if err == nil {
		t.Fatalf("download cloud chunk should reject hash mismatch: %#v", chunk)
	}
}

func newLazyTestRepo(t testing.TB) *Repo {
	t.Helper()

	root := t.TempDir()
	aesKey, err := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	if err != nil {
		t.Fatalf("kdf failed: %s", err)
	}
	repo, err := NewRepoWithLazyLoad(
		filepath.Join(root, "data"),
		filepath.Join(root, "repo"),
		filepath.Join(root, "history"),
		filepath.Join(root, "temp"),
		deviceID, deviceName, deviceOS, aesKey, ignoreLines(), nil, true)
	if err != nil {
		t.Fatalf("new repo failed: %s", err)
	}
	if err = os.MkdirAll(filepath.Join(root, "data", "assets"), 0755); err != nil {
		t.Fatalf("mkdir assets failed: %s", err)
	}
	return repo
}

func putTestFile(t *testing.T, repo *Repo, file *entity.File, chunkData []byte) {
	t.Helper()
	if err := repo.store.PutFile(file); err != nil {
		t.Fatalf("put file failed: %s", err)
	}
	if len(file.Chunks) == 0 || chunkData == nil {
		return
	}
	if err := repo.store.PutChunk(&entity.Chunk{ID: file.Chunks[0], Data: chunkData}); err != nil {
		t.Fatalf("put chunk failed: %s", err)
	}
}

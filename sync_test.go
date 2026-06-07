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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/dejavu/util"
	"github.com/siyuan-note/encryption"
)

func TestSync(t *testing.T) {
	repo, _ := initIndex(t)

	userId := "0"
	token := ""

	return // 注释掉不跑

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

func TestDiffUpsertRemoveNormalizesLazyAssetPaths(t *testing.T) {
	repo := newLazyTestRepo(t)

	left := []*entity.File{{ID: "left-id", Path: "/assets/a.png", Size: 1, Updated: 1000}}
	right := []*entity.File{{ID: "right-id", Path: "assets/a.png", Size: 1, Updated: 1000}}

	upserts, removes := repo.diffUpsertRemove(left, right, false)
	if len(upserts) != 0 || len(removes) != 0 {
		t.Fatalf("equivalent lazy asset paths should not diff, upserts=%#v removes=%#v", upserts, removes)
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

func newLazyTestRepo(t *testing.T) *Repo {
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

// DejaVu - Data snapshot and sync.
// Copyright (c) 2022-present, b3log.org
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package dejavu

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/siyuan-note/dejavu/entity"
)

func TestLazyManifestFilesForSanitizeAvoidsMetadataReads(t *testing.T) {
	const count = 100000
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: make(map[string]*LazyAsset, count)}
	index := &entity.Index{ID: "current", LazyManifest: "manifest", LazyFiles: make([]string, 0, count)}
	for i := 0; i < count; i++ {
		path := fmt.Sprintf("assets/performance/%06d.bin", i)
		file := entity.NewFile(path, int64(i+1), int64(i+2))
		asset := &LazyAsset{Path: path, FileID: file.ID, Size: file.Size, Modified: file.Updated,
			Chunks: []string{fmt.Sprintf("%040d", i)}}
		manifest.Assets[path] = asset
		index.LazyFiles = append(index.LazyFiles, file.ID)
	}
	repo := &Repo{DataPath: filepath.Join(t.TempDir(), "data"), lazyLoadEnabled: true}
	repo.lazyLoader = NewLazyLoader(repo)
	repo.lazyLoader.manifest = manifest

	start := time.Now()
	files, usable, err := repo.lazyManifestFilesForSanitize(index)
	if nil != err {
		t.Fatal(err)
	}
	if !usable || count != len(files) {
		t.Fatalf("manifest fast path unusable [usable=%v, files=%d]", usable, len(files))
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("100k manifest sanitize took too long: %s", elapsed)
	}
}

func TestLazyManifestFilesForExactCloudIndexAvoidsMetadataObjects(t *testing.T) {
	const count = 100000
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: make(map[string]*LazyAsset, count)}
	index := &entity.Index{LazyManifest: "manifest", LazyFiles: make([]string, 0, count)}
	for i := 0; i < count; i++ {
		path := fmt.Sprintf("assets/cloud/%06d.bin", i)
		file := entity.NewFile(path, int64(i+1), int64(i+2))
		manifest.Assets[path] = &LazyAsset{Path: path, FileID: file.ID, Size: file.Size, Modified: file.Updated,
			Chunks: []string{fmt.Sprintf("%040d", i)}}
		index.LazyFiles = append(index.LazyFiles, file.ID)
	}

	start := time.Now()
	files, usable, err := lazyManifestFilesForExactIndex(manifest, index)
	if nil != err || !usable || count != len(files) {
		t.Fatalf("cloud manifest fast path failed [usable=%v, files=%d, err=%v]", usable, len(files), err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("100k cloud manifest metadata took too long: %s", elapsed)
	}
}

func TestLazyManifestFilesForExactCloudIndexRejectsMismatchedClosure(t *testing.T) {
	file := entity.NewFile("assets/cloud/exact.bin", 1, 2)
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{
		file.Path: {Path: file.Path, FileID: file.ID, Size: file.Size, Modified: file.Updated, Chunks: []string{"chunk"}},
	}}
	for _, index := range []*entity.Index{
		{LazyManifest: "manifest", LazyFiles: []string{"missing"}},
		{LazyManifest: "manifest", LazyFiles: []string{file.ID, file.ID}},
	} {
		if _, usable, err := lazyManifestFilesForExactIndex(manifest, index); nil != err || usable {
			t.Fatalf("mismatched cloud closure accepted [ids=%v, usable=%v, err=%v]", index.LazyFiles, usable, err)
		}
	}
}

func TestIndexPublishesExactLazyManifestClosureForNewAsset(t *testing.T) {
	repo := newLazyTestRepo(t)
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{}}
	if err := repo.lazyLoader.saveManifest(manifest); nil != err {
		t.Fatal(err)
	}
	assetPath := filepath.Join(repo.DataPath, "assets", "new.png")
	if err := os.MkdirAll(filepath.Dir(assetPath), 0755); nil != err {
		t.Fatal(err)
	}
	if err := os.WriteFile(assetPath, []byte("new lazy asset"), 0644); nil != err {
		t.Fatal(err)
	}

	index, err := repo.Index("new lazy asset", true, map[string]interface{}{})
	if nil != err {
		t.Fatal(err)
	}
	published, err := repo.lazyLoader.getManifest()
	if nil != err {
		t.Fatal(err)
	}
	files, usable, err := lazyManifestFilesForExactIndex(published, index)
	if nil != err || !usable || 1 != len(files) {
		t.Fatalf("new lazy asset published a mismatched manifest closure [usable=%v, files=%d, err=%v]", usable,
			len(files), err)
	}
	if files[0].Path != "assets/new.png" || index.LazyFiles[0] != files[0].ID {
		t.Fatalf("new lazy asset was not canonicalized [file=%#v, ids=%v]", files[0], index.LazyFiles)
	}
}

func TestCloudLazyFilesForSyncFallsBackWithoutCurrentManifest(t *testing.T) {
	repo := newLazyTestRepo(t)
	file := entity.NewFile("assets/cloud/legacy.bin", 3, 4)
	file.Chunks = []string{"legacy-chunk"}
	if err := repo.store.PutFile(file); nil != err {
		t.Fatal(err)
	}
	files, manifestBacked, err := repo.cloudLazyFilesForSync(&entity.Index{LazyFiles: []string{file.ID}},
		map[string]interface{}{})
	if nil != err {
		t.Fatal(err)
	}
	if manifestBacked || 1 != len(files) || file.ID != files[0].ID {
		t.Fatalf("legacy object fallback failed [manifestBacked=%v, files=%v]", manifestBacked, files)
	}
}

func TestLazyManifestFilesForSanitizeRequiresExactIndexClosure(t *testing.T) {
	path := "assets/exact.bin"
	file := entity.NewFile(path, 1, 2)
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{
		path: {Path: path, FileID: file.ID, Size: file.Size, Modified: file.Updated, Chunks: []string{"chunk"}},
	}}
	repo := &Repo{DataPath: filepath.Join(t.TempDir(), "data"), lazyLoadEnabled: true}
	repo.lazyLoader = NewLazyLoader(repo)
	repo.lazyLoader.manifest = manifest

	if _, usable, err := repo.lazyManifestFilesForSanitize(&entity.Index{LazyManifest: "manifest", LazyFiles: []string{file.ID}}); nil != err || !usable {
		t.Fatalf("matching closure rejected [usable=%v, err=%v]", usable, err)
	}
	if _, usable, err := repo.lazyManifestFilesForSanitize(&entity.Index{LazyManifest: "manifest", LazyFiles: []string{file.ID, file.ID}}); nil != err || usable {
		t.Fatalf("mismatched closure accepted [usable=%v, err=%v]", usable, err)
	}
}

func TestManifestBackedSanitizeStillFiltersProtectedFile(t *testing.T) {
	repo := newLazyTestRepo(t)
	protectedPath := normalizeLazyPath(refUsedRepoPath)
	protected := entity.NewFile(protectedPath, 1, 2)
	keep := entity.NewFile("assets/keep.bin", 1, 2)
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{
		protected.Path: {Path: protected.Path, FileID: protected.ID, Size: protected.Size, Modified: protected.Updated, Chunks: []string{"protected-chunk"}},
		keep.Path:      {Path: keep.Path, FileID: keep.ID, Size: keep.Size, Modified: keep.Updated, Chunks: []string{"keep-chunk"}},
	}}
	repo.lazyLoader.manifest = manifest
	if err := repo.store.PutFile(keep); nil != err {
		t.Fatal(err)
	}
	index := &entity.Index{ID: "manifest-protected", LazyManifest: "manifest", LazyFiles: []string{protected.ID, keep.ID}}
	cleaned, changed, err := repo.sanitizeStoredIndex(index, "test sanitize")
	if nil != err {
		t.Fatal(err)
	}
	if !changed || 1 != len(cleaned.LazyFiles) || keep.ID != cleaned.LazyFiles[0] {
		t.Fatalf("protected lazy file was not removed: %#v", cleaned.LazyFiles)
	}
}

func TestSanitizeStoredIndexUsesManifestWithoutLazyObjects(t *testing.T) {
	repo := newLazyTestRepo(t)
	const count = 100000
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: make(map[string]*LazyAsset, count)}
	index := &entity.Index{ID: "manifest-only", LazyManifest: "manifest", LazyFiles: make([]string, 0, count)}
	for i := 0; i < count; i++ {
		path := fmt.Sprintf("assets/performance/%06d.bin", i)
		file := entity.NewFile(path, int64(i+1), int64(i+2))
		manifest.Assets[path] = &LazyAsset{Path: path, FileID: file.ID, Size: file.Size, Modified: file.Updated,
			Chunks: []string{fmt.Sprintf("%040d", i)}}
		index.LazyFiles = append(index.LazyFiles, file.ID)
	}
	repo.lazyLoader.manifest = manifest
	if err := os.MkdirAll(repo.Path, 0755); nil != err {
		t.Fatal(err)
	}

	start := time.Now()
	cleaned, changed, err := repo.sanitizeStoredIndex(index, "test manifest fast path")
	if nil != err {
		t.Fatal(err)
	}
	if changed || cleaned != index {
		t.Fatalf("unchanged manifest index was rewritten [changed=%v]", changed)
	}
	// 该测试在竞态检测下包含 10 万项映射和安全写盘，保留宽松上限避免把检测器开销误判为性能回归。
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("100k full sanitize took too long: %s", elapsed)
	}
	marker, err := os.ReadFile(filepath.Join(repo.Path, "sanitized-current-index"))
	if nil != err || string(marker) != "1:"+index.ID {
		t.Fatalf("sanitize marker not written [marker=%q, err=%v]", marker, err)
	}
}

func TestSanitizeStoredIndexFallsBackForInvalidManifest(t *testing.T) {
	repo := newLazyTestRepo(t)
	file := entity.NewFile("assets/from-object.bin", 3, 4)
	if err := repo.store.PutFile(file); nil != err {
		t.Fatal(err)
	}
	// 错误的键与路径组合必须使清单快路失效，清理逻辑应继续读取不可变对象。
	repo.lazyLoader.manifest = &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{
		"assets/wrong.bin": {Path: file.Path, FileID: file.ID, Size: file.Size, Modified: file.Updated},
	}}
	index := &entity.Index{ID: "invalid-manifest", LazyManifest: "manifest", LazyFiles: []string{file.ID}}
	cleaned, changed, err := repo.sanitizeStoredIndex(index, "test object fallback")
	if nil != err {
		t.Fatal(err)
	}
	if changed || cleaned != index {
		t.Fatalf("object fallback changed clean index [changed=%v]", changed)
	}
}

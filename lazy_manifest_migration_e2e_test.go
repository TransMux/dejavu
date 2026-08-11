// DejaVu - Data snapshot and sync.
// Copyright (c) 2022-present b3log.org
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package dejavu

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/dejavu/util"
	"github.com/siyuan-note/encryption"
)

type migrationUploadBarrierCloud struct {
	cloud.Cloud
	ready   chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (c *migrationUploadBarrierCloud) UploadObject(path string, overwrite bool) (length int64, err error) {
	if lockSyncKey != path {
		c.once.Do(func() {
			close(c.ready)
			<-c.proceed
		})
	}
	return c.Cloud.UploadObject(path, overwrite)
}

type migrationLockObserverCloud struct {
	cloud.Cloud
	observed chan struct{}
	proceed  chan struct{}
	once     sync.Once
}

func (c *migrationLockObserverCloud) DownloadObject(path string) (data []byte, err error) {
	data, err = c.Cloud.DownloadObject(path)
	if lockSyncKey == path && nil == err {
		c.once.Do(func() {
			close(c.observed)
			<-c.proceed
		})
	}
	return
}

func TestLazyManifestRepositoryMigrationPropagatesAcrossTwoRepos(t *testing.T) {
	root := t.TempDir()
	cloudEndpoint := filepath.Join(root, "cloud")
	key, err := encryption.KDF("lazy-manifest-migration-password", "lazy-manifest-migration-salt")
	if nil != err {
		t.Fatal(err)
	}
	newRepo := func(name string, backend cloud.Cloud) *Repo {
		if nil == backend {
			backend = cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: "lazy-manifest-migration", AvailableSize: 1 << 40,
				Local: &cloud.ConfLocal{Endpoint: cloudEndpoint, ConcurrentReqs: 1}}})
		}
		base := filepath.Join(root, name)
		repo, newErr := NewRepoWithLazyLoad(filepath.Join(base, "data"), filepath.Join(base, "repo"),
			filepath.Join(base, "history"), filepath.Join(base, "temp"), name, name, runtime.GOOS, key, nil, backend, true)
		if nil != newErr {
			t.Fatal(newErr)
		}
		return repo
	}

	a := newRepo("device-a", nil)
	if err = os.MkdirAll(filepath.Join(a.DataPath, "assets"), 0755); nil != err {
		t.Fatal(err)
	}
	data := []byte("shared lazy bytes")
	chunkID := util.Hash(data)
	updated := time.Unix(1700000000, 0).UnixMilli()
	legacyFile := &entity.File{ID: "legacy-metadata-id", Path: "/assets/shared.bin", Size: int64(len(data)), Updated: updated,
		Chunks: []string{chunkID}}
	putTestFile(t, a, legacyFile, data)
	if err = a.lazyLoader.saveManifest(&LazyManifest{Version: lazyManifestFormatLegacy, Assets: map[string]*LazyAsset{
		legacyFile.Path: {Path: legacyFile.Path, FileID: legacyFile.ID, Size: legacyFile.Size, Modified: legacyFile.Updated,
			Chunks: legacyFile.Chunks},
	}}); nil != err {
		t.Fatal(err)
	}
	baseline, err := a.Index("legacy baseline", false, map[string]interface{}{})
	if nil != err {
		t.Fatal(err)
	}
	if _, _, err = a.Sync(map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	cloudBaseline, err := a.GetCloudLatest(map[string]interface{}{})
	if nil != err || cloudBaseline.ID != baseline.ID {
		t.Fatalf("legacy cloud baseline=%v err=%v", cloudBaseline, err)
	}

	b := newRepo("device-b", nil)
	if err = os.MkdirAll(filepath.Join(b.DataPath, "assets"), 0755); nil != err {
		t.Fatal(err)
	}
	putTestFile(t, b, legacyFile, data)
	// 接收端仍保留独立的旧版清单，不能预先拥有发布端迁移生成的规范元数据。
	if err = b.lazyLoader.saveManifest(&LazyManifest{Version: lazyManifestFormatLegacy, Assets: map[string]*LazyAsset{
		legacyFile.Path: {Path: legacyFile.Path, FileID: legacyFile.ID, Size: legacyFile.Size, Modified: legacyFile.Updated,
			Chunks: legacyFile.Chunks},
	}}); nil != err {
		t.Fatal(err)
	}
	receiverManifestTime := time.Unix(1700000020, 0)
	if err = os.Chtimes(b.lazyLoader.getManifestPath(), receiverManifestTime, receiverManifestTime); nil != err {
		t.Fatal(err)
	}
	if _, err = b.Index("independent receiver baseline", false, map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	localDeltaPath := filepath.Join(b.DataPath, "assets", "receiver-only.txt")
	if err = os.WriteFile(localDeltaPath, []byte("receiver delta"), 0644); nil != err {
		t.Fatal(err)
	}
	localDeltaTime := time.Unix(1700000010, 0)
	if err = os.Chtimes(localDeltaPath, localDeltaTime, localDeltaTime); nil != err {
		t.Fatal(err)
	}
	if _, err = b.Index("receiver local delta", false, map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	canonical := entity.NewFile("assets/shared.bin", legacyFile.Size, legacyFile.Updated)
	canonical.Chunks = legacyFile.Chunks
	canonicalObject := filepath.Join(b.Path, "objects", canonical.ID[:2], canonical.ID[2:])
	if err = os.Remove(canonicalObject); nil != err && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	fileCache.Clear()

	// 模拟已升级客户端的新建本地清单带有新版标记，迁移仍必须由云端已发布的旧版格式驱动。
	if err = a.saveLazyManifestWithNewIdentity(&LazyManifest{Version: lazyManifestFormatCurrent,
		Assets: map[string]*LazyAsset{}}); nil != err {
		t.Fatal(err)
	}

	failing := &failOnceObjectCloud{Cloud: a.cloud, failAt: 1}
	a.cloud = failing
	if _, _, err = a.Sync(map[string]interface{}{}); nil == err {
		t.Fatal("expected migration upload interruption")
	}
	cloudAfterFailure, latestErr := a.GetCloudLatest(map[string]interface{}{})
	if nil != latestErr || cloudAfterFailure.ID != baseline.ID {
		t.Fatalf("failed migration published latest=%v err=%v, want %s", cloudAfterFailure, latestErr, baseline.ID)
	}

	failing.mu.Lock()
	failing.failAt = 0
	failing.mu.Unlock()

	previousProcessLock := lockSyncProcess
	previousRetryDelay := cloudLockRetryDelay
	lockSyncProcess = func() func() { return func() {} }
	cloudLockRetryDelay = 0
	defer func() {
		lockSyncProcess = previousProcessLock
		cloudLockRetryDelay = previousRetryDelay
	}()
	publisherBarrier := &migrationUploadBarrierCloud{Cloud: failing, ready: make(chan struct{}), proceed: make(chan struct{})}
	receiverBarrier := &migrationLockObserverCloud{Cloud: b.cloud, observed: make(chan struct{}), proceed: make(chan struct{})}
	a.cloud = publisherBarrier
	b.cloud = receiverBarrier
	publisherResult := make(chan error, 1)
	go func() {
		_, _, syncErr := a.Sync(map[string]interface{}{})
		publisherResult <- syncErr
	}()
	<-publisherBarrier.ready
	receiverResult := make(chan error, 1)
	go func() {
		_, _, syncErr := b.Sync(map[string]interface{}{})
		receiverResult <- syncErr
	}()
	<-receiverBarrier.observed
	select {
	case syncErr := <-receiverResult:
		t.Fatalf("receiver completed while publisher still held the cloud lock: %v", syncErr)
	default:
	}
	close(publisherBarrier.proceed)
	if err = <-publisherResult; nil != err {
		t.Fatal(err)
	}
	if got := a.lazyManifestMigrations.Load(); 1 != got {
		t.Fatalf("publisher migration count=%d, want 1", got)
	}
	published, err := a.GetCloudLatest(map[string]interface{}{})
	if nil != err || published.ID == baseline.ID {
		t.Fatalf("migration was not published: latest=%v err=%v", published, err)
	}
	manifestA, err := a.lazyLoader.getManifest()
	if nil != err || manifestA.Version != lazyManifestFormatCurrent {
		t.Fatalf("publisher manifest=%v err=%v", manifestA, err)
	}
	if _, statErr := os.Stat(canonicalObject); !os.IsNotExist(statErr) {
		t.Fatalf("independent receiver unexpectedly contains canonical metadata [%s]", canonical.ID)
	}
	if asset := manifestA.Assets[canonical.Path]; nil == asset || asset.FileID != canonical.ID {
		t.Fatalf("publisher did not canonicalize manifest: %#v", manifestA.Assets)
	}
	publishedManifest, err := a.readPublishedLazyManifest(published, map[string]interface{}{})
	if nil != err || lazyManifestFormatCurrent != lazyManifestFormat(publishedManifest) {
		t.Fatalf("cloud did not publish current manifest: manifest=%v err=%v", publishedManifest, err)
	}
	publishedAsset := publishedManifest.Assets[canonical.Path]
	if nil == publishedAsset || publishedAsset.FileID != canonical.ID ||
		len(publishedAsset.Chunks) != 1 || publishedAsset.Chunks[0] != chunkID {
		t.Fatalf("cloud manifest closure is not canonical: %#v", publishedManifest.Assets)
	}
	cloudMetadata, err := a.getFilesWithCloudFallback([]string{canonical.ID}, map[string]interface{}{})
	if nil != err || 1 != len(cloudMetadata) || cloudMetadata[0].Path != canonical.Path ||
		len(cloudMetadata[0].Chunks) != 1 || cloudMetadata[0].Chunks[0] != chunkID {
		t.Fatalf("cloud canonical metadata is incomplete: files=%#v err=%v", cloudMetadata, err)
	}
	select {
	case syncErr := <-receiverResult:
		t.Fatalf("receiver bypassed the cloud-lock observation barrier: %v", syncErr)
	default:
	}

	fileCache.Clear()
	close(receiverBarrier.proceed)
	if err = <-receiverResult; !errors.Is(err, ErrCloudLocked) {
		t.Fatalf("receiver lock result=%v, want ErrCloudLocked", err)
	}
	if _, _, err = b.Sync(map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	if got := b.lazyManifestMigrations.Load(); 0 != got {
		t.Fatalf("receiver performed full migration %d times", got)
	}
	if _, statErr := os.Stat(canonicalObject); nil != statErr {
		t.Fatalf("receiver did not hydrate canonical metadata: %v", statErr)
	}
	marker := filepath.Join(b.Path, lazyManifestHydratedMarker)
	markerInfo, statErr := os.Stat(marker)
	if nil != statErr {
		t.Fatalf("receiver hydration marker missing: %v", statErr)
	}
	manifestB, err := b.lazyLoader.getManifest()
	if nil != err || manifestB.Version != lazyManifestFormatCurrent {
		t.Fatalf("receiver manifest=%v err=%v", manifestB, err)
	}
	if asset := manifestB.Assets[canonical.Path]; nil == asset || asset.FileID != canonical.ID {
		t.Fatalf("receiver did not consume canonical manifest: %#v", manifestB.Assets)
	}
	if nil == manifestB.Assets["assets/receiver-only.txt"] {
		t.Fatalf("receiver local delta was lost: %#v", manifestB.Assets)
	}
	bLatest, err := b.Latest()
	if nil != err || bLatest.ID == published.ID {
		t.Fatalf("receiver local delta did not produce merged latest=%v err=%v", bLatest, err)
	}
	cloudAfterReceiver, err := b.GetCloudLatest(map[string]interface{}{})
	if nil != err || cloudAfterReceiver.ID != bLatest.ID {
		t.Fatalf("receiver merge was not published: cloud=%v err=%v", cloudAfterReceiver, err)
	}
	if _, _, err = b.Sync(map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	markerAfterSecond, statErr := os.Stat(marker)
	if nil != statErr || !markerAfterSecond.ModTime().Equal(markerInfo.ModTime()) {
		t.Fatalf("second receiver sync repeated hydration: before=%v after=%v err=%v", markerInfo, markerAfterSecond, statErr)
	}
	bSecond, err := b.Latest()
	if nil != err || bSecond.ID != bLatest.ID {
		t.Fatalf("second receiver sync changed latest=%v err=%v", bSecond, err)
	}

	// 全新接收端没有旧清单或元数据对象，也必须能在启动索引后消费已发布仓库并保持二次同步幂等。
	c := newRepo("device-c", nil)
	if err = os.MkdirAll(c.DataPath, 0755); nil != err {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.DataPath, "bootstrap.txt"), []byte("blank receiver"), 0644); nil != err {
		t.Fatal(err)
	}
	if _, err = c.Index("blank receiver baseline", false, map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	if _, _, err = c.Sync(map[string]interface{}{}); nil != err {
		t.Fatalf("blank receiver sync failed: %v", err)
	}
	cFirst, err := c.Latest()
	if nil != err {
		t.Fatal(err)
	}
	cManifest, err := c.lazyLoader.getManifest()
	if nil != err || lazyManifestFormatCurrent != lazyManifestFormat(cManifest) ||
		nil == cManifest.Assets[canonical.Path] {
		t.Fatalf("blank receiver did not consume published manifest: manifest=%#v err=%v", cManifest, err)
	}
	if _, _, err = c.Sync(map[string]interface{}{}); nil != err {
		t.Fatalf("blank receiver second sync failed: %v", err)
	}
	cSecond, err := c.Latest()
	if nil != err || cSecond.ID != cFirst.ID {
		t.Fatalf("blank receiver second sync changed latest=%v first=%v err=%v", cSecond, cFirst, err)
	}
}

func TestHydrateLazyManifestMetadataRebuildsMissingAndReidentifiesConflictingObjects(t *testing.T) {
	root := t.TempDir()
	key, err := encryption.KDF("lazy-manifest-hydration-password", "lazy-manifest-hydration-salt")
	if nil != err {
		t.Fatal(err)
	}
	backend := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: "lazy-manifest-hydration", AvailableSize: 1 << 40,
		Local: &cloud.ConfLocal{Endpoint: filepath.Join(root, "cloud"), ConcurrentReqs: 1}}})
	repo, err := NewRepoWithLazyLoad(filepath.Join(root, "data"), filepath.Join(root, "repo"), filepath.Join(root, "history"),
		filepath.Join(root, "temp"), "receiver", "receiver", runtime.GOOS, key, nil, backend, true)
	if nil != err {
		t.Fatal(err)
	}
	file := entity.NewFile("assets/missing.bin", 7, 1700000000000)
	file.Chunks = []string{util.Hash([]byte("missing"))}
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{
		file.Path: {Path: file.Path, FileID: file.ID, Size: file.Size, Modified: file.Updated, Chunks: file.Chunks},
	}}
	if repaired, hydrateErr := repo.hydrateLazyManifestMetadata(manifest, map[string]interface{}{}); nil != hydrateErr || repaired != 1 {
		t.Fatalf("rebuild missing metadata repaired=%d err=%v", repaired, hydrateErr)
	}
	conflicting := entity.NewFile(file.Path, file.Size, file.Updated)
	conflicting.Chunks = []string{util.Hash([]byte("stale"))}
	_, conflictingPath := repo.store.AbsPath(conflicting.ID)
	if err = os.Remove(conflictingPath); nil != err {
		t.Fatal(err)
	}
	fileCache.Del(repo.store.fileCacheKey(conflicting.ID))
	if err = repo.store.PutFile(conflicting); nil != err {
		t.Fatal(err)
	}
	oldID := file.ID
	if changed, canonicalErr := repo.canonicalizeLazyManifestObjectIdentities(manifest); nil != canonicalErr || changed != 1 {
		t.Fatalf("reidentify conflicting metadata changed=%d err=%v", changed, canonicalErr)
	}
	if manifest.Assets[file.Path].FileID == oldID {
		t.Fatal("conflicting metadata retained the historical object ID")
	}
	historical, err := repo.store.GetFile(oldID)
	if nil != err || !slices.Equal(historical.Chunks, conflicting.Chunks) {
		t.Fatalf("historical metadata was overwritten: file=%#v err=%v", historical, err)
	}
	repaired, err := repo.store.GetFile(manifest.Assets[file.Path].FileID)
	if nil != err || !slices.Equal(repaired.Chunks, file.Chunks) {
		t.Fatalf("manifest metadata was not authoritative: file=%#v err=%v", repaired, err)
	}
}

func TestPrepareLocalLazyManifestClosureRepairsLegacyPathIdentityBeforeValidation(t *testing.T) {
	root := t.TempDir()
	key, err := encryption.KDF("legacy-path-identity-password", "legacy-path-identity-salt")
	if nil != err {
		t.Fatal(err)
	}
	backend := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: "legacy-path-identity", AvailableSize: 1 << 40,
		Local: &cloud.ConfLocal{Endpoint: filepath.Join(root, "cloud"), ConcurrentReqs: 1}}})
	repo, err := NewRepoWithLazyLoad(filepath.Join(root, "data"), filepath.Join(root, "repo"), filepath.Join(root, "history"),
		filepath.Join(root, "temp"), "receiver", "receiver", runtime.GOOS, key, nil, backend, true)
	if nil != err {
		t.Fatal(err)
	}
	legacy := entity.NewFile("/assets/legacy.png", 8, 1700003000000)
	legacy.Chunks = []string{util.Hash([]byte("legacy"))}
	if err = repo.store.PutFile(legacy); nil != err {
		t.Fatal(err)
	}
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{
		"assets/legacy.png": {Path: "assets/legacy.png", FileID: legacy.ID, Size: legacy.Size, Modified: legacy.Updated,
			Chunks: append([]string(nil), legacy.Chunks...)},
	}}
	if err = repo.lazyLoader.saveManifest(manifest); nil != err {
		t.Fatal(err)
	}
	if err = repo.prepareLocalLazyManifestClosure(map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	repaired, err := repo.lazyLoader.getManifest()
	if nil != err {
		t.Fatal(err)
	}
	asset := repaired.Assets["assets/legacy.png"]
	expected := entity.NewFile(asset.Path, asset.Size, asset.Modified)
	if asset.FileID != expected.ID || asset.FileID == legacy.ID {
		t.Fatalf("legacy identity was not repaired: asset=%#v expected=%s", asset, expected.ID)
	}
	historical, err := repo.store.GetFile(legacy.ID)
	if nil != err || historical.Path != legacy.Path {
		t.Fatalf("historical object changed: file=%#v err=%v", historical, err)
	}
	if err = repo.prepareLocalLazyManifestClosure(map[string]interface{}{}); nil != err {
		t.Fatalf("second preparation should be idempotent: %v", err)
	}
	incoming := entity.NewFile("/assets/new.png", 3, 1700003001000)
	incoming.Chunks = []string{util.Hash([]byte("new"))}
	if err = repo.updateLazyManifest([]*entity.File{incoming}); nil != err {
		t.Fatal(err)
	}
	updated, err := repo.lazyLoader.getManifest()
	if nil != err {
		t.Fatal(err)
	}
	created := updated.Assets["assets/new.png"]
	want := entity.NewFile("assets/new.png", incoming.Size, incoming.Updated)
	if nil == created || created.Path != want.Path || created.FileID != want.ID {
		t.Fatalf("new manifest identity was not generated from normalized path: %#v", created)
	}
}

func TestPrepareLocalLazyManifestClosureRejectsUnprovenIdentityMismatch(t *testing.T) {
	root := t.TempDir()
	key, err := encryption.KDF("tampered-identity-password", "tampered-identity-salt")
	if nil != err {
		t.Fatal(err)
	}
	backend := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: "tampered-identity", AvailableSize: 1 << 40,
		Local: &cloud.ConfLocal{Endpoint: filepath.Join(root, "cloud"), ConcurrentReqs: 1}}})
	repo, err := NewRepoWithLazyLoad(filepath.Join(root, "data"), filepath.Join(root, "repo"), filepath.Join(root, "history"),
		filepath.Join(root, "temp"), "receiver", "receiver", runtime.GOOS, key, nil, backend, true)
	if nil != err {
		t.Fatal(err)
	}
	legacy := entity.NewFile("/assets/tampered.png", 8, 1700003000000)
	legacy.Chunks = []string{util.Hash([]byte("historical"))}
	if err = repo.store.PutFile(legacy); nil != err {
		t.Fatal(err)
	}
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{
		"assets/tampered.png": {Path: "assets/tampered.png", FileID: legacy.ID, Size: legacy.Size, Modified: legacy.Updated,
			Chunks: []string{util.Hash([]byte("tampered"))}},
	}}
	if err = repo.lazyLoader.saveManifest(manifest); nil != err {
		t.Fatal(err)
	}
	if err = repo.prepareLocalLazyManifestClosure(map[string]interface{}{}); nil == err ||
		!strings.Contains(err.Error(), "file ID mismatch") {
		t.Fatalf("unproven identity mismatch was not rejected: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(repo.Path, lazyManifestLocalClosureMarker)); !os.IsNotExist(statErr) {
		t.Fatalf("tampered manifest wrote completion marker: %v", statErr)
	}
	manifest.Assets["assets/tampered.png"].FileID = "short"
	if err = repo.lazyLoader.saveManifest(manifest); nil != err {
		t.Fatal(err)
	}
	if err = repo.prepareLocalLazyManifestClosure(map[string]interface{}{}); nil == err ||
		!strings.Contains(err.Error(), "file ID mismatch") {
		t.Fatalf("malformed identity did not fail closed: %v", err)
	}
}

func TestCanonicalPathLegacyIdentityRequiresCompleteLocalMetadataProof(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*entity.File)
		put    bool
		valid  bool
	}{
		{name: "valid", put: true, valid: true},
		{name: "missing", put: false},
		{name: "path mismatch", put: true, mutate: func(file *entity.File) { file.Path = "/assets/other.png" }},
		{name: "size mismatch", put: true, mutate: func(file *entity.File) { file.Size++ }},
		{name: "updated mismatch", put: true, mutate: func(file *entity.File) { file.Updated += 1000 }},
		{name: "chunks mismatch", put: true, mutate: func(file *entity.File) { file.Chunks = []string{util.RandHash()} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			key, err := encryption.KDF("canonical-legacy-proof-password", "canonical-legacy-proof-salt")
			if nil != err {
				t.Fatal(err)
			}
			backend := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: tc.name, AvailableSize: 1 << 40,
				Local: &cloud.ConfLocal{Endpoint: filepath.Join(root, "cloud"), ConcurrentReqs: 1}}})
			repo, err := NewRepoWithLazyLoad(filepath.Join(root, "data"), filepath.Join(root, "repo"), filepath.Join(root, "history"),
				filepath.Join(root, "temp"), "receiver", "receiver", runtime.GOOS, key, nil, backend, true)
			if nil != err {
				t.Fatal(err)
			}
			legacy := entity.NewFile("/assets/proven.png", 8, 1700003000000)
			legacy.Chunks = []string{util.Hash([]byte("historical"))}
			stored := *legacy
			stored.Chunks = append([]string(nil), legacy.Chunks...)
			if nil != tc.mutate {
				tc.mutate(&stored)
			}
			if tc.put {
				if err = repo.store.PutFile(&stored); nil != err {
					t.Fatal(err)
				}
			}
			manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{
				"assets/proven.png": {Path: "assets/proven.png", FileID: legacy.ID, Size: legacy.Size, Modified: legacy.Updated,
					Chunks: append([]string(nil), legacy.Chunks...)},
			}}
			if err = repo.lazyLoader.saveManifest(manifest); nil != err {
				t.Fatal(err)
			}
			err = repo.prepareLocalLazyManifestClosure(map[string]interface{}{})
			if !tc.valid {
				if nil == err || !strings.Contains(err.Error(), "file ID mismatch") {
					t.Fatalf("incomplete proof was accepted: %v", err)
				}
				return
			}
			if nil != err {
				t.Fatal(err)
			}
			asset := manifest.Assets["assets/proven.png"]
			if asset.FileID == legacy.ID || asset.FileID != entity.NewFile(asset.Path, asset.Size, asset.Modified).ID {
				t.Fatalf("proven legacy identity was not canonicalized: %#v", asset)
			}
			historical, err := repo.store.GetFile(legacy.ID)
			if nil != err || historical.Path != legacy.Path || !slices.Equal(historical.Chunks, legacy.Chunks) {
				t.Fatalf("historical object was overwritten: %#v err=%v", historical, err)
			}
		})
	}
}

func TestIndexPreparesCurrentLazyManifestClosureBeforeSync(t *testing.T) {
	root := t.TempDir()
	key, err := encryption.KDF("lazy-manifest-index-password", "lazy-manifest-index-salt")
	if nil != err {
		t.Fatal(err)
	}
	backend := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: "lazy-manifest-index", AvailableSize: 1 << 40,
		Local: &cloud.ConfLocal{Endpoint: filepath.Join(root, "cloud"), ConcurrentReqs: 1}}})
	repo, err := NewRepoWithLazyLoad(filepath.Join(root, "data"), filepath.Join(root, "repo"), filepath.Join(root, "history"),
		filepath.Join(root, "temp"), "receiver", "receiver", runtime.GOOS, key, nil, backend, true)
	if nil != err {
		t.Fatal(err)
	}
	data := []byte("lazy metadata closure")
	if err = os.MkdirAll(filepath.Join(repo.DataPath, "assets"), 0755); nil != err {
		t.Fatal(err)
	}
	assetPath := filepath.Join(repo.DataPath, "assets", "boot.bin")
	if err = os.WriteFile(assetPath, data, 0644); nil != err {
		t.Fatal(err)
	}
	updated := time.Unix(1700001000, 0)
	if err = os.Chtimes(assetPath, updated, updated); nil != err {
		t.Fatal(err)
	}
	file := entity.NewFile("assets/boot.bin", int64(len(data)), updated.UnixMilli())
	file.Chunks = []string{util.Hash(data)}
	putTestFile(t, repo, file, data)
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{
		file.Path: {Path: file.Path, FileID: file.ID, Size: file.Size, Modified: file.Updated, Chunks: file.Chunks},
	}}
	if err = repo.lazyLoader.saveManifest(manifest); nil != err {
		t.Fatal(err)
	}
	if _, err = repo.Index("receiver baseline", false, map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	_, objectPath := repo.store.AbsPath(file.ID)
	cloudObjectPath := filepath.Join("objects", file.ID[:2], file.ID[2:])
	if _, err = repo.cloud.UploadObject(cloudObjectPath, false); nil != err {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(repo.Path, lazyManifestLocalClosureMarker)); nil != err {
		t.Fatal(err)
	}
	// 云端迁移消费标记只表示清单格式已处理，不能代替当前本地清单对应的对象闭包标记。
	if err = repo.markLazyManifestHydrated(file.ID); nil != err {
		t.Fatal(err)
	}
	if err = os.Remove(objectPath); nil != err {
		t.Fatal(err)
	}
	fileCache.Clear()
	if err = os.WriteFile(filepath.Join(repo.DataPath, "boot-change.txt"), []byte("change"), 0644); nil != err {
		t.Fatal(err)
	}
	if _, err = repo.Index("boot index before sync", false, map[string]interface{}{}); nil != err {
		t.Fatalf("boot index did not hydrate local lazy metadata closure: %v", err)
	}
	if _, err = os.Stat(objectPath); nil != err {
		t.Fatalf("boot index did not restore lazy metadata object: %v", err)
	}
	if _, err = os.Stat(filepath.Join(repo.Path, lazyManifestLocalClosureMarker)); nil != err {
		t.Fatalf("boot index did not persist local closure marker: %v", err)
	}
}

func TestPublishedLazyManifestMetadataClosureUploadsPendingMetadataWithoutLocalChunks(t *testing.T) {
	root := t.TempDir()
	key, err := encryption.KDF("lazy-publication-closure-password", "lazy-publication-closure-salt")
	if nil != err {
		t.Fatal(err)
	}
	backend := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: "lazy-publication-closure", AvailableSize: 1 << 40,
		Local: &cloud.ConfLocal{Endpoint: filepath.Join(root, "cloud"), ConcurrentReqs: 1}}})
	repo, err := NewRepoWithLazyLoad(filepath.Join(root, "data"), filepath.Join(root, "repo"), filepath.Join(root, "history"),
		filepath.Join(root, "temp"), "publisher", "publisher", runtime.GOOS, key, nil, backend, true)
	if nil != err {
		t.Fatal(err)
	}
	file := entity.NewFile("assets/pending.bin", 7, 1700002000000)
	file.Chunks = []string{util.Hash([]byte("pending"))}
	stale := entity.NewFile(file.Path, file.Size, file.Updated)
	stale.Chunks = []string{util.Hash([]byte("stale"))}
	if err = repo.store.PutFile(stale); nil != err {
		t.Fatal(err)
	}
	staleObjectPath := filepath.Join("objects", stale.ID[:2], stale.ID[2:])
	if _, err = repo.cloud.UploadObject(staleObjectPath, false); nil != err {
		t.Fatal(err)
	}
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{
		file.Path: {Path: file.Path, FileID: file.ID, Size: file.Size, Modified: file.Updated, Chunks: file.Chunks},
	}}
	if err = repo.lazyLoader.saveManifest(manifest); nil != err {
		t.Fatal(err)
	}
	if changed, canonicalErr := repo.canonicalizeLazyManifestObjectIdentities(manifest); nil != canonicalErr || changed != 1 {
		t.Fatalf("reidentify publication conflict changed=%d err=%v", changed, canonicalErr)
	}
	file.ID = manifest.Assets[file.Path].FileID
	file.Updated = manifest.Assets[file.Path].Modified
	index := &entity.Index{ID: util.RandHash(), LazyFiles: []string{file.ID}}
	traffic := &TrafficStat{m: &sync.Mutex{}}
	if err = repo.ensurePublishedLazyManifestMetadata(index, traffic, map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	objectPath := filepath.Join("objects", file.ID[:2], file.ID[2:])
	if _, err = repo.cloud.DownloadObject(objectPath); nil != err {
		t.Fatalf("published manifest metadata was not uploaded: %v", err)
	}
	_, cloudFile, err := repo.downloadCloudFile(file.ID, 1, 1, map[string]interface{}{})
	if nil != err || !slices.Equal(cloudFile.Chunks, file.Chunks) {
		t.Fatalf("conflicting remote metadata was not overwritten from manifest: file=%#v err=%v", cloudFile, err)
	}
	_, historicalCloudFile, err := repo.downloadCloudFile(stale.ID, 1, 1, map[string]interface{}{})
	if nil != err || !slices.Equal(historicalCloudFile.Chunks, stale.Chunks) {
		t.Fatalf("historical remote metadata was overwritten: file=%#v err=%v", historicalCloudFile, err)
	}
	if err = repo.cloud.RemoveObject(objectPath); nil != err {
		t.Fatal(err)
	}
	if err = repo.ensurePublishedLazyManifestMetadata(index, traffic, map[string]interface{}{}); nil != err {
		t.Fatalf("resuming closure after remote loss failed: %v", err)
	}
	if _, err = repo.cloud.DownloadObject(objectPath); nil != err {
		t.Fatalf("remotely lost completed metadata was not re-uploaded: %v", err)
	}
}

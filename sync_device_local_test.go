// DejaVu - Data snapshot and sync.
// Copyright (c) 2022-present, b3log.org
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package dejavu

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/88250/gulu"
	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/dejavu/util"
	"github.com/siyuan-note/encryption"
	"github.com/siyuan-note/filelock"
)

type blockingDownloadCloud struct {
	cloud.Cloud
	blocked chan struct{}
	release chan struct{}
	once    sync.Once
}

type panickingDownloadCloud struct {
	cloud.Cloud
}

func (panicking *panickingDownloadCloud) DownloadObject(filePath string) ([]byte, error) {
	if "refs/latest" == filePath {
		panic("device-local sync panic")
	}
	return panicking.Cloud.DownloadObject(filePath)
}

func (blocking *blockingDownloadCloud) DownloadObject(filePath string) ([]byte, error) {
	if "refs/latest" == filePath {
		blocking.once.Do(func() {
			close(blocking.blocked)
			<-blocking.release
		})
	}
	return blocking.Cloud.DownloadObject(filePath)
}

func TestFilterDeviceLocalMergeResult(t *testing.T) {
	repo := &Repo{DataPath: filepath.Join(t.TempDir(), "data") + string(os.PathSeparator)}
	keep := &entity.File{Path: "/storage/keep.json"}
	alias := "../" + filepath.Base(filepath.Clean(repo.DataPath)) + "/storage/ref-used.json"
	upserts := []*entity.File{{Path: refUsedRepoPath}, keep}
	if "darwin" == runtime.GOOS || "windows" == runtime.GOOS {
		upserts = append(upserts, &entity.File{Path: "/Storage/REF-USED.JSON"})
	}
	mergeResult := &MergeResult{
		Upserts:   upserts,
		Removes:   []*entity.File{{Path: alias}, keep},
		Conflicts: []*entity.File{{Path: `storage\ref-used.json`}, keep},
	}

	repo.filterProtectedMergeResult(mergeResult)
	for name, files := range map[string][]*entity.File{
		"upserts": mergeResult.Upserts, "removes": mergeResult.Removes, "conflicts": mergeResult.Conflicts,
	} {
		if 1 != len(files) || keep != files[0] {
			t.Fatalf("%s = %#v, want only control file", name, files)
		}
	}
	unsafe := []*entity.File{{Path: "../../outside.json"}, keep}
	filtered, didFilter := repo.filterProtectedSyncFiles(unsafe)
	if !didFilter || 1 != len(filtered) || keep != filtered[0] {
		t.Fatalf("unsafe checkout path was not filtered: %#v", filtered)
	}
}

func TestDeviceLocalRefUsedSurvivesAutomaticAndManualSync(t *testing.T) {
	tests := []struct {
		name           string
		publishesClean bool
		run            func(*Repo) (*MergeResult, error)
	}{
		{
			name: "automatic", publishesClean: true,
			run: func(repo *Repo) (*MergeResult, error) {
				mergeResult, _, err := repo.Sync(map[string]interface{}{})
				return mergeResult, err
			},
		},
		{
			name: "manual download",
			run: func(repo *Repo) (*MergeResult, error) {
				mergeResult, _, err := repo.SyncDownload(map[string]interface{}{})
				return mergeResult, err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, client := newLegacyRefUsedSyncFixture(t)
			refUsedPath := filepath.Join(client.DataPath, "storage", "ref-used.json")
			localData := []byte(`{"local":true}`)
			writeRefUsedTestFile(t, refUsedPath, localData)

			mergeResult, err := test.run(client)
			if err != nil {
				t.Fatal(err)
			}
			assertMergeResultExcludesRefUsed(t, client, mergeResult)
			assertStoredIndexesExcludeRefUsed(t, client)
			if test.publishesClean {
				assertCloudLatestExcludesRefUsed(t, client)
			}
			data, err := filelock.ReadFile(refUsedPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(localData) != string(data) {
				t.Fatalf("ref-used data = %q, want %q", data, localData)
			}
		})
	}
}

func TestGetSyncCloudFilesFiltersDeviceLocalBootPrefetch(t *testing.T) {
	for _, lazy := range []bool{false, true} {
		placement := "normal"
		if lazy {
			placement = "lazy"
		}
		t.Run(placement, func(t *testing.T) {
			server, client := newLegacyRefUsedSyncFixtureWithPlacement(t, lazy)
			cloudLatest, err := client.GetCloudLatest(map[string]interface{}{})
			if err != nil {
				t.Fatal(err)
			}
			protectedID := legacyRefUsedFileID(t, server, cloudLatest.ID)
			if _, err = client.store.Stat(protectedID); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("device-local metadata exists before boot prefetch: %v", err)
			}

			fetchedFiles, err := client.GetSyncCloudFiles(cloudLatest, map[string]interface{}{})
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range fetchedFiles {
				if client.isProtectedSyncPath(file.Path) {
					t.Fatalf("boot prefetch returned device-local metadata [%s]", file.Path)
				}
			}
			_, statErr := client.store.Stat(protectedID)
			if lazy && !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("lazy device-local metadata was prefetched: %v", statErr)
			}
			if !lazy && statErr != nil {
				t.Fatalf("normal metadata was not fetched before result filtering: %v", statErr)
			}
		})
	}
}

func TestSyncUploadMigratesLegacyLatestBeforePublishing(t *testing.T) {
	for _, cloudState := range []string{"existing legacy cloud", "empty cloud"} {
		t.Run(cloudState, func(t *testing.T) {
			server, baselineClient := newLegacyRefUsedSyncFixture(t)
			legacyID := mustLatest(t, server).ID
			if "empty cloud" == cloudState {
				if err := server.cloud.RemoveRepo("device-local"); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := server.SyncUpload(map[string]interface{}{}); err != nil {
				t.Fatal(err)
			}
			cleaned := mustLatest(t, server)
			if legacyID == cleaned.ID {
				t.Fatal("legacy latest ID was reused after device-local files were removed")
			}
			assertStoredIndexesExcludeRefUsed(t, server)
			assertCloudLatestExcludesRefUsed(t, server)

			cloudLatest, err := server.GetCloudLatest(map[string]interface{}{})
			if err != nil {
				t.Fatal(err)
			}
			checkIndexData, err := server.cloud.DownloadObject("check/indexes/" + cloudLatest.CheckIndexID)
			if err != nil {
				t.Fatal(err)
			}
			checkIndexData, err = server.store.compressDecoder.DecodeAll(checkIndexData, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkIndex := &entity.CheckIndex{}
			if err = gulu.JSON.UnmarshalJSON(checkIndexData, checkIndex); err != nil {
				t.Fatal(err)
			}
			legacyFileID := legacyRefUsedFileID(t, server, legacyID)
			for _, checkFile := range checkIndex.Files {
				if checkFile.ID == legacyFileID {
					t.Fatalf("check index contains legacy ref-used file [%s]", checkFile.ID)
				}
			}

			fresh := cloneSyncTestRepo(t, baselineClient, "fresh")
			mergeResult, _, err := fresh.SyncDownload(map[string]interface{}{})
			if err != nil {
				t.Fatal(err)
			}
			assertMergeResultExcludesRefUsed(t, fresh, mergeResult)
			assertStoredIndexesExcludeRefUsed(t, fresh)
			if data, readErr := os.ReadFile(filepath.Join(fresh.DataPath, "document.txt")); readErr != nil || "baseline" != string(data) {
				t.Fatalf("fresh client document = %q, err=%v", data, readErr)
			}
		})
	}
}

func TestLocalUpsertFilesFiltersLegacyLazyRefUsed(t *testing.T) {
	server, _ := newLegacyRefUsedSyncFixtureWithPlacement(t, true)
	legacy := mustLatest(t, server)
	files, err := server.localUpsertFiles(legacy, &entity.Index{}, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if server.isProtectedSyncPath(file.Path) {
			t.Fatalf("local upsert files contains protected lazy file [%s]", file.Path)
		}
	}
}

func TestDeviceLocalRefUsedInLegacyLazyIndexSurvivesSync(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Repo) (*MergeResult, error)
	}{
		{
			name: "automatic",
			run: func(repo *Repo) (*MergeResult, error) {
				mergeResult, _, err := repo.Sync(map[string]interface{}{})
				return mergeResult, err
			},
		},
		{
			name: "manual download",
			run: func(repo *Repo) (*MergeResult, error) {
				mergeResult, _, err := repo.SyncDownload(map[string]interface{}{})
				return mergeResult, err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, client := newLegacyRefUsedSyncFixtureWithPlacement(t, true)
			refUsedPath := filepath.Join(client.DataPath, "storage", "ref-used.json")
			localData := []byte(`{"local":"lazy"}`)
			writeRefUsedTestFile(t, refUsedPath, localData)
			mergeResult, err := test.run(client)
			if err != nil {
				t.Fatal(err)
			}
			assertMergeResultExcludesRefUsed(t, client, mergeResult)
			assertStoredIndexesExcludeRefUsed(t, client)
			data, err := filelock.ReadFile(refUsedPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(localData) != string(data) {
				t.Fatalf("ref-used data = %q, want %q", data, localData)
			}
		})
	}
}

func TestDeviceLocalRefUsedSurvivesRealRemoveConflictAndAbsentUpsert(t *testing.T) {
	t.Run("originally absent cloud upsert", func(t *testing.T) {
		_, client := newLegacyRefUsedSyncFixture(t)
		refUsedPath := filepath.Join(client.DataPath, "storage", "ref-used.json")
		if err := filelock.Remove(refUsedPath); nil != err && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}

		mergeResult, _, err := client.SyncDownload(map[string]interface{}{})
		if nil != err {
			t.Fatal(err)
		}
		assertMergeResultExcludesRefUsed(t, client, mergeResult)
		if _, err = os.Stat(refUsedPath); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("device-local file was created from cloud upsert: %v", err)
		}
	})

	t.Run("cloud remove", func(t *testing.T) {
		server, client := newLegacyRefUsedSyncFixture(t)
		copyTestDir(t, server.Path, client.Path)
		aesKey := client.store.AesKey
		clientCloud := client.cloud
		var err error
		client, err = NewRepo(client.DataPath, client.Path, client.HistoryPath, client.TempPath, "client", "client", runtime.GOOS,
			aesKey, nil, clientCloud)
		if nil != err {
			t.Fatal(err)
		}
		if _, err = server.SyncUpload(map[string]interface{}{}); nil != err {
			t.Fatal(err)
		}

		refUsedPath := filepath.Join(client.DataPath, "storage", "ref-used.json")
		localData := []byte(`{"local":"remove"}`)
		writeRefUsedTestFile(t, refUsedPath, localData)
		mergeResult, _, err := client.SyncDownload(map[string]interface{}{})
		if nil != err {
			t.Fatal(err)
		}
		assertMergeResultExcludesRefUsed(t, client, mergeResult)
		data, err := filelock.ReadFile(refUsedPath)
		if nil != err || string(localData) != string(data) {
			t.Fatalf("ref-used after cloud remove = %q, err=%v", data, err)
		}
	})

	t.Run("cloud and local conflict", func(t *testing.T) {
		server, client := newLegacyRefUsedSyncFixture(t)
		localData := []byte(`{"local":"conflict"}`)
		publishLegacyRefUsed(t, client, localData, false)
		publishLegacyRefUsed(t, server, []byte(`{"cloud":"conflict"}`), false)

		refUsedPath := filepath.Join(client.DataPath, "storage", "ref-used.json")
		writeRefUsedTestFile(t, refUsedPath, localData)
		mergeResult, _, err := client.Sync(map[string]interface{}{})
		if nil != err {
			t.Fatal(err)
		}
		assertMergeResultExcludesRefUsed(t, client, mergeResult)
		data, err := filelock.ReadFile(refUsedPath)
		if nil != err || string(localData) != string(data) {
			t.Fatalf("ref-used after conflict = %q, err=%v", data, err)
		}
	})
}

func TestDeviceLocalRefUsedConcurrentWriteWinsAfterSync(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Repo) error
	}{
		{
			name: "automatic",
			run: func(repo *Repo) error {
				_, _, err := repo.Sync(map[string]interface{}{})
				return err
			},
		},
		{
			name: "manual download",
			run: func(repo *Repo) error {
				_, _, err := repo.SyncDownload(map[string]interface{}{})
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, client := newLegacyRefUsedSyncFixture(t)
			refUsedPath := filepath.Join(client.DataPath, "storage", "ref-used.json")
			writeRefUsedTestFile(t, refUsedPath, []byte(`{"local":"before"}`))

			blocking := &blockingDownloadCloud{
				Cloud: client.cloud, blocked: make(chan struct{}), release: make(chan struct{}),
			}
			client.cloud = blocking
			syncDone := make(chan error, 1)
			go func() {
				syncDone <- test.run(client)
			}()

			select {
			case <-blocking.blocked:
			case <-time.After(5 * time.Second):
				t.Fatal("sync did not reach the blocked cloud read")
			}

			newerData := []byte(`{"local":"newer"}`)
			writeDone := make(chan error, 1)
			go func() {
				writeDone <- filelock.WriteFile(refUsedPath, newerData)
			}()
			select {
			case err := <-writeDone:
				t.Fatalf("concurrent local write completed before sync released the device-local lock: %v", err)
			case <-time.After(100 * time.Millisecond):
			}

			close(blocking.release)
			if err := <-syncDone; err != nil {
				t.Fatal(err)
			}
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			data, err := filelock.ReadFile(refUsedPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(newerData) != string(data) {
				t.Fatalf("ref-used data = %q, want concurrent write %q", data, newerData)
			}
		})
	}
}

func TestDeviceLocalRefUsedLockReleasedAfterSyncPanic(t *testing.T) {
	_, client := newLegacyRefUsedSyncFixture(t)
	refUsedPath := filepath.Join(client.DataPath, "storage", "ref-used.json")
	writeRefUsedTestFile(t, refUsedPath, []byte(`{"local":"before-panic"}`))
	client.cloud = &panickingDownloadCloud{Cloud: client.cloud}

	func() {
		defer func() {
			if nil == recover() {
				t.Fatal("sync did not propagate the injected panic")
			}
		}()
		_, _, _ = client.Sync(map[string]interface{}{})
	}()

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- filelock.WriteFile(refUsedPath, []byte(`{"local":"after-panic"}`))
	}()
	select {
	case err := <-writeDone:
		if nil != err {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("device-local lock remained held after sync panic")
	}
}

func TestDeviceLocalRefUsedSurvivesDirectCheckout(t *testing.T) {
	server, client := newLegacyRefUsedSyncFixture(t)
	copyTestDir(t, server.Path, client.Path)
	aesKey, err := encryption.KDF("device-local-password", "device-local-salt")
	if err != nil {
		t.Fatal(err)
	}
	client, err = NewRepo(client.DataPath, client.Path, client.HistoryPath, client.TempPath, "client", "client", runtime.GOOS,
		aesKey, nil, client.cloud)
	if err != nil {
		t.Fatal(err)
	}
	refUsedPath := filepath.Join(client.DataPath, "storage", "ref-used.json")
	localData := []byte(`{"local":"checkout"}`)
	writeRefUsedTestFile(t, refUsedPath, localData)
	legacy, err := server.Latest()
	if err != nil {
		t.Fatal(err)
	}

	upserts, removes, err := client.Checkout(legacy.ID, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	for _, files := range [][]*entity.File{upserts, removes} {
		for _, file := range files {
			if client.isProtectedSyncPath(file.Path) {
				t.Fatalf("checkout returned device-local path [%s]", file.Path)
			}
		}
	}
	data, err := filelock.ReadFile(refUsedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(localData) != string(data) {
		t.Fatalf("ref-used data = %q, want %q", data, localData)
	}
}

func TestDeviceLocalRefUsedSurvivesCheckoutFilesFromCloud(t *testing.T) {
	server, client := newLegacyRefUsedSyncFixture(t)
	legacy, err := server.Latest()
	if err != nil {
		t.Fatal(err)
	}
	legacyFiles, err := server.getFiles(legacy.Files)
	if err != nil {
		t.Fatal(err)
	}
	var refUsedFile *entity.File
	for _, file := range legacyFiles {
		if server.isProtectedSyncPath(file.Path) {
			refUsedFile = file
			break
		}
	}
	if nil == refUsedFile {
		t.Fatal("legacy index does not contain ref-used file")
	}
	refUsedPath := filepath.Join(client.DataPath, "storage", "ref-used.json")
	localData := []byte(`{"local":"cloud-checkout"}`)
	writeRefUsedTestFile(t, refUsedPath, localData)

	stat, err := client.CheckoutFilesFromCloud([]*entity.File{refUsedFile}, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if 0 != stat.DownloadChunkCount || 0 != stat.DownloadBytes {
		t.Fatalf("device-local cloud checkout downloaded data: %#v", stat)
	}
	data, err := filelock.ReadFile(refUsedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(localData) != string(data) {
		t.Fatalf("ref-used data = %q, want %q", data, localData)
	}
}

func newLegacyRefUsedSyncFixture(t *testing.T) (server, client *Repo) {
	return newLegacyRefUsedSyncFixtureWithPlacement(t, false)
}

func newLegacyRefUsedSyncFixtureWithPlacement(t *testing.T, lazy bool) (server, client *Repo) {
	t.Helper()
	root := t.TempDir()
	cloudEndpoint := filepath.Join(root, "cloud")
	if err := os.MkdirAll(cloudEndpoint, 0755); err != nil {
		t.Fatal(err)
	}
	aesKey, err := encryption.KDF("device-local-password", "device-local-salt")
	if err != nil {
		t.Fatal(err)
	}
	newRepo := func(name string) *Repo {
		base := filepath.Join(root, name)
		localCloud := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{
			Dir: "device-local", UserID: "0", AvailableSize: 1 << 40,
			Local: &cloud.ConfLocal{Endpoint: cloudEndpoint, ConcurrentReqs: 4},
		}})
		ret, newErr := NewRepo(filepath.Join(base, "data"), filepath.Join(base, "repo"), filepath.Join(base, "history"),
			filepath.Join(base, "temp"), name, name, runtime.GOOS, aesKey, nil, localCloud)
		if newErr != nil {
			t.Fatal(newErr)
		}
		return ret
	}

	server = newRepo("server")
	if err = os.MkdirAll(server.DataPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(server.DataPath, "document.txt"), []byte("baseline"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = server.Index("baseline", true, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = server.Sync(map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}

	client = newRepo("client")
	copyTestDir(t, server.DataPath, client.DataPath)
	copyTestDir(t, server.Path, client.Path)
	client, err = NewRepo(client.DataPath, client.Path, client.HistoryPath, client.TempPath, "client", "client", runtime.GOOS,
		aesKey, nil, client.cloud)
	if err != nil {
		t.Fatal(err)
	}
	publishLegacyRefUsed(t, server, []byte(`{"cloud":"legacy"}`), lazy)
	return
}

func publishLegacyRefUsed(t *testing.T, repo *Repo, data []byte, lazy bool) {
	t.Helper()
	absPath := filepath.Join(repo.DataPath, "storage", "ref-used.json")
	writeRefUsedTestFile(t, absPath, data)
	info, err := os.Stat(absPath)
	if err != nil {
		t.Fatal(err)
	}
	file := entity.NewFile(refUsedRepoPath, info.Size(), info.ModTime().UnixMilli())
	if err = repo.putFileChunks(file, map[string]interface{}{}, 1, 1); err != nil {
		t.Fatal(err)
	}
	latest, err := repo.Latest()
	if err != nil {
		t.Fatal(err)
	}
	legacy := *latest
	legacy.ID = util.RandHash()
	legacy.Created = time.Now().Add(time.Second).UnixMilli()
	legacy.Files = append([]string{}, latest.Files...)
	legacy.LazyFiles = append([]string{}, latest.LazyFiles...)
	if lazy {
		legacy.LazyFiles = append(legacy.LazyFiles, file.ID)
	} else {
		legacy.Files = append(legacy.Files, file.ID)
	}
	legacy.Count = len(legacy.Files)
	legacy.Size += file.Size
	if err = repo.store.PutIndex(&legacy); err != nil {
		t.Fatal(err)
	}
	if err = repo.UpdateLatest(&legacy); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.uploadChunks(file.Chunks, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.uploadFiles([]*entity.File{file}, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.uploadIndex(&legacy, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.updateCloudRef("refs/latest", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if err = repo.UpdateLatestSync(&legacy); err != nil {
		t.Fatal(err)
	}
}

func writeRefUsedTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := filelock.WriteFile(path, data); err != nil {
		t.Fatal(err)
	}
}

func assertMergeResultExcludesRefUsed(t *testing.T, repo *Repo, mergeResult *MergeResult) {
	t.Helper()
	if nil == mergeResult {
		t.Fatal("merge result is nil")
	}
	for name, files := range map[string][]*entity.File{
		"upserts": mergeResult.Upserts, "removes": mergeResult.Removes, "conflicts": mergeResult.Conflicts,
	} {
		for _, file := range files {
			if nil != file && repo.isProtectedSyncPath(file.Path) {
				t.Fatalf("%s contains device-local file [%s]", name, file.Path)
			}
		}
	}
}

func assertStoredIndexesExcludeRefUsed(t *testing.T, repo *Repo) {
	t.Helper()
	for name, index := range map[string]*entity.Index{"latest": mustLatest(t, repo), "latest-sync": repo.latestSync()} {
		files, err := repo.getFiles(append(append([]string{}, index.Files...), index.LazyFiles...))
		if err != nil {
			t.Fatalf("get %s files failed: %s", name, err)
		}
		for _, file := range files {
			if repo.isProtectedSyncPath(file.Path) {
				t.Fatalf("%s contains protected file [%s]", name, file.Path)
			}
		}
	}
}

func assertCloudLatestExcludesRefUsed(t *testing.T, repo *Repo) {
	t.Helper()
	cloudLatest, err := repo.GetCloudLatest(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := repo.getFiles(append(append([]string{}, cloudLatest.Files...), cloudLatest.LazyFiles...))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if repo.isProtectedSyncPath(file.Path) {
			t.Fatalf("cloud latest contains protected file [%s]", file.Path)
		}
	}
}

func legacyRefUsedFileID(t *testing.T, repo *Repo, legacyID string) string {
	t.Helper()
	legacy, err := repo.store.GetIndex(legacyID)
	if err != nil {
		t.Fatal(err)
	}
	files, err := repo.getFiles(append(append([]string{}, legacy.Files...), legacy.LazyFiles...))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if repo.isProtectedSyncPath(file.Path) {
			return file.ID
		}
	}
	t.Fatal("legacy index does not contain ref-used file")
	return ""
}

func mustLatest(t *testing.T, repo *Repo) *entity.Index {
	t.Helper()
	latest, err := repo.Latest()
	if err != nil {
		t.Fatal(err)
	}
	return latest
}

func cloneSyncTestRepo(t *testing.T, source *Repo, name string) *Repo {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	dataPath := filepath.Join(root, "data")
	repoPath := filepath.Join(root, "repo")
	copyTestDir(t, source.DataPath, dataPath)
	copyTestDir(t, source.Path, repoPath)
	aesKey, err := encryption.KDF("device-local-password", "device-local-salt")
	if err != nil {
		t.Fatal(err)
	}
	conf := source.cloud.GetConf()
	localCloud := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{
		Dir: conf.Dir, UserID: conf.UserID, AvailableSize: conf.AvailableSize,
		Local: &cloud.ConfLocal{Endpoint: conf.Local.Endpoint, ConcurrentReqs: conf.Local.ConcurrentReqs},
	}})
	ret, err := NewRepo(dataPath, repoPath, filepath.Join(root, "history"), filepath.Join(root, "temp"), name, name,
		runtime.GOOS, aesKey, nil, localCloud)
	if err != nil {
		t.Fatal(err)
	}
	return ret
}

func copyTestDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(target, data, 0644)
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

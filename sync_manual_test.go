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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/encryption"
)

type concurrentUploadCloud struct {
	*cloud.BaseCloud
	concurrentReqs int
	upload         func(string) (int64, error)
}

type concurrentDownloadCloud struct {
	cloud.Cloud
	concurrentReqs int
	download       func(string) ([]byte, error)
}

type countingDownloadCloud struct {
	cloud.Cloud
	downloads atomic.Int64
}

func (testCloud *countingDownloadCloud) DownloadObject(filePath string) ([]byte, error) {
	if lockSyncKey != filePath {
		testCloud.downloads.Add(1)
	}
	return testCloud.Cloud.DownloadObject(filePath)
}

func (testCloud *concurrentDownloadCloud) GetConcurrentReqs() int {
	return testCloud.concurrentReqs
}

func (testCloud *concurrentDownloadCloud) DownloadObject(filePath string) ([]byte, error) {
	return testCloud.download(filePath)
}

func (testCloud *concurrentUploadCloud) GetConcurrentReqs() int {
	return testCloud.concurrentReqs
}

func (testCloud *concurrentUploadCloud) UploadObject(filePath string, _ bool) (int64, error) {
	return testCloud.upload(filePath)
}

func TestConcurrentUploadsAggregateBytesAndFirstError(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Repo, []string) (int64, error)
	}{
		{
			name: "files",
			run: func(repo *Repo, ids []string) (int64, error) {
				files := make([]*entity.File, 0, len(ids))
				for _, id := range ids {
					files = append(files, &entity.File{ID: id})
				}
				return repo.uploadFiles(files, nil)
			},
		},
		{
			name: "chunks",
			run: func(repo *Repo, ids []string) (int64, error) {
				return repo.uploadChunks(ids, nil)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name+" success", func(t *testing.T) {
			ids := uploadTestIDs(64)
			var calls atomic.Int64
			testCloud := &concurrentUploadCloud{
				BaseCloud:      &cloud.BaseCloud{Conf: &cloud.Conf{}},
				concurrentReqs: 8,
				upload: func(string) (int64, error) {
					calls.Add(1)
					return 7, nil
				},
			}

			uploaded, err := test.run(&Repo{cloud: testCloud}, ids)
			if err != nil {
				t.Fatal(err)
			}
			if uploaded != int64(len(ids))*7 {
				t.Fatalf("uploaded bytes = %d, want %d", uploaded, int64(len(ids))*7)
			}
			if calls.Load() != int64(len(ids)) {
				t.Fatalf("upload calls = %d, want %d", calls.Load(), len(ids))
			}
		})

		t.Run(test.name+" failure", func(t *testing.T) {
			ids := uploadTestIDs(128)
			firstErr := errors.New("first concurrent upload failure")
			var successfulBytes atomic.Int64
			var failed atomic.Bool
			testCloud := &concurrentUploadCloud{
				BaseCloud:      &cloud.BaseCloud{Conf: &cloud.Conf{}},
				concurrentReqs: 16,
				upload: func(string) (int64, error) {
					if failed.CompareAndSwap(false, true) {
						return 0, firstErr
					}
					successfulBytes.Add(3)
					return 3, nil
				},
			}

			uploaded, err := test.run(&Repo{cloud: testCloud}, ids)
			if !errors.Is(err, firstErr) {
				t.Fatalf("upload error = %v, want %v", err, firstErr)
			}
			if uploaded != successfulBytes.Load() {
				t.Fatalf("uploaded bytes = %d, completed successful bytes = %d", uploaded, successfulBytes.Load())
			}
		})

		t.Run(test.name+" zero concurrency", func(t *testing.T) {
			ids := uploadTestIDs(4)
			testCloud := &concurrentUploadCloud{
				BaseCloud:      &cloud.BaseCloud{Conf: &cloud.Conf{}},
				concurrentReqs: 0,
				upload: func(string) (int64, error) {
					return 5, nil
				},
			}

			uploaded, err := test.run(&Repo{cloud: testCloud}, ids)
			if err != nil {
				t.Fatal(err)
			}
			if uploaded != int64(len(ids))*5 {
				t.Fatalf("uploaded bytes = %d, want %d", uploaded, int64(len(ids))*5)
			}
		})
	}
}

func TestConcurrentUploadsReturnCompletedBytesOnFailure(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Repo, []string) (int64, error)
	}{
		{
			name: "files",
			run: func(repo *Repo, ids []string) (int64, error) {
				return repo.uploadFiles([]*entity.File{{ID: ids[0]}, {ID: ids[1]}}, nil)
			},
		},
		{
			name: "chunks",
			run: func(repo *Repo, ids []string) (int64, error) {
				return repo.uploadChunks(ids, nil)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ids := uploadTestIDs(2)
			failure := errors.New("concurrent upload failure")
			started := make(chan struct{}, 2)
			proceed := make(chan struct{})
			testCloud := &concurrentUploadCloud{
				BaseCloud:      &cloud.BaseCloud{Conf: &cloud.Conf{}},
				concurrentReqs: 2,
				upload: func(filePath string) (int64, error) {
					started <- struct{}{}
					<-proceed
					if strings.HasSuffix(filePath, ids[0][2:]) {
						return 0, failure
					}
					return 11, nil
				},
			}

			result := make(chan struct {
				bytes int64
				err   error
			}, 1)
			go func() {
				uploaded, err := test.run(&Repo{cloud: testCloud}, ids)
				result <- struct {
					bytes int64
					err   error
				}{uploaded, err}
			}()
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			for i := 0; i < 2; i++ {
				select {
				case <-started:
				case <-timer.C:
					close(proceed)
					t.Fatal("concurrent uploads did not both start")
				}
			}
			close(proceed)
			got := <-result
			if !errors.Is(got.err, failure) {
				t.Fatalf("upload error = %v, want %v", got.err, failure)
			}
			if got.bytes != 11 {
				t.Fatalf("uploaded bytes = %d, want completed 11 bytes", got.bytes)
			}
		})
	}
}

func TestConcurrentUploadsReturnWorkerPanic(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Repo) (int64, error)
	}{
		{
			name: "files",
			run: func(repo *Repo) (int64, error) {
				return repo.uploadFiles([]*entity.File{{ID: "invalid"}}, nil)
			},
		},
		{
			name: "chunks",
			run: func(repo *Repo) (int64, error) {
				return repo.uploadChunks([]string{"invalid"}, nil)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testCloud := &concurrentUploadCloud{
				BaseCloud:      &cloud.BaseCloud{Conf: &cloud.Conf{}},
				concurrentReqs: 1,
				upload: func(string) (int64, error) {
					panic("cloud upload panic")
				},
			}

			uploaded, err := test.run(&Repo{cloud: testCloud})
			if err == nil || !strings.Contains(err.Error(), "worker panic") {
				t.Fatalf("upload error = %v, want worker panic", err)
			}
			if uploaded != 0 {
				t.Fatalf("uploaded bytes = %d, want 0", uploaded)
			}
		})
	}
}

func TestConcurrentUploadsPreservePanickedError(t *testing.T) {
	panicErr := errors.New("typed cloud panic")
	tests := []struct {
		name string
		run  func(*Repo) (int64, error)
	}{
		{
			name: "files",
			run: func(repo *Repo) (int64, error) {
				return repo.uploadFiles([]*entity.File{{ID: uploadTestIDs(1)[0]}}, nil)
			},
		},
		{
			name: "chunks",
			run: func(repo *Repo) (int64, error) {
				return repo.uploadChunks(uploadTestIDs(1), nil)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testCloud := &concurrentUploadCloud{
				BaseCloud:      &cloud.BaseCloud{Conf: &cloud.Conf{}},
				concurrentReqs: 1,
				upload: func(string) (int64, error) {
					panic(panicErr)
				},
			}

			uploaded, err := test.run(&Repo{cloud: testCloud})
			if !errors.Is(err, panicErr) {
				t.Fatalf("upload error = %v, want wrapped panic %v", err, panicErr)
			}
			if uploaded != 0 {
				t.Fatalf("uploaded bytes = %d, want 0", uploaded)
			}
		})
	}
}

func TestConcurrentDownloadsPreserveCompletedTrafficAndFirstError(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Repo, []string) concurrentObjectTransferResult
	}{
		{
			name: "files",
			run: func(repo *Repo, ids []string) concurrentObjectTransferResult {
				result, _ := repo.downloadCloudFilesPutDetailed(ids, nil)
				return result
			},
		},
		{
			name: "chunks",
			run: func(repo *Repo, ids []string) concurrentObjectTransferResult {
				return repo.downloadCloudChunksPutDetailed(ids, nil)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, client := newLegacyRefUsedSyncFixture(t)
			latest := mustLatest(t, server)
			files, err := server.getFiles(append(append([]string{}, latest.Files...), latest.LazyFiles...))
			if nil != err {
				t.Fatal(err)
			}
			ids := make([]string, 0, 2)
			if "files" == test.name {
				for _, file := range files {
					ids = append(ids, file.ID)
					if 2 == len(ids) {
						break
					}
				}
			} else {
				ids = repoUniqueChunkIDs(files)
				if 2 < len(ids) {
					ids = ids[:2]
				}
			}
			if 2 != len(ids) {
				t.Fatalf("fixture produced %d %s IDs, want 2", len(ids), test.name)
			}
			for _, id := range ids {
				_ = client.store.Remove(id)
			}

			underlying := client.cloud
			failure := errors.New("concurrent download failure")
			started := make(chan string, 2)
			release := make(chan struct{})
			client.cloud = &concurrentDownloadCloud{
				Cloud: underlying, concurrentReqs: 2,
				download: func(filePath string) ([]byte, error) {
					for _, id := range ids {
						if strings.HasSuffix(filePath, id[2:]) {
							started <- id
							<-release
							if id == ids[0] {
								return nil, failure
							}
							break
						}
					}
					return underlying.DownloadObject(filePath)
				},
			}

			resultDone := make(chan concurrentObjectTransferResult, 1)
			go func() { resultDone <- test.run(client, ids) }()
			seen := map[string]bool{<-started: true, <-started: true}
			if !seen[ids[0]] || !seen[ids[1]] {
				close(release)
				t.Fatalf("download workers did not both start: %#v", seen)
			}
			close(release)
			result := <-resultDone
			if !errors.Is(result.err, failure) {
				t.Fatalf("download error = %v, want %v", result.err, failure)
			}
			if 1 != result.completed || 2 != result.attempted || 1 > result.bytes {
				t.Fatalf("unexpected partial download result: %#v", result)
			}
		})
	}
}

func TestAutomaticAndManualDownloadAPIGetMatchesCloudCalls(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Repo) (*TrafficStat, error)
	}{
		{
			name: "automatic",
			run: func(repo *Repo) (*TrafficStat, error) {
				_, traffic, err := repo.Sync(map[string]interface{}{})
				return traffic, err
			},
		},
		{
			name: "manual download",
			run: func(repo *Repo) (*TrafficStat, error) {
				_, traffic, err := repo.SyncDownload(map[string]interface{}{})
				return traffic, err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, client := newLegacyRefUsedSyncFixture(t)
			counting := &countingDownloadCloud{Cloud: client.cloud}
			client.cloud = counting
			traffic, err := test.run(client)
			if nil != err {
				t.Fatal(err)
			}
			if want := int(counting.downloads.Load()); want != traffic.APIGet {
				t.Fatalf("API GET count = %d, cloud DownloadObject calls = %d", traffic.APIGet, want)
			}
		})
	}
}

func repoUniqueChunkIDs(files []*entity.File) (ret []string) {
	seen := map[string]bool{}
	for _, file := range files {
		for _, chunkID := range file.Chunks {
			if !seen[chunkID] {
				seen[chunkID] = true
				ret = append(ret, chunkID)
			}
		}
	}
	return
}

func uploadTestIDs(count int) []string {
	ret := make([]string, 0, count)
	for i := 0; i < count; i++ {
		ret = append(ret, fmt.Sprintf("%040x", i+1))
	}
	return ret
}

type recordingLocalCloud struct {
	*cloud.Local
	mu      sync.Mutex
	uploads []string
}

type corruptObjectReadbackCloud struct {
	*cloud.Local
	corrupt atomic.Bool
	missing atomic.Bool
}

func (testCloud *corruptObjectReadbackCloud) DownloadObject(filePath string) ([]byte, error) {
	data, err := testCloud.Local.DownloadObject(filePath)
	if err == nil && testCloud.missing.Load() && strings.HasPrefix(filepath.ToSlash(filePath), "objects/") {
		return nil, cloud.ErrCloudObjectNotFound
	}
	if err == nil && testCloud.corrupt.Load() && strings.HasPrefix(filepath.ToSlash(filePath), "objects/") {
		return []byte("corrupt readback"), nil
	}
	return data, err
}

type failingLocalCloud struct {
	*cloud.Local
	failPath string
	err      error
}

func (testCloud *failingLocalCloud) UploadObject(filePath string, overwrite bool) (int64, error) {
	if filePath == testCloud.failPath {
		return 0, testCloud.err
	}
	return testCloud.Local.UploadObject(filePath, overwrite)
}

func TestUpdateCloudIndexesDoesNotPublishLatestWhenPrerequisiteFails(t *testing.T) {
	root := t.TempDir()
	dataPath := filepath.Join(root, "data")
	repoPath := filepath.Join(root, "repo")
	cloudPath := filepath.Join(root, "cloud")
	for _, dir := range []string{dataPath, repoPath, cloudPath} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataPath, "document.txt"), []byte("publication ordering"), 0644); err != nil {
		t.Fatal(err)
	}
	aesKey, err := encryption.KDF("publication-password", "publication-salt")
	if err != nil {
		t.Fatal(err)
	}
	localCloud := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{
		Dir: "publication", AvailableSize: 1 << 40,
		Local: &cloud.ConfLocal{Endpoint: cloudPath, ConcurrentReqs: 2},
	}})
	testCloud := &failingLocalCloud{Local: localCloud, failPath: "indexes-v2.json", err: errors.New("injected indexes failure")}
	repo, err := NewRepo(dataPath, repoPath, filepath.Join(root, "history"), filepath.Join(root, "temp"),
		"publication-device", "publication-device", "test", aesKey, nil, testCloud)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := repo.Index("publication", true, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	oldLatest := "old-published-index"
	if _, err = localCloud.UploadBytes("refs/latest", []byte(oldLatest), true); err != nil {
		t.Fatal(err)
	}
	traffic := &TrafficStat{m: &sync.Mutex{}}
	if err = repo.updateCloudIndexes(latest, traffic, map[string]interface{}{}); !errors.Is(err, testCloud.err) {
		t.Fatalf("error = %v, want injected failure", err)
	}
	got, err := localCloud.DownloadObject("refs/latest")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != oldLatest {
		t.Fatalf("refs/latest advanced to %q after prerequisite failure, want %q", got, oldLatest)
	}
}

func (testCloud *recordingLocalCloud) UploadObject(filePath string, overwrite bool) (int64, error) {
	testCloud.mu.Lock()
	testCloud.uploads = append(testCloud.uploads, filePath)
	testCloud.mu.Unlock()
	return testCloud.Local.UploadObject(filePath, overwrite)
}

func (testCloud *recordingLocalCloud) uploadedObject(id string) bool {
	testCloud.mu.Lock()
	defer testCloud.mu.Unlock()
	want := filepath.ToSlash(filepath.Join("objects", id[:2], id[2:]))
	for _, uploaded := range testCloud.uploads {
		if uploaded == want {
			return true
		}
	}
	return false
}

func (testCloud *recordingLocalCloud) objectUploadCount(id string) int {
	testCloud.mu.Lock()
	defer testCloud.mu.Unlock()
	want := filepath.ToSlash(filepath.Join("objects", id[:2], id[2:]))
	var ret int
	for _, uploaded := range testCloud.uploads {
		if uploaded == want {
			ret++
		}
	}
	return ret
}

func (testCloud *recordingLocalCloud) resetUploads() {
	testCloud.mu.Lock()
	testCloud.uploads = nil
	testCloud.mu.Unlock()
}

func TestSyncDeduplicatesPreUploadAcrossMerge(t *testing.T) {
	root := t.TempDir()
	aesKey, err := encryption.KDF("merged-sync-password", "merged-sync-salt")
	if nil != err {
		t.Fatal(err)
	}
	var bCloud *recordingLocalCloud
	newRepo := func(name string) *Repo {
		dataPath := filepath.Join(root, name, "data")
		if mkdirErr := os.MkdirAll(dataPath, 0755); nil != mkdirErr {
			t.Fatal(mkdirErr)
		}
		testCloud := &recordingLocalCloud{Local: cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{
			Dir: "merged-sync", AvailableSize: 1 << 40,
			Local: &cloud.ConfLocal{Endpoint: filepath.Join(root, "cloud"), ConcurrentReqs: 4},
		}})}
		ret, newErr := NewRepo(dataPath, filepath.Join(root, name, "repo"), filepath.Join(root, name, "history"),
			filepath.Join(root, name, "temp"), name, name, "test", aesKey, nil, testCloud)
		if nil != newErr {
			t.Fatal(newErr)
		}
		if "b" == name {
			bCloud = testCloud
		}
		return ret
	}

	a := newRepo("a")
	b := newRepo("b")
	if err = os.WriteFile(filepath.Join(b.DataPath, "seed.txt"), []byte("receiver seed"), 0644); nil != err {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(a.DataPath, "cloud.txt"), []byte("cloud baseline"), 0644); nil != err {
		t.Fatal(err)
	}
	if _, err = a.Index("baseline", true, map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	if _, _, err = a.Sync(map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	if _, err = b.Index("empty baseline", true, map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	if _, _, err = b.Sync(map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}

	if err = os.WriteFile(filepath.Join(a.DataPath, "cloud.txt"), []byte("cloud changed"), 0644); nil != err {
		t.Fatal(err)
	}
	if _, err = a.Index("cloud change", true, map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	if _, _, err = a.Sync(map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(b.DataPath, "assets"), 0755); nil != err {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(b.DataPath, "assets", "local.bin"), []byte(strings.Repeat("local", 4096)), 0644); nil != err {
		t.Fatal(err)
	}
	localLatest, err := b.Index("local change", true, map[string]interface{}{})
	if nil != err {
		t.Fatal(err)
	}
	localFiles, err := b.getFiles(localLatest.Files)
	if nil != err {
		t.Fatal(err)
	}
	var localFile *entity.File
	for _, file := range localFiles {
		if "/assets/local.bin" == file.Path || "assets/local.bin" == file.Path {
			localFile = file
			break
		}
	}
	if nil == localFile {
		t.Fatal("local merge fixture file not indexed")
	}
	bCloud.resetUploads()
	if _, _, err = b.Sync(map[string]interface{}{}); nil != err {
		t.Fatal(err)
	}
	if got := bCloud.objectUploadCount(localFile.ID); 1 != got {
		t.Fatalf("local file upload count across pre-upload and merge = %d, want 1", got)
	}
	for _, id := range localFile.Chunks {
		if got := bCloud.objectUploadCount(id); 1 != got {
			t.Fatalf("local chunk %s upload count across pre-upload and merge = %d, want 1", id, got)
		}
	}
}

func TestUploadCloudSessionDeduplicatesAcrossMergedIndex(t *testing.T) {
	root := t.TempDir()
	dataPath := filepath.Join(root, "data")
	if err := os.MkdirAll(dataPath, 0755); nil != err {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataPath, "document.txt"), []byte(strings.Repeat("merged", 2048)), 0644); nil != err {
		t.Fatal(err)
	}

	aesKey, err := encryption.KDF("merged-upload-password", "merged-upload-salt")
	if nil != err {
		t.Fatal(err)
	}
	testCloud := &recordingLocalCloud{Local: cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{
		Dir: "merged-upload", AvailableSize: 1 << 40,
		Local: &cloud.ConfLocal{Endpoint: filepath.Join(root, "cloud"), ConcurrentReqs: 4},
	}})}
	repo, err := NewRepo(dataPath, filepath.Join(root, "repo"), filepath.Join(root, "history"), filepath.Join(root, "temp"),
		"merged-device", "merged-device", "test", aesKey, nil, testCloud)
	if nil != err {
		t.Fatal(err)
	}
	latest, err := repo.Index("pre-merge", true, map[string]interface{}{})
	if nil != err {
		t.Fatal(err)
	}
	files, err := repo.getFiles(latest.Files)
	if nil != err {
		t.Fatal(err)
	}
	chunks := repo.getChunks(files)
	session := newSyncUploadSession()
	traffic := &TrafficStat{m: &sync.Mutex{}}
	if err = repo.uploadCloud(map[string]interface{}{}, latest, &entity.Index{}, nil, traffic, session); nil != err {
		t.Fatal(err)
	}

	merged := *latest
	merged.ID = "0123456789abcdef0123456789abcdef01234567"
	if err = repo.uploadCloud(map[string]interface{}{}, &merged, &entity.Index{}, nil, traffic, session); nil != err {
		t.Fatal(err)
	}
	for _, file := range files {
		if got := testCloud.objectUploadCount(file.ID); 1 != got {
			t.Fatalf("file %s upload count = %d, want 1", file.ID, got)
		}
	}
	for _, id := range chunks {
		if got := testCloud.objectUploadCount(id); 1 != got {
			t.Fatalf("chunk %s upload count = %d, want 1", id, got)
		}
	}
}

func TestManualSyncRecordsChunkAndFileUploadsThroughProductionPath(t *testing.T) {
	root := t.TempDir()
	dataPath := filepath.Join(root, "data")
	repoPath := filepath.Join(root, "repo")
	cloudPath := filepath.Join(root, "cloud")
	for _, dir := range []string{dataPath, repoPath, cloudPath} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for i, data := range []string{
		strings.Repeat("alpha", 1024),
		strings.Repeat("beta", 1536),
	} {
		if err := os.WriteFile(filepath.Join(dataPath, fmt.Sprintf("document-%d.txt", i)), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}

	aesKey, err := encryption.KDF("manual-sync-password", "manual-sync-salt")
	if err != nil {
		t.Fatal(err)
	}
	localCloud := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{
		Dir:           "manual-sync",
		AvailableSize: 1 << 40,
		Local: &cloud.ConfLocal{
			Endpoint:       cloudPath,
			ConcurrentReqs: 4,
		},
	}})
	testCloud := &recordingLocalCloud{Local: localCloud}
	repo, err := NewRepo(dataPath, repoPath, filepath.Join(root, "history"), filepath.Join(root, "temp"),
		"manual-device", "manual-device", "test", aesKey, nil, testCloud)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := repo.Index("manual sync", true, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := repo.getFiles(latest.Files)
	if err != nil {
		t.Fatal(err)
	}
	chunkIDs := repo.getChunks(files)
	if len(files) == 0 || len(chunkIDs) == 0 {
		t.Fatalf("test fixture must produce files and chunks: files=%d chunks=%d", len(files), len(chunkIDs))
	}

	traffic, err := repo.SyncUpload(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if traffic.UploadChunkCount != len(chunkIDs) {
		t.Fatalf("upload chunk count = %d, want %d", traffic.UploadChunkCount, len(chunkIDs))
	}
	// updateCloudIndexes uploads the index, refs/latest and indexes-v2.json in addition to the data files.
	if traffic.UploadFileCount != len(files)+3 {
		t.Fatalf("upload file count = %d, want %d", traffic.UploadFileCount, len(files)+3)
	}
	// Data objects are followed by the index, refs/latest and indexes-v2.json writes.
	wantAPIPut := len(chunkIDs) + len(files) + 3
	if traffic.APIPut != wantAPIPut {
		t.Fatalf("API PUT count = %d, want %d", traffic.APIPut, wantAPIPut)
	}
	// SyncUpload reads refs/latest and indexes-v2.json, then reads back every
	// newly uploaded object and index before publication.
	wantAPIGet := 2 + len(chunkIDs) + len(files) + 1
	if traffic.APIGet != wantAPIGet {
		t.Fatalf("API GET count = %d, want %d", traffic.APIGet, wantAPIGet)
	}
	for _, id := range chunkIDs {
		if !testCloud.uploadedObject(id) {
			t.Fatalf("chunk %s was not uploaded through the production cloud path", id)
		}
	}
	for _, file := range files {
		if !testCloud.uploadedObject(file.ID) {
			t.Fatalf("file %s was not uploaded through the production cloud path", file.ID)
		}
	}
}

func TestSyncUploadCorruptReadbackPreservesPublishedLatest(t *testing.T) {
	root := t.TempDir()
	dataPath := filepath.Join(root, "data")
	repoPath := filepath.Join(root, "repo")
	cloudPath := filepath.Join(root, "cloud")
	for _, dir := range []string{dataPath, repoPath, cloudPath} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	documentPath := filepath.Join(dataPath, "document.txt")
	if err := os.WriteFile(documentPath, []byte("first version"), 0644); err != nil {
		t.Fatal(err)
	}
	aesKey, err := encryption.KDF("readback-password", "readback-salt")
	if err != nil {
		t.Fatal(err)
	}
	localCloud := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: "readback", AvailableSize: 1 << 40,
		Local: &cloud.ConfLocal{Endpoint: cloudPath, ConcurrentReqs: 2}}})
	testCloud := &corruptObjectReadbackCloud{Local: localCloud}
	repo, err := NewRepo(dataPath, repoPath, filepath.Join(root, "history"), filepath.Join(root, "temp"),
		"readback-device", "readback-device", "test", aesKey, nil, testCloud)
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.Index("first", true, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.SyncUpload(map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(documentPath, []byte("second version with changed bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(2 * time.Second)
	if err = os.Chtimes(documentPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Index("second", true, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	testCloud.corrupt.Store(true)
	if _, err = repo.SyncUpload(map[string]interface{}{}); err == nil {
		t.Fatal("expected corrupt uploaded object readback to fail sync")
	}
	testCloud.corrupt.Store(false)
	published, err := localCloud.DownloadObject("refs/latest")
	if err != nil {
		t.Fatal(err)
	}
	if string(published) != first.ID {
		t.Fatalf("published latest advanced to %q, want %q", published, first.ID)
	}

	testCloud.missing.Store(true)
	if _, err = repo.SyncUpload(map[string]interface{}{}); err == nil {
		t.Fatal("expected missing uploaded object readback to fail sync")
	}
	testCloud.missing.Store(false)
	published, err = localCloud.DownloadObject("refs/latest")
	if err != nil {
		t.Fatal(err)
	}
	if string(published) != first.ID {
		t.Fatalf("published latest advanced after missing readback to %q, want %q", published, first.ID)
	}
}

func TestUploadCloudCountsOnlyCurrentBatchAPIPuts(t *testing.T) {
	root := t.TempDir()
	dataPath := filepath.Join(root, "data")
	repoPath := filepath.Join(root, "repo")
	cloudPath := filepath.Join(root, "cloud")
	for _, dir := range []string{dataPath, repoPath, cloudPath} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataPath, "document.txt"), []byte(strings.Repeat("merge", 2048)), 0644); err != nil {
		t.Fatal(err)
	}

	aesKey, err := encryption.KDF("upload-cloud-password", "upload-cloud-salt")
	if err != nil {
		t.Fatal(err)
	}
	testCloud := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{
		Dir:           "upload-cloud",
		AvailableSize: 1 << 40,
		Local: &cloud.ConfLocal{
			Endpoint:       cloudPath,
			ConcurrentReqs: 4,
		},
	}})
	repo, err := NewRepo(dataPath, repoPath, filepath.Join(root, "history"), filepath.Join(root, "temp"),
		"upload-device", "upload-device", "test", aesKey, nil, testCloud)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := repo.Index("upload cloud stats", true, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := repo.getFiles(latest.Files)
	if err != nil {
		t.Fatal(err)
	}
	chunkIDs := repo.getChunks(files)

	traffic := &TrafficStat{
		UploadTrafficStat: UploadTrafficStat{UploadFileCount: 11, UploadChunkCount: 13},
		APITrafficStat:    APITrafficStat{APIPut: 17},
		m:                 &sync.Mutex{},
	}
	if err := repo.uploadCloud(map[string]interface{}{}, latest, &entity.Index{}, nil, traffic); err != nil {
		t.Fatal(err)
	}
	if traffic.UploadChunkCount != 13+len(chunkIDs) {
		t.Fatalf("upload chunk count = %d, want %d", traffic.UploadChunkCount, 13+len(chunkIDs))
	}
	if traffic.UploadFileCount != 11+len(files) {
		t.Fatalf("upload file count = %d, want %d", traffic.UploadFileCount, 11+len(files))
	}
	if traffic.APIPut != 17+len(chunkIDs)+len(files) {
		t.Fatalf("API PUT count = %d, want %d", traffic.APIPut, 17+len(chunkIDs)+len(files))
	}
}

func TestUploadCloudPreservesPartialTrafficOnFailure(t *testing.T) {
	root := t.TempDir()
	dataPath := filepath.Join(root, "data")
	if err := os.MkdirAll(dataPath, 0755); nil != err {
		t.Fatal(err)
	}
	for i, data := range []string{strings.Repeat("first", 1024), strings.Repeat("second", 1024)} {
		if err := os.WriteFile(filepath.Join(dataPath, fmt.Sprintf("document-%d.txt", i)), []byte(data), 0644); nil != err {
			t.Fatal(err)
		}
	}

	aesKey, err := encryption.KDF("partial-upload-password", "partial-upload-salt")
	if nil != err {
		t.Fatal(err)
	}
	testCloud := &concurrentUploadCloud{
		BaseCloud:      &cloud.BaseCloud{Conf: &cloud.Conf{AvailableSize: 1 << 40}},
		concurrentReqs: 2,
	}
	repo, err := NewRepo(dataPath, filepath.Join(root, "repo"), filepath.Join(root, "history"), filepath.Join(root, "temp"),
		"partial-upload", "partial-upload", "test", aesKey, nil, testCloud)
	if nil != err {
		t.Fatal(err)
	}
	latest, err := repo.Index("partial upload", true, map[string]interface{}{})
	if nil != err {
		t.Fatal(err)
	}
	files, err := repo.getFiles(latest.Files)
	if nil != err {
		t.Fatal(err)
	}
	chunkIDs := repo.getChunks(files)
	if 2 > len(chunkIDs) {
		t.Fatalf("fixture produced %d chunks, want at least 2", len(chunkIDs))
	}

	failure := errors.New("partial production upload failed")
	started := make(chan string, 2)
	release := make(chan struct{})
	testCloud.upload = func(filePath string) (int64, error) {
		for _, chunkID := range chunkIDs[:2] {
			if strings.HasSuffix(filePath, chunkID[2:]) {
				started <- chunkID
				<-release
				if chunkID == chunkIDs[0] {
					return 0, failure
				}
				return 11, nil
			}
		}
		return 13, nil
	}
	traffic := &TrafficStat{m: &sync.Mutex{}}
	done := make(chan error, 1)
	go func() { done <- repo.uploadCloud(map[string]interface{}{}, latest, &entity.Index{}, nil, traffic) }()
	seen := map[string]bool{<-started: true, <-started: true}
	if !seen[chunkIDs[0]] || !seen[chunkIDs[1]] {
		close(release)
		t.Fatalf("upload workers did not both start: %#v", seen)
	}
	close(release)
	if err = <-done; !errors.Is(err, failure) {
		t.Fatalf("upload error = %v, want %v", err, failure)
	}
	if 1 != traffic.UploadChunkCount || 11 != traffic.UploadBytes || 2 != traffic.APIPut || 0 != traffic.UploadFileCount {
		t.Fatalf("unexpected partial production traffic: %#v", traffic)
	}
}

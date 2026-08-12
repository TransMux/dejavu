// DejaVu - Data snapshot and sync.
// Copyright (c) 2022-present, b3log.org
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package dejavu

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/encryption"
)

type syncAuditEvent struct {
	Sequence   int64             `json:"sequence"`
	Scenario   string            `json:"scenario"`
	Operation  string            `json:"operation"`
	Parent     string            `json:"parent,omitempty"`
	Object     string            `json:"object,omitempty"`
	StartedNS  int64             `json:"started_ns"`
	DurationNS int64             `json:"duration_ns"`
	Result     string            `json:"result"`
	Bytes      int64             `json:"bytes,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

const syncAuditParentContextKey = "syncAuditParent"

func syncAuditChildContext(recorder *syncAuditRecorder, parent string) map[string]interface{} {
	return map[string]interface{}{SyncAuditContextKey: recorder, syncAuditParentContextKey: parent}
}

type syncAuditRecorder struct {
	scenario string
	origin   time.Time
	sequence atomic.Int64
	mu       sync.Mutex
	events   []syncAuditEvent
}

func (recorder *syncAuditRecorder) record(operation, object string, started time.Time, bytes int64, err error, attributes map[string]string) {
	result := "success"
	if err != nil {
		result = "failure"
	}
	event := syncAuditEvent{Sequence: recorder.sequence.Add(1), Scenario: recorder.scenario, Operation: operation,
		Object: object, StartedNS: started.Sub(recorder.origin).Nanoseconds(), DurationNS: time.Since(started).Nanoseconds(),
		Result: result, Bytes: bytes, Attributes: attributes}
	recorder.mu.Lock()
	recorder.events = append(recorder.events, event)
	recorder.mu.Unlock()
}

func (recorder *syncAuditRecorder) RecordSyncAuditEvent(event SyncAuditEvent) {
	recorder.mu.Lock()
	recorder.events = append(recorder.events, syncAuditEvent{Sequence: recorder.sequence.Add(1), Scenario: recorder.scenario,
		Operation: event.Operation, Parent: event.Attributes["parent"], StartedNS: event.StartedAt.Sub(recorder.origin).Nanoseconds(), DurationNS: event.DurationNS,
		Result: event.Result, Attributes: event.Attributes})
	recorder.mu.Unlock()
}

func (recorder *syncAuditRecorder) snapshot() []syncAuditEvent {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	ret := append([]syncAuditEvent(nil), recorder.events...)
	sort.Slice(ret, func(i, j int) bool { return ret[i].Sequence < ret[j].Sequence })
	return ret
}

type syncAuditCloud struct {
	cloud.Cloud
	mu            sync.Mutex
	recorder      *syncAuditRecorder
	failAt        map[string]int64
	failObject    map[string]int64
	failPredicate func(operation, object string) bool
	calls         sync.Map
}

func (audit *syncAuditCloud) invoke(operation, object string, action func() (int64, error)) (int64, error) {
	started := time.Now()
	counter, _ := audit.calls.LoadOrStore(operation, &atomic.Int64{})
	call := counter.(*atomic.Int64).Add(1)
	audit.mu.Lock()
	failCall := audit.failAt[operation]
	objectKey := operation + "\x00" + object
	_, failObject := audit.failObject[objectKey]
	if failObject {
		delete(audit.failObject, objectKey)
	}
	failPredicate := audit.failPredicate != nil && audit.failPredicate(operation, object)
	if failPredicate {
		audit.failPredicate = nil
	}
	recorder := audit.recorder
	audit.mu.Unlock()
	if failPredicate || failObject || failCall == call {
		err := fmt.Errorf("injected %s failure at call %d", operation, call)
		recorder.record(operation, object, started, 0, err, map[string]string{"call": fmt.Sprint(call), "injected": "true"})
		return 0, err
	}
	bytes, err := action()
	recorder.record(operation, object, started, bytes, err, map[string]string{"call": fmt.Sprint(call)})
	return bytes, err
}

func (audit *syncAuditCloud) UploadObject(path string, overwrite bool) (int64, error) {
	return audit.invoke("cloud.upload_object", path, func() (int64, error) { return audit.Cloud.UploadObject(path, overwrite) })
}

func (audit *syncAuditCloud) UploadBytes(path string, data []byte, overwrite bool) (int64, error) {
	return audit.invoke("cloud.upload_bytes", path, func() (int64, error) { return audit.Cloud.UploadBytes(path, data, overwrite) })
}

func (audit *syncAuditCloud) DownloadObject(path string) ([]byte, error) {
	var data []byte
	_, err := audit.invoke("cloud.download_object", path, func() (int64, error) {
		var innerErr error
		data, innerErr = audit.Cloud.DownloadObject(path)
		return int64(len(data)), innerErr
	})
	return data, err
}

func (audit *syncAuditCloud) RemoveObject(path string) error {
	_, err := audit.invoke("cloud.remove_object", path, func() (int64, error) { return 0, audit.Cloud.RemoveObject(path) })
	return err
}

func (audit *syncAuditCloud) GetIndex(id string) (*entity.Index, error) {
	var index *entity.Index
	_, err := audit.invoke("cloud.get_index", id, func() (int64, error) {
		var innerErr error
		index, innerErr = audit.Cloud.GetIndex(id)
		return 0, innerErr
	})
	return index, err
}

func (audit *syncAuditCloud) GetChunks(ids []string) ([]string, error) {
	var ret []string
	_, err := audit.invoke("cloud.get_chunks", "", func() (int64, error) {
		var innerErr error
		ret, innerErr = audit.Cloud.GetChunks(ids)
		return 0, innerErr
	})
	return ret, err
}

func (audit *syncAuditCloud) ListObjects(prefix string) (map[string]*entity.ObjectInfo, error) {
	var ret map[string]*entity.ObjectInfo
	_, err := audit.invoke("cloud.list_objects", prefix, func() (int64, error) {
		var innerErr error
		ret, innerErr = audit.Cloud.ListObjects(prefix)
		return 0, innerErr
	})
	return ret, err
}

type syncAuditFixture struct {
	t       *testing.T
	root    string
	cloud   string
	key     []byte
	outputs string
}

func newSyncAuditFixture(t *testing.T) *syncAuditFixture {
	root := t.TempDir()
	key, err := encryption.KDF("sync-audit-password", "sync-audit-salt")
	if err != nil {
		t.Fatal(err)
	}
	outputs := os.Getenv("DEJAVU_SYNC_AUDIT_OUTPUT")
	if outputs == "" {
		outputs = filepath.Join(root, "audit")
	}
	if err = os.MkdirAll(outputs, 0755); err != nil {
		t.Fatal(err)
	}
	return &syncAuditFixture{t: t, root: root, cloud: filepath.Join(root, "cloud"), key: key, outputs: outputs}
}

func (fixture *syncAuditFixture) repo(name, scenario string, failAt map[string]int64) (*Repo, *syncAuditRecorder) {
	base := filepath.Join(fixture.root, name)
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(data, 0755); err != nil {
		fixture.t.Fatal(err)
	}
	recorder := &syncAuditRecorder{scenario: scenario, origin: time.Now()}
	local := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: "sync-audit", RepoPath: filepath.Join(base, "repo"),
		AvailableSize: 1 << 40, Local: &cloud.ConfLocal{Endpoint: fixture.cloud, ConcurrentReqs: 4}}})
	audited := &syncAuditCloud{Cloud: local, recorder: recorder, failAt: failAt}
	repo, err := NewRepo(data, filepath.Join(base, "repo"), filepath.Join(base, "history"), filepath.Join(base, "temp"),
		name, name, runtime.GOOS, fixture.key, nil, audited)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return repo, recorder
}

func (fixture *syncAuditFixture) write(recorder *syncAuditRecorder) {
	path := filepath.Join(fixture.outputs, recorder.scenario+".jsonl")
	file, err := os.Create(path)
	if err != nil {
		fixture.t.Fatal(err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, event := range recorder.snapshot() {
		if err = encoder.Encode(event); err != nil {
			fixture.t.Fatal(err)
		}
	}
	fixture.t.Logf("同步审计事件：%s", path)
}

func auditIndexAndSync(t *testing.T, repo *Repo, memo string) error {
	recorder := repo.cloud.(*syncAuditCloud).recorder
	context := map[string]interface{}{SyncAuditContextKey: recorder}
	finishIndex := BeginSyncAudit(context, "dejavu.index", nil)
	if _, err := repo.Index(memo, true, context); err != nil {
		finishIndex(err)
		return err
	}
	finishIndex(nil)
	_, _, err := repo.Sync(context)
	return err
}

func TestSyncAuditCoversRequiredOperations(t *testing.T) {
	fixture := newSyncAuditFixture(t)
	repo, recorder := fixture.repo("coverage", "coverage", nil)
	if err := os.WriteFile(filepath.Join(repo.DataPath, "doc.txt"), []byte("coverage"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := auditIndexAndSync(t, repo, "coverage"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range recorder.snapshot() {
		seen[event.Operation] = true
	}
	for _, operation := range []string{
		"dejavu.index", "dejavu.sync", "dejavu.sync.core", "dejavu.sync.download_latest",
		"dejavu.sync.verify_chunks", "dejavu.sync.verify_files", "dejavu.sync.restore_files",
		"dejavu.sync.publish_check_index", "dejavu.sync.upload_index", "dejavu.sync.publish_ref",
		"dejavu.sync.publish_indexes", "dejavu.sync.merge_publish",
	} {
		if !seen[operation] {
			t.Errorf("missing required audit operation %s", operation)
		}
	}
}

func TestSyncAuditMatrix(t *testing.T) {
	fixture := newSyncAuditFixture(t)
	a, aRecorder := fixture.repo("a", "baseline_publish", nil)
	if err := os.WriteFile(filepath.Join(a.DataPath, "shared.txt"), []byte("baseline"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := auditIndexAndSync(t, a, "baseline"); err != nil {
		t.Fatal(err)
	}
	fixture.write(aRecorder)

	b, bRecorder := fixture.repo("b", "remote_download", nil)
	if err := os.WriteFile(filepath.Join(b.DataPath, "receiver-seed.txt"), []byte("receiver"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := auditIndexAndSync(t, b, "receiver baseline"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(b.DataPath, "shared.txt")); err != nil || string(data) != "baseline" {
		t.Fatalf("remote download did not converge: %q %v", data, err)
	}
	fixture.write(bRecorder)

	noChange := &syncAuditRecorder{scenario: "no_change", origin: time.Now()}
	b.cloud.(*syncAuditCloud).recorder = noChange
	if _, _, err := b.Sync(map[string]interface{}{SyncAuditContextKey: noChange}); err != nil {
		t.Fatal(err)
	}
	fixture.write(noChange)

	localChange := &syncAuditRecorder{scenario: "local_change", origin: time.Now()}
	b.cloud.(*syncAuditCloud).recorder = localChange
	if err := os.WriteFile(filepath.Join(b.DataPath, "local.txt"), []byte("local"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := auditIndexAndSync(t, b, "local change"); err != nil {
		t.Fatal(err)
	}
	fixture.write(localChange)

	if err := os.WriteFile(filepath.Join(a.DataPath, "a.txt"), []byte("from a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := auditIndexAndSync(t, a, "a concurrent change"); err != nil {
		t.Fatal(err)
	}
	both := &syncAuditRecorder{scenario: "bidirectional_non_conflict", origin: time.Now()}
	b.cloud.(*syncAuditCloud).recorder = both
	if err := os.WriteFile(filepath.Join(b.DataPath, "b.txt"), []byte("from b"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := auditIndexAndSync(t, b, "b concurrent change"); err != nil {
		t.Fatal(err)
	}
	fixture.write(both)

	manualDownload := &syncAuditRecorder{scenario: "manual_download_no_change", origin: time.Now()}
	b.cloud.(*syncAuditCloud).recorder = manualDownload
	if _, _, err := b.SyncDownload(map[string]interface{}{SyncAuditContextKey: manualDownload}); err != nil {
		t.Fatal(err)
	}
	fixture.write(manualDownload)

	manualUpload := &syncAuditRecorder{scenario: "manual_upload_no_change", origin: time.Now()}
	b.cloud.(*syncAuditCloud).recorder = manualUpload
	if _, err := b.SyncUpload(map[string]interface{}{SyncAuditContextKey: manualUpload}); err != nil {
		t.Fatal(err)
	}
	fixture.write(manualUpload)
}

func TestSyncAuditFailureBeforeLatestIsRetryable(t *testing.T) {
	fixture := newSyncAuditFixture(t)
	repo, recorder := fixture.repo("failure", "failure_before_latest", map[string]int64{"cloud.upload_object": 2})
	if err := os.WriteFile(filepath.Join(repo.DataPath, "doc.txt"), []byte(strings.Repeat("failure", 4096)), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Index("failure", true, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	_, _, firstErr := repo.Sync(map[string]interface{}{SyncAuditContextKey: recorder})
	if firstErr == nil {
		t.Fatal("expected injected upload failure")
	}
	fixture.write(recorder)

	retry := &syncAuditRecorder{scenario: "retry_after_failure", origin: time.Now()}
	audit := repo.cloud.(*syncAuditCloud)
	audit.recorder = retry
	audit.failAt = nil
	if _, _, err := repo.Sync(map[string]interface{}{SyncAuditContextKey: retry}); err != nil {
		t.Fatal(err)
	}
	fixture.write(retry)
}

func TestSyncAuditEventsAreCompleteAndOrdered(t *testing.T) {
	recorder := &syncAuditRecorder{scenario: "contract", origin: time.Now()}
	recorder.record("cloud.download_object", "refs/latest", time.Now(), 12, nil, nil)
	recorder.record("cloud.upload_object", "refs/latest", time.Now(), 9, errors.New("failure"), nil)
	events := recorder.snapshot()
	if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("unexpected ordered events: %#v", events)
	}
	for _, event := range events {
		if event.Operation == "" || event.Result == "" || event.DurationNS < 0 {
			t.Fatalf("incomplete event: %#v", event)
		}
	}
}

func newConvergedAuditPair(t *testing.T, fixture *syncAuditFixture, filePath string, data []byte) (a, b *Repo) {
	a, _ = fixture.repo("a", "pair_a", nil)
	b, _ = fixture.repo("b", "pair_b", nil)
	if err := os.WriteFile(filepath.Join(a.DataPath, "persistent-seed.txt"), []byte("persistent"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.DataPath, filePath), data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := auditIndexAndSync(t, a, "publish baseline"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.DataPath, "receiver-seed.txt"), []byte("receiver"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := auditIndexAndSync(t, b, "consume baseline"); err != nil {
		t.Fatal(err)
	}
	return
}

func auditSyncWithResult(t *testing.T, repo *Repo, recorder *syncAuditRecorder, memo string) *MergeResult {
	context := map[string]interface{}{SyncAuditContextKey: recorder}
	finishIndex := BeginSyncAudit(context, "dejavu.index", nil)
	if _, err := repo.Index(memo, true, context); err != nil {
		finishIndex(err)
		t.Fatal(err)
	}
	finishIndex(nil)
	result, _, err := repo.Sync(context)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSyncAuditConflictDeleteAndRecreate(t *testing.T) {
	t.Run("content conflict", func(t *testing.T) {
		fixture := newSyncAuditFixture(t)
		a, b := newConvergedAuditPair(t, fixture, "shared.txt", []byte("baseline"))
		if err := os.WriteFile(filepath.Join(b.DataPath, "shared.txt"), []byte("local edit"), 0644); err != nil {
			t.Fatal(err)
		}
		localTime := time.Now().Add(2 * time.Second)
		if err := os.Chtimes(filepath.Join(b.DataPath, "shared.txt"), localTime, localTime); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Index("local edit before remote publish", true, map[string]interface{}{}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(a.DataPath, "shared.txt"), []byte("remote edit"), 0644); err != nil {
			t.Fatal(err)
		}
		remoteTime := localTime.Add(2 * time.Second)
		if err := os.Chtimes(filepath.Join(a.DataPath, "shared.txt"), remoteTime, remoteTime); err != nil {
			t.Fatal(err)
		}
		if err := auditIndexAndSync(t, a, "remote edit"); err != nil {
			t.Fatal(err)
		}
		recorder := &syncAuditRecorder{scenario: "content_conflict", origin: time.Now()}
		b.cloud.(*syncAuditCloud).recorder = recorder
		result, _, err := b.Sync(map[string]interface{}{SyncAuditContextKey: recorder})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Conflicts) == 0 {
			t.Fatal("content conflict was not reported")
		}
		fixture.write(recorder)
	})

	t.Run("remote delete", func(t *testing.T) {
		fixture := newSyncAuditFixture(t)
		a, b := newConvergedAuditPair(t, fixture, "deleted.txt", []byte("delete me"))
		if err := os.Remove(filepath.Join(a.DataPath, "deleted.txt")); err != nil {
			t.Fatal(err)
		}
		if err := auditIndexAndSync(t, a, "remote delete"); err != nil {
			t.Fatal(err)
		}
		recorder := &syncAuditRecorder{scenario: "remote_delete", origin: time.Now()}
		b.cloud.(*syncAuditCloud).recorder = recorder
		result := auditSyncWithResult(t, b, recorder, "consume delete")
		if len(result.Removes) == 0 {
			t.Fatal("remote delete was not reported")
		}
		if _, err := os.Stat(filepath.Join(b.DataPath, "deleted.txt")); !os.IsNotExist(err) {
			t.Fatalf("deleted file remains: %v", err)
		}
		fixture.write(recorder)
	})

	t.Run("delete then recreate", func(t *testing.T) {
		fixture := newSyncAuditFixture(t)
		a, b := newConvergedAuditPair(t, fixture, "recreated.txt", []byte("old"))
		if err := os.Remove(filepath.Join(a.DataPath, "recreated.txt")); err != nil {
			t.Fatal(err)
		}
		if err := auditIndexAndSync(t, a, "delete old"); err != nil {
			t.Fatal(err)
		}
		if err := auditIndexAndSync(t, b, "consume delete"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b.DataPath, "recreated.txt"), []byte("new"), 0644); err != nil {
			t.Fatal(err)
		}
		recreatedTime := time.Now().Add(2 * time.Second)
		if err := os.Chtimes(filepath.Join(b.DataPath, "recreated.txt"), recreatedTime, recreatedTime); err != nil {
			t.Fatal(err)
		}
		recorder := &syncAuditRecorder{scenario: "delete_then_recreate", origin: time.Now()}
		b.cloud.(*syncAuditCloud).recorder = recorder
		auditSyncWithResult(t, b, recorder, "recreate path")
		if err := auditIndexAndSync(t, a, "consume recreation"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(a.DataPath, "recreated.txt"))
		if err != nil || string(data) != "new" {
			t.Fatalf("recreated file did not converge: %q %v", data, err)
		}
		fixture.write(recorder)
	})
}

func TestSyncAuditLatestPublicationFailurePreservesPreviousLatest(t *testing.T) {
	fixture := newSyncAuditFixture(t)
	repo, _ := fixture.repo("latest-failure", "latest_baseline", nil)
	if err := os.WriteFile(filepath.Join(repo.DataPath, "doc.txt"), []byte("baseline"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := auditIndexAndSync(t, repo, "baseline"); err != nil {
		t.Fatal(err)
	}
	before, err := repo.GetCloudLatest(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo.DataPath, "doc.txt"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	changedTime := time.Now().Add(2 * time.Second)
	if err = os.Chtimes(filepath.Join(repo.DataPath, "doc.txt"), changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Index("changed", true, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	recorder := &syncAuditRecorder{scenario: "latest_publication_failure", origin: time.Now()}
	audit := repo.cloud.(*syncAuditCloud)
	audit.recorder = recorder
	audit.failObject = map[string]int64{"cloud.upload_object\x00refs/latest": 1}
	if _, _, syncErr := repo.Sync(map[string]interface{}{SyncAuditContextKey: recorder}); syncErr == nil {
		t.Fatal("expected refs/latest publication failure")
	}
	afterFailure, err := repo.GetCloudLatest(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.ID != before.ID {
		t.Fatalf("failed publication advanced latest from %s to %s", before.ID, afterFailure.ID)
	}
	fixture.write(recorder)

	retry := &syncAuditRecorder{scenario: "latest_publication_retry", origin: time.Now()}
	audit.recorder = retry
	audit.failObject = nil
	if _, _, err = repo.Sync(map[string]interface{}{SyncAuditContextKey: retry}); err != nil {
		t.Fatal(err)
	}
	afterRetry, err := repo.GetCloudLatest(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if afterRetry.ID == before.ID {
		t.Fatal("retry did not publish the changed latest")
	}
	fixture.write(retry)
}

func TestCloudMissingObjectRepairWorkerEligibility(t *testing.T) {
	localRepo := &Repo{cloud: cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{}})}
	if localRepo.shouldUploadCloudMissingObjects() {
		t.Fatal("local cloud must not start the official-cloud repair worker")
	}

	officialRepo := &Repo{cloud: &cloud.SiYuan{BaseCloud: &cloud.BaseCloud{Conf: &cloud.Conf{}}}}
	if !officialRepo.shouldUploadCloudMissingObjects() {
		t.Fatal("official cloud should run repair once")
	}
	if !officialRepo.cloudMissingObjectsUploaded.CompareAndSwap(false, true) {
		t.Fatal("first repair claim failed")
	}
	if officialRepo.shouldUploadCloudMissingObjects() {
		t.Fatal("official cloud repair worker must be repository-scoped and one-shot")
	}
}

func TestSyncAuditPublicationBoundaryFailuresRemainRetryable(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		object    func(string) bool
	}{
		{name: "chunk object", operation: "cloud.upload_object", object: func(object string) bool { return strings.HasPrefix(object, "objects/") }},
		{name: "index", operation: "cloud.upload_object", object: func(object string) bool { return strings.HasPrefix(object, "indexes/") }},
		{name: "index catalog", operation: "cloud.upload_object", object: func(object string) bool { return object == "indexes-v2.json" }},
		{name: "object readback", operation: "cloud.download_object", object: func(object string) bool { return strings.HasPrefix(object, "objects/") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSyncAuditFixture(t)
			repo, _ := fixture.repo("boundary", "boundary_baseline", nil)
			if err := os.WriteFile(filepath.Join(repo.DataPath, "doc.txt"), []byte("baseline"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := auditIndexAndSync(t, repo, "baseline"); err != nil {
				t.Fatal(err)
			}
			before, err := repo.GetCloudLatest(map[string]interface{}{})
			if err != nil {
				t.Fatal(err)
			}
			changed := []byte("changed-" + test.name)
			if err = os.WriteFile(filepath.Join(repo.DataPath, "doc.txt"), changed, 0644); err != nil {
				t.Fatal(err)
			}
			changedTime := time.Now().Add(2 * time.Second)
			if err = os.Chtimes(filepath.Join(repo.DataPath, "doc.txt"), changedTime, changedTime); err != nil {
				t.Fatal(err)
			}
			if _, err = repo.Index("changed", true, map[string]interface{}{}); err != nil {
				t.Fatal(err)
			}
			recorder := &syncAuditRecorder{scenario: "failure_" + strings.ReplaceAll(test.name, " ", "_"), origin: time.Now()}
			audit := repo.cloud.(*syncAuditCloud)
			audit.recorder = recorder
			audit.failPredicate = func(operation, object string) bool { return operation == test.operation && test.object(object) }
			if _, _, syncErr := repo.Sync(map[string]interface{}{SyncAuditContextKey: recorder}); syncErr == nil {
				t.Fatalf("expected %s failure", test.name)
			}
			afterFailure, latestErr := repo.GetCloudLatest(map[string]interface{}{})
			if latestErr != nil || afterFailure.ID != before.ID {
				t.Fatalf("%s failure changed latest: before=%s after=%v err=%v", test.name, before.ID, afterFailure, latestErr)
			}
			fixture.write(recorder)
			audit.failPredicate = nil
			retry := &syncAuditRecorder{scenario: "retry_" + strings.ReplaceAll(test.name, " ", "_"), origin: time.Now()}
			audit.recorder = retry
			if _, _, err = repo.Sync(map[string]interface{}{SyncAuditContextKey: retry}); err != nil {
				t.Fatalf("%s retry failed: %v", test.name, err)
			}
			afterRetry, latestErr := repo.GetCloudLatest(map[string]interface{}{})
			if latestErr != nil || afterRetry.ID == before.ID {
				t.Fatalf("%s retry did not advance latest: before=%s after=%v err=%v", test.name, before.ID, afterRetry, latestErr)
			}
			fixture.write(retry)
		})
	}
}

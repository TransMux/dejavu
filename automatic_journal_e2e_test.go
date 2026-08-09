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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/encryption"
)

type failOnceObjectCloud struct {
	cloud.Cloud
	mu       sync.Mutex
	failAt   int
	attempts int
	failed   bool
	uploads  []string
}

func (c *failOnceObjectCloud) UploadObject(key string, overwrite bool) (int64, error) {
	c.mu.Lock()
	if strings.HasPrefix(filepath.ToSlash(key), "objects/") {
		c.attempts++
		c.uploads = append(c.uploads, key)
		if !c.failed && c.attempts == c.failAt {
			c.failed = true
			c.mu.Unlock()
			return 0, errors.New("injected automatic upload interruption")
		}
	}
	c.mu.Unlock()
	return c.Cloud.UploadObject(key, overwrite)
}

func TestAutomaticUploadJournalResumesAcrossTwoRepos(t *testing.T) {
	root := t.TempDir()
	cloudEndpoint := filepath.Join(root, "cloud")
	key, err := encryption.KDF("automatic-journal-password", "automatic-journal-salt")
	if err != nil {
		t.Fatal(err)
	}
	newRepo := func(name string) (*Repo, *cloud.Local) {
		local := cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: "automatic-journal", AvailableSize: 1 << 40,
			Local: &cloud.ConfLocal{Endpoint: cloudEndpoint, ConcurrentReqs: 1}}})
		base := filepath.Join(root, name)
		repo, newErr := NewRepo(filepath.Join(base, "data"), filepath.Join(base, "repo"), filepath.Join(base, "history"),
			filepath.Join(base, "temp"), name, name, runtime.GOOS, key, nil, local)
		if newErr != nil {
			t.Fatal(newErr)
		}
		return repo, local
	}
	a, baseCloud := newRepo("device-a")
	b, _ := newRepo("device-b")
	if err = os.MkdirAll(a.DataPath, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.txt", "two.txt"} {
		filePath := filepath.Join(a.DataPath, name)
		if err = os.WriteFile(filePath, []byte("baseline "+name), 0644); err != nil {
			t.Fatal(err)
		}
		if err = os.Chtimes(filePath, time.Unix(1700000000, 0), time.Unix(1700000000, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = a.Index("baseline", true, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = a.Sync(map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	copyTestDir(t, a.DataPath, b.DataPath)
	copyTestDir(t, a.Path, b.Path)
	b, err = NewRepo(b.DataPath, b.Path, b.HistoryPath, b.TempPath, "device-b", "device-b", runtime.GOOS, key, nil, b.cloud)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = b.Sync(map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.txt", "two.txt"} {
		filePath := filepath.Join(a.DataPath, name)
		if err = os.WriteFile(filePath, []byte("changed content "+name), 0644); err != nil {
			t.Fatal(err)
		}
		if err = os.Chtimes(filePath, time.Unix(1700000010, 0), time.Unix(1700000010, 0)); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := a.Index("interrupted", true, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	failing := &failOnceObjectCloud{Cloud: baseCloud, failAt: 2}
	a.cloud = failing
	if _, _, err = a.Sync(map[string]interface{}{}); err == nil {
		t.Fatal("expected deterministic automatic upload interruption")
	}
	if _, err = os.Stat(filepath.Join(a.Path, "upload-transactions", "current.json")); err != nil {
		t.Fatalf("durable automatic transaction missing: %v", err)
	}
	failing.mu.Lock()
	firstCompletedPath := failing.uploads[0]
	failing.uploads = nil
	failing.failAt = 0
	failing.mu.Unlock()
	if _, _, err = a.Sync(map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(a.Path, "upload-transactions", "current.json")); !os.IsNotExist(err) {
		t.Fatalf("completed automatic transaction remains: %v", err)
	}
	failing.mu.Lock()
	defer failing.mu.Unlock()
	for _, uploaded := range failing.uploads {
		if uploaded == firstCompletedPath {
			t.Fatalf("verified object %s was uploaded again during resume", uploaded)
		}
	}
	cloudLatest, err := a.GetCloudLatest(map[string]interface{}{})
	if err != nil || cloudLatest.ID != latest.ID {
		t.Fatalf("cloud latest=%v err=%v, want %s", cloudLatest, err, latest.ID)
	}
}

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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/88250/gulu"
	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/dejavu/entity"
)

const maxLazyRepairQueueItems = 4096

var lazyRepairQueueMu sync.Mutex

type lazyRepairItem struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Reason   string `json:"reason"`
	Attempts int    `json:"attempts"`
	Updated  int64  `json:"updated"`
}

func (repo *Repo) lazyRepairQueuePath() string {
	return filepath.Join(repo.Path, "repair-queue.json")
}

func (repo *Repo) enqueueLazyRepair(id, kind string, reason error) error {
	if err := validateChunkID(id); nil != err {
		return err
	}
	if kind != "chunk" && kind != "file" {
		return fmt.Errorf("invalid lazy repair kind [%s]", kind)
	}
	lazyRepairQueueMu.Lock()
	defer lazyRepairQueueMu.Unlock()
	items, err := repo.loadLazyRepairQueue()
	if nil != err {
		return err
	}
	if len(items) >= maxLazyRepairQueueItems {
		return fmt.Errorf("lazy repair queue limit [%d] reached", maxLazyRepairQueueItems)
	}
	for _, item := range items {
		if item.ID == id && item.Kind == kind {
			item.Reason = reason.Error()
			item.Updated = time.Now().UnixMilli()
			return repo.saveLazyRepairQueue(items)
		}
	}
	items = append(items, &lazyRepairItem{ID: id, Kind: kind, Reason: reason.Error(), Updated: time.Now().UnixMilli()})
	return repo.saveLazyRepairQueue(items)
}

func (repo *Repo) loadLazyRepairQueue() ([]*lazyRepairItem, error) {
	data, err := os.ReadFile(repo.lazyRepairQueuePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if nil != err {
		return nil, err
	}
	var items []*lazyRepairItem
	if err = json.Unmarshal(data, &items); nil != err {
		return nil, err
	}
	return items, nil
}

func (repo *Repo) saveLazyRepairQueue(items []*lazyRepairItem) error {
	data, err := json.MarshalIndent(items, "", "  ")
	if nil != err {
		return err
	}
	return gulu.File.WriteFileSafer(repo.lazyRepairQueuePath(), data, 0644)
}

// ProcessLazyRepairQueue retries at most limit objects from trusted local
// repository bytes. Unrepairable entries remain queued for a later batch.
func (repo *Repo) ProcessLazyRepairQueue(limit int, context map[string]interface{}) (repaired int, err error) {
	lazyRepairQueueMu.Lock()
	defer lazyRepairQueueMu.Unlock()
	if limit < 1 {
		limit = 16
	}
	items, err := repo.loadLazyRepairQueue()
	if nil != err {
		return 0, err
	}
	traffic := &cloud.Traffic{}
	defer func() {
		if 0 < traffic.APIPut || 0 < traffic.APIGet {
			go repo.cloud.AddTraffic(traffic)
		}
	}()
	remaining := make([]*lazyRepairItem, 0, len(items))
	for i, item := range items {
		if i >= limit {
			remaining = append(remaining, items[i:]...)
			break
		}
		if nil == item || (item.Kind != "chunk" && item.Kind != "file") {
			continue
		}
		item.Attempts++
		item.Updated = time.Now().UnixMilli()
		if idErr := validateChunkID(item.ID); nil != idErr {
			item.Reason = idErr.Error()
			remaining = append(remaining, item)
			continue
		}
		objectPath := filepath.ToSlash(filepath.Join("objects", item.ID[:2], item.ID[2:]))
		if localErr := repo.verifyLocalRepairObject(item.ID, item.Kind); nil != localErr {
			item.Reason = localErr.Error()
			remaining = append(remaining, item)
			continue
		}
		traffic.APIPut++
		uploadBytes, uploadErr := repo.cloud.UploadObject(objectPath, true)
		if nil != uploadErr {
			item.Reason = uploadErr.Error()
			remaining = append(remaining, item)
			continue
		}
		traffic.UploadBytes += uploadBytes
		var verifyErr error
		var verifyBytes int64
		var verifyGets int
		if item.Kind == "chunk" {
			verifyBytes, _, verifyGets, verifyErr = repo.verifyUploadedChunks([]string{item.ID}, context)
		} else {
			verifyBytes, _, verifyGets, verifyErr = repo.verifyUploadedFiles([]string{item.ID}, context)
		}
		traffic.DownloadBytes += verifyBytes
		traffic.APIGet += verifyGets
		if nil != verifyErr {
			item.Reason = verifyErr.Error()
			remaining = append(remaining, item)
			continue
		}
		repaired++
	}
	err = repo.saveLazyRepairQueue(remaining)
	return
}

func (repo *Repo) verifyLocalRepairObject(id, kind string) error {
	if kind == "chunk" {
		_, err := repo.store.GetChunk(id)
		return err
	}
	_, objectPath := repo.store.AbsPath(id)
	data, err := os.ReadFile(objectPath)
	if nil != err {
		return err
	}
	data, err = repo.store.decodeData(data)
	if nil != err {
		return err
	}
	actual := &entity.File{}
	if err = json.Unmarshal(data, actual); nil != err {
		return err
	}
	expected := entity.NewFile(actual.Path, actual.Size, actual.Updated)
	expected.Chunks = actual.Chunks
	if expected.ID != id || !reflect.DeepEqual(expected, actual) {
		return fmt.Errorf("local file metadata [%s] identity mismatch", id)
	}
	return nil
}

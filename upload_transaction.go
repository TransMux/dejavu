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
	"sort"
	"time"

	"github.com/88250/gulu"
)

type uploadTransaction struct {
	Version         int               `json:"version"`
	IndexID         string            `json:"indexID"`
	PlannedChunks   []string          `json:"plannedChunks"`
	PlannedFiles    []string          `json:"plannedFiles"`
	CompletedChunks map[string]bool   `json:"completedChunks"`
	CompletedFiles  map[string]bool   `json:"completedFiles"`
	Failed          map[string]string `json:"failed,omitempty"`
	SkippedChunks   []string          `json:"skippedChunks,omitempty"`
	SkippedFiles    []string          `json:"skippedFiles,omitempty"`
	Updated         int64             `json:"updated"`
	State           string            `json:"state"`
}

func completedUploadIDs(completed map[string]bool) []string {
	ret := make([]string, 0, len(completed))
	for id, done := range completed {
		if done {
			ret = append(ret, id)
		}
	}
	sort.Strings(ret)
	return ret
}

func (repo *Repo) verifyAndRecordUploadIDs(tx *uploadTransaction, chunks bool, ids []string, traffic *TrafficStat,
	context map[string]interface{}) (verifyErr, saveErr error) {
	completed := tx.CompletedFiles
	if chunks {
		completed = tx.CompletedChunks
	}
	for _, id := range uniqueUploadIDs(ids) {
		var bytes int64
		var verified, gets int
		var err error
		if chunks {
			bytes, verified, gets, err = repo.verifyUploadedChunks([]string{id}, context)
			traffic.DownloadChunkCount += verified
		} else {
			bytes, verified, gets, err = repo.verifyUploadedFiles([]string{id}, context)
			traffic.DownloadFileCount += verified
		}
		traffic.DownloadBytes += bytes
		traffic.APIGet += gets
		if nil != err {
			delete(completed, id)
			tx.Failed[id] = err.Error()
			kind := "file"
			if chunks {
				kind = "chunk"
			}
			if queueErr := repo.enqueueLazyRepair(id, kind, err); nil != queueErr && nil == saveErr {
				saveErr = queueErr
			}
			if nil == verifyErr {
				verifyErr = err
			}
			continue
		}
		completed[id] = true
		delete(tx.Failed, id)
	}
	if txSaveErr := repo.saveUploadTransaction(tx); nil == saveErr {
		saveErr = txSaveErr
	}
	return
}

func (repo *Repo) beginUploadTransaction(indexID string, chunks, files []string) (*uploadTransaction, error) {
	if err := validateUploadTransactionID(indexID); nil != err {
		return nil, err
	}
	chunks = uniqueUploadIDs(chunks)
	files = uniqueUploadIDs(files)
	current := filepath.Join(repo.Path, "upload-transactions", "current.json")
	tx := &uploadTransaction{}
	if data, err := os.ReadFile(current); nil == err {
		if err = json.Unmarshal(data, tx); nil != err {
			return nil, fmt.Errorf("read upload transaction: %w", err)
		}
	}
	if tx.IndexID != indexID {
		tx = &uploadTransaction{Version: 1, IndexID: indexID, CompletedChunks: map[string]bool{}, CompletedFiles: map[string]bool{}, Failed: map[string]string{}}
	}
	if nil == tx.CompletedChunks {
		tx.CompletedChunks = map[string]bool{}
	}
	if nil == tx.CompletedFiles {
		tx.CompletedFiles = map[string]bool{}
	}
	if nil == tx.Failed {
		tx.Failed = map[string]string{}
	}
	tx.PlannedChunks = append([]string(nil), chunks...)
	tx.PlannedFiles = append([]string(nil), files...)
	tx.CompletedChunks = retainPlannedUploadIDs(tx.CompletedChunks, tx.PlannedChunks)
	tx.CompletedFiles = retainPlannedUploadIDs(tx.CompletedFiles, tx.PlannedFiles)
	_, tx.SkippedChunks = pendingUploadIDs(tx.PlannedChunks, tx.CompletedChunks)
	_, tx.SkippedFiles = pendingUploadIDs(tx.PlannedFiles, tx.CompletedFiles)
	tx.State = "uploading"
	return tx, repo.saveUploadTransaction(tx)
}

func retainPlannedUploadIDs(completed map[string]bool, planned []string) map[string]bool {
	ret := make(map[string]bool, len(completed))
	for _, id := range planned {
		if completed[id] {
			ret[id] = true
		}
	}
	return ret
}

func validateUploadTransactionID(id string) error {
	if 40 != len(id) {
		return fmt.Errorf("invalid upload transaction index ID [%s]", id)
	}
	for _, c := range id {
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return fmt.Errorf("invalid upload transaction index ID [%s]", id)
		}
	}
	return nil
}

func uniqueUploadIDs(ids []string) []string {
	ret := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			ret = append(ret, id)
		}
	}
	return ret
}

func (repo *Repo) saveUploadTransaction(tx *uploadTransaction) error {
	tx.Updated = time.Now().UnixMilli()
	data, err := json.MarshalIndent(tx, "", "  ")
	if nil != err {
		return err
	}
	path := filepath.Join(repo.Path, "upload-transactions", "current.json")
	if err = os.MkdirAll(filepath.Dir(path), 0755); nil != err {
		return err
	}
	return gulu.File.WriteFileSafer(path, data, 0644)
}

func pendingUploadIDs(planned []string, completed map[string]bool) (pending, skipped []string) {
	for _, id := range planned {
		if completed[id] {
			skipped = append(skipped, id)
		} else {
			pending = append(pending, id)
		}
	}
	return
}

func (repo *Repo) recordUploadResult(tx *uploadTransaction, chunks bool, result concurrentObjectTransferResult) error {
	completed := tx.CompletedFiles
	if chunks {
		completed = tx.CompletedChunks
	}
	for _, id := range result.completedIDs {
		completed[id] = true
		delete(tx.Failed, id)
	}
	for id, failure := range result.failedIDs {
		tx.Failed[id] = failure.Error()
	}
	return repo.saveUploadTransaction(tx)
}

func (repo *Repo) completeUploadTransaction(tx *uploadTransaction) error {
	tx.State = "completed"
	if err := repo.saveUploadTransaction(tx); nil != err {
		return err
	}
	data, err := os.ReadFile(filepath.Join(repo.Path, "upload-transactions", "current.json"))
	if nil != err {
		return err
	}
	summary := filepath.Join(repo.Path, "upload-transactions", "completed", tx.IndexID+".json")
	if err = os.MkdirAll(filepath.Dir(summary), 0755); nil != err {
		return err
	}
	if err = gulu.File.WriteFileSafer(summary, data, 0644); nil != err {
		return err
	}
	return os.Remove(filepath.Join(repo.Path, "upload-transactions", "current.json"))
}

func (repo *Repo) completeUploadTransactionForIndex(indexID string) error {
	data, err := os.ReadFile(filepath.Join(repo.Path, "upload-transactions", "current.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if nil != err {
		return err
	}
	tx := &uploadTransaction{}
	if err = json.Unmarshal(data, tx); nil != err {
		return err
	}
	if tx.IndexID != indexID {
		return nil
	}
	return repo.completeUploadTransaction(tx)
}

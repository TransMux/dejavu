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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/dejavu/util"
)

func TestRunConcurrentSyncTransfersAggregatesAfterBothWorkers(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	download := func(traffic *TrafficStat) error {
		started <- "download"
		<-release
		traffic.DownloadFileCount = 2
		traffic.DownloadChunkCount = 3
		traffic.DownloadBytes = 5
		traffic.APIGet = 7
		return nil
	}
	upload := func(traffic *TrafficStat) error {
		started <- "upload"
		<-release
		traffic.UploadFileCount = 11
		traffic.UploadChunkCount = 13
		traffic.UploadBytes = 17
		traffic.APIPut = 19
		return nil
	}

	done := make(chan struct {
		traffic *TrafficStat
		err     error
	}, 1)
	go func() {
		traffic, err := runConcurrentSyncTransfers(download, upload)
		done <- struct {
			traffic *TrafficStat
			err     error
		}{traffic: traffic, err: err}
	}()
	seen := map[string]bool{<-started: true, <-started: true}
	if !seen["download"] || !seen["upload"] {
		t.Fatalf("workers did not both reach the barrier: %#v", seen)
	}
	close(release)
	result := <-done
	if nil != result.err {
		t.Fatal(result.err)
	}
	if 2 != result.traffic.DownloadFileCount || 3 != result.traffic.DownloadChunkCount || 5 != result.traffic.DownloadBytes ||
		11 != result.traffic.UploadFileCount || 13 != result.traffic.UploadChunkCount || 17 != result.traffic.UploadBytes ||
		7 != result.traffic.APIGet || 19 != result.traffic.APIPut {
		t.Fatalf("unexpected aggregate traffic: %#v", result.traffic)
	}
}

func TestUploadTransactionRejectsMalformedAndUnsafeState(t *testing.T) {
	repo := &Repo{Path: t.TempDir()}
	transactionDir := filepath.Join(repo.Path, "upload-transactions")
	if err := os.MkdirAll(transactionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transactionDir, "current.json"), []byte(`{"indexID":"0123456789abcdef0123456789abcdef01234567"`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.beginUploadTransaction("0123456789abcdef0123456789abcdef01234567", nil, nil); err == nil {
		t.Fatal("malformed durable transaction was silently accepted")
	}
	if err := os.Remove(filepath.Join(transactionDir, "current.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.beginUploadTransaction("../../outside", nil, nil); err == nil {
		t.Fatal("unsafe transaction index ID was accepted")
	}
	tx, err := repo.beginUploadTransaction("0123456789abcdef0123456789abcdef01234567", []string{"a", "a", "b"}, []string{"f", "f"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.PlannedChunks) != 2 || len(tx.PlannedFiles) != 1 {
		t.Fatalf("duplicate plan was not normalized: %#v %#v", tx.PlannedChunks, tx.PlannedFiles)
	}
}

func TestUploadTransactionDemotesMissingRemoteConfirmation(t *testing.T) {
	repo := newLazyTestRepo(t)
	repo.cloud = cloud.NewLocal(&cloud.BaseCloud{Conf: &cloud.Conf{Dir: "journal-revalidate", AvailableSize: 1 << 30,
		Local: &cloud.ConfLocal{Endpoint: t.TempDir(), ConcurrentReqs: 1}}})
	indexID := "0123456789abcdef0123456789abcdef01234567"
	chunkID := util.Hash([]byte("previously uploaded"))
	tx, err := repo.beginUploadTransaction(indexID, []string{chunkID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tx.CompletedChunks[chunkID] = true
	if err = repo.saveUploadTransaction(tx); err != nil {
		t.Fatal(err)
	}
	traffic := &TrafficStat{}
	verifyErr, saveErr := repo.verifyAndRecordUploadIDs(tx, true, completedUploadIDs(tx.CompletedChunks), traffic, map[string]interface{}{})
	if verifyErr == nil || saveErr != nil {
		t.Fatalf("verifyErr=%v saveErr=%v", verifyErr, saveErr)
	}
	if tx.CompletedChunks[chunkID] || tx.Failed[chunkID] == "" || traffic.APIGet != 1 || traffic.DownloadChunkCount != 0 {
		t.Fatalf("missing confirmation not durably demoted: tx=%#v traffic=%#v", tx, traffic)
	}
	data, err := os.ReadFile(filepath.Join(repo.Path, "upload-transactions", "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	persisted := &uploadTransaction{}
	if err = json.Unmarshal(data, persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.CompletedChunks[chunkID] || persisted.Failed[chunkID] == "" {
		t.Fatalf("demotion was not persisted: %#v", persisted)
	}
}

func TestRunConcurrentSyncTransfersUsesStableErrorPriority(t *testing.T) {
	downloadErr := errors.New("download failed")
	uploadErr := errors.New("upload failed")
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	task := func(err error, apiGet, apiPut int) syncTransferTask {
		return func(traffic *TrafficStat) error {
			started <- struct{}{}
			<-release
			traffic.APIGet = apiGet
			traffic.APIPut = apiPut
			return err
		}
	}

	done := make(chan struct {
		traffic *TrafficStat
		err     error
	}, 1)
	go func() {
		traffic, err := runConcurrentSyncTransfers(task(downloadErr, 1, 0), task(uploadErr, 0, 1))
		done <- struct {
			traffic *TrafficStat
			err     error
		}{traffic: traffic, err: err}
	}()
	<-started
	<-started
	close(release)
	result := <-done
	if !errors.Is(result.err, downloadErr) {
		t.Fatalf("error = %v, want stable download priority %v", result.err, downloadErr)
	}
	if 1 != result.traffic.APIGet || 1 != result.traffic.APIPut {
		t.Fatalf("partial traffic was not aggregated after both workers: %#v", result.traffic)
	}
}

func TestRunConcurrentSyncTransfersReturnsWorkerPanics(t *testing.T) {
	panicErr := errors.New("typed sync transfer panic")
	tests := []struct {
		name      string
		download  syncTransferTask
		upload    syncTransferTask
		wantError error
		wantText  string
	}{
		{
			name: "download typed panic",
			download: func(*TrafficStat) error {
				panic(panicErr)
			},
			upload:    func(*TrafficStat) error { return nil },
			wantError: panicErr,
		},
		{
			name:     "upload string panic",
			download: func(*TrafficStat) error { return nil },
			upload: func(*TrafficStat) error {
				panic("string sync transfer panic")
			},
			wantText: "string sync transfer panic",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := runConcurrentSyncTransfers(test.download, test.upload)
			if nil != test.wantError && !errors.Is(err, test.wantError) {
				t.Fatalf("error = %v, want wrapped %v", err, test.wantError)
			}
			if "" != test.wantText && (nil == err || !strings.Contains(err.Error(), test.wantText)) {
				t.Fatalf("error = %v, want text %q", err, test.wantText)
			}
		})
	}
}

func TestRunConcurrentSyncTransfersPreservesTrafficBeforePanic(t *testing.T) {
	panicErr := errors.New("panic after partial transfer")
	traffic, err := runConcurrentSyncTransfers(func(ret *TrafficStat) error {
		ret.DownloadChunkCount = 2
		ret.DownloadBytes = 17
		ret.APIGet = 3
		panic(panicErr)
	}, func(ret *TrafficStat) error {
		ret.UploadFileCount = 5
		ret.UploadBytes = 19
		ret.APIPut = 7
		return nil
	})
	if !errors.Is(err, panicErr) {
		t.Fatalf("error = %v, want wrapped panic %v", err, panicErr)
	}
	if 2 != traffic.DownloadChunkCount || 17 != traffic.DownloadBytes || 3 != traffic.APIGet ||
		5 != traffic.UploadFileCount || 19 != traffic.UploadBytes || 7 != traffic.APIPut {
		t.Fatalf("partial traffic was lost after panic: %#v", traffic)
	}
}

func TestRunConcurrentObjectTransfersWaitsAndPreservesCompletedTraffic(t *testing.T) {
	failure := errors.New("object transfer failed")
	started := make(chan string, 2)
	release := make(chan struct{})
	resultDone := make(chan concurrentObjectTransferResult, 1)
	go func() {
		resultDone <- runConcurrentObjectTransfers([]string{"success", "failure"}, 2, "test object",
			func(id string, _ int) (int64, error) {
				started <- id
				<-release
				if "failure" == id {
					return 0, failure
				}
				return 17, nil
			})
	}()
	seen := map[string]bool{<-started: true, <-started: true}
	if !seen["success"] || !seen["failure"] {
		close(release)
		t.Fatalf("workers did not both start: %#v", seen)
	}
	close(release)
	result := <-resultDone
	if !errors.Is(result.err, failure) {
		t.Fatalf("error = %v, want %v", result.err, failure)
	}
	if 17 != result.bytes || 1 != result.completed || 2 != result.attempted {
		t.Fatalf("unexpected partial result: %#v", result)
	}
}

func TestRunConcurrentObjectTransfersReportsObjectOutcomes(t *testing.T) {
	failure := errors.New("object unavailable")
	result := runConcurrentObjectTransfers([]string{"ok", "bad"}, 1, "test object",
		func(id string, _ int) (int64, error) {
			if id == "bad" {
				return 0, failure
			}
			return 7, nil
		})
	if len(result.completedIDs) != 1 || result.completedIDs[0] != "ok" {
		t.Fatalf("completed IDs = %#v", result.completedIDs)
	}
	if !errors.Is(result.failedIDs["bad"], failure) {
		t.Fatalf("failed IDs = %#v", result.failedIDs)
	}
}

func TestUploadTransactionPersistsAndResumesConfirmedObjects(t *testing.T) {
	repo := &Repo{Path: t.TempDir()}
	indexID := "0123456789abcdef0123456789abcdef01234567"
	tx, err := repo.beginUploadTransaction(indexID, []string{"chunk-a", "chunk-b"}, []string{"file-a"})
	if err != nil {
		t.Fatal(err)
	}
	result := concurrentObjectTransferResult{completedIDs: []string{"chunk-a"}, failedIDs: map[string]error{"chunk-b": errors.New("offline")}}
	if err = repo.recordUploadResult(tx, true, result); err != nil {
		t.Fatal(err)
	}
	resumed, err := repo.beginUploadTransaction(indexID, []string{"chunk-a", "chunk-b"}, []string{"file-a"})
	if err != nil {
		t.Fatal(err)
	}
	pending, skipped := pendingUploadIDs(resumed.PlannedChunks, resumed.CompletedChunks)
	if len(pending) != 1 || pending[0] != "chunk-b" || len(skipped) != 1 || skipped[0] != "chunk-a" {
		t.Fatalf("resume pending=%#v skipped=%#v", pending, skipped)
	}
	if resumed.Failed["chunk-b"] != "offline" {
		t.Fatalf("failed result was not durable: %#v", resumed.Failed)
	}
	if err = repo.completeUploadTransaction(resumed); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(repo.Path, "upload-transactions", "current.json")); !os.IsNotExist(err) {
		t.Fatalf("current transaction was not cleaned up: %v", err)
	}
	if _, err = os.Stat(filepath.Join(repo.Path, "upload-transactions", "completed", indexID+".json")); err != nil {
		t.Fatalf("completed summary missing: %v", err)
	}
}

func TestRunConcurrentObjectTransfersReportsSkippedIDsDeterministically(t *testing.T) {
	failure := errors.New("stop")
	result := runConcurrentObjectTransfers([]string{"bad", "z", "a"}, 1, "test object",
		func(id string, _ int) (int64, error) {
			if id == "bad" {
				return 0, failure
			}
			return 1, nil
		})
	if !errors.Is(result.failedIDs["bad"], failure) {
		t.Fatalf("failed IDs = %#v", result.failedIDs)
	}
	if len(result.skippedIDs) != 2 || result.skippedIDs[0] != "a" || result.skippedIDs[1] != "z" {
		t.Fatalf("skipped IDs = %#v", result.skippedIDs)
	}
}

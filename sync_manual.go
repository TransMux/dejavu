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
	"path/filepath"
	"sync"
	"time"

	"github.com/88250/gulu"
	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/logging"
)

func (repo *Repo) SyncDownload(context map[string]interface{}) (mergeResult *MergeResult, trafficStat *TrafficStat, err error) {
	finishAudit := BeginSyncAudit(context, "dejavu.sync.download", nil)
	defer func() { finishAudit(err) }()
	lock.Lock()
	defer lock.Unlock()
	unlockDeviceLocalFiles := repo.lockDeviceLocalSyncFiles()
	defer unlockDeviceLocalFiles()

	// 锁定云端，防止其他设备并发上传数据
	err = repo.tryLockCloud(repo.DeviceID, context)
	if nil != err {
		return
	}
	defer repo.unlockCloud(context)

	mergeResult = &MergeResult{Time: time.Now()}
	trafficStat = &TrafficStat{m: &sync.Mutex{}}

	// 获取本地最新索引
	latest, err := repo.Latest()
	if nil != err {
		logging.LogErrorf("get latest failed: %s", err)
		return
	}
	latest, _, err = repo.sanitizeStoredIndex(latest, "[Sync Download] Remove device-local files")
	if nil != err {
		logging.LogErrorf("sanitize latest failed: %s", err)
		return
	}

	// 从云端获取最新索引
	length, latestAPIGet, cloudLatest, err := repo.downloadCloudLatest(context)
	if nil != err {
		if !errors.Is(err, cloud.ErrCloudObjectNotFound) {
			logging.LogErrorf("download cloud latest failed: %s", err)
			return
		}
	}
	trafficStat.DownloadFileCount += latestAPIGet
	trafficStat.DownloadBytes += length
	trafficStat.APIGet += latestAPIGet

	if cloudLatest.ID == latest.ID || "" == cloudLatest.ID {
		// 数据一致或者云端为空，直接返回
		return
	}

	// 计算本地缺失的文件
	fetchFileIDs, err := repo.localNotFoundFiles(cloudLatest.Files)
	if nil != err {
		logging.LogErrorf("get local not found files failed: %s", err)
		return
	}

	// 从云端下载缺失文件并入库
	downloadResult, fetchedFiles := repo.downloadCloudFilesPutDetailed(fetchFileIDs, context)
	trafficStat.DownloadFileCount += downloadResult.completed
	trafficStat.DownloadBytes += downloadResult.bytes
	trafficStat.APIGet += downloadResult.attempted
	if nil != downloadResult.err {
		err = downloadResult.err
		logging.LogErrorf("download cloud files put failed: %s", err)
		return
	}

	// 组装还原云端最新文件列表
	cloudLatestFiles, err := repo.getFiles(cloudLatest.Files)
	if nil != err {
		logging.LogErrorf("get cloud latest files failed: %s", err)
		return
	}
	cloudLazyFiles, lazyErr := repo.getFilesWithCloudFallback(cloudLatest.LazyFiles, context)
	if nil != lazyErr {
		logging.LogErrorf("get cloud lazy files failed: %s", lazyErr)
		return nil, nil, lazyErr
	}
	var ignoredCloudDeviceLocalFiles bool
	cloudLatest, ignoredCloudDeviceLocalFiles, err = repo.sanitizeIndexFiles(cloudLatest, cloudLatestFiles, cloudLazyFiles,
		"[Sync Download] Remove device-local cloud files")
	if nil != err {
		logging.LogErrorf("sanitize cloud latest failed: %s", err)
		return nil, nil, err
	}
	cloudLatestFiles, _ = repo.filterProtectedSyncFiles(cloudLatestFiles)
	cloudLazyFiles, _ = repo.filterProtectedSyncFiles(cloudLazyFiles)

	// 处理懒加载文件：只更新清单，不参与常规同步流程
	if len(cloudLatest.LazyFiles) > 0 {
		err = repo.updateLazyManifestFromCloudIndex(cloudLatest.LazyFiles, context)
		if nil != err {
			logging.LogErrorf("update lazy manifest from cloud index failed: %s", err)
			return nil, nil, err
		}
	}

	// 如果本地未启用懒加载，需要将云端的懒加载文件当作普通文件处理
	if !repo.lazyLoadEnabled && len(cloudLatest.LazyFiles) > 0 {
		cloudLatestFiles = append(cloudLatestFiles, cloudLazyFiles...)
	}

	// 所有文件都是普通文件（懒加载文件已单独处理）
	normalFiles := cloudLatestFiles

	// 只从普通文件列表中得到去重后的分块列表
	cloudChunkIDs := repo.getChunks(normalFiles)

	// 计算本地缺失的分块（只包含普通文件的chunks）
	fetchChunkIDs, err := repo.localNotFoundChunks(cloudChunkIDs)
	if nil != err {
		logging.LogErrorf("get local not found chunks failed: %s", err)
		return
	}

	// 从云端下载缺失分块并入库（只下载普通文件的chunks）
	downloadResult = repo.downloadCloudChunksPutDetailed(fetchChunkIDs, context)
	trafficStat.DownloadBytes += downloadResult.bytes
	trafficStat.DownloadChunkCount += downloadResult.completed
	trafficStat.APIGet += downloadResult.attempted - downloadResult.peerCount
	trafficStat.PeerDownloadBytes += downloadResult.peerBytes
	trafficStat.PeerDownloadChunkCount += downloadResult.peerCount
	trafficStat.PeerFallbackCount += downloadResult.peerFallbackCount
	if nil != downloadResult.err {
		err = downloadResult.err
		logging.LogErrorf("download cloud chunks put failed: %s", err)
		return
	}

	// 计算本地相比上一个同步点的 upsert 和 remove 差异
	latestFiles, err := repo.getFiles(latest.Files)
	if nil != err {
		logging.LogErrorf("get latest files failed: %s", err)
		return
	}
	latestSync := repo.latestSync()
	latestSyncFiles, err := repo.getFiles(latestSync.Files)
	if nil != err {
		logging.LogErrorf("get latest sync files failed: %s", err)
		return
	}
	localUpserts, localRemoves := repo.diffUpsertRemove(latestFiles, latestSyncFiles, false)
	localUpserts, _ = repo.filterProtectedSyncFiles(localUpserts)
	localRemoves, _ = repo.filterProtectedSyncFiles(localRemoves)
	localChanged := 0 < len(localUpserts) || 0 < len(localRemoves) || ignoredCloudDeviceLocalFiles

	// 计算云端最新相比本地最新的 upsert 和 remove 差异
	// 在单向同步的情况下该结果可直接作为合并结果
	mergeResult.Upserts, mergeResult.Removes = repo.diffUpsertRemove(cloudLatestFiles, latestFiles, false)

	var fetchedFileIDs []string
	for _, fetchedFile := range fetchedFiles {
		fetchedFileIDs = append(fetchedFileIDs, fetchedFile.ID)
	}

	// 计算冲突的 upsert
	// 冲突的文件以云端 upsert 和 remove 为准
	for _, localUpsert := range localUpserts {
		if nil != repo.getFile(mergeResult.Upserts, localUpsert) || nil != repo.getFile(mergeResult.Removes, localUpsert) {
			mergeResult.Conflicts = append(mergeResult.Conflicts, localUpsert)
			logging.LogInfof("sync download conflict [%s, %s, %s]", localUpsert.ID, localUpsert.Path, time.UnixMilli(localUpsert.Updated).Format("2006-01-02 15:04:05"))
		}
	}

	// 冲突文件复制到数据历史文件夹
	repo.filterProtectedMergeResult(mergeResult)
	if 0 < len(mergeResult.Conflicts) {
		now := mergeResult.Time.Format("2006-01-02-150405")
		temp := filepath.Join(repo.TempPath, "repo", "sync", "conflicts", now)
		for i, file := range mergeResult.Conflicts {
			var checkoutTmp *entity.File
			checkoutTmp, err = repo.store.GetFile(file.ID)
			if nil != err {
				logging.LogErrorf("get file failed: %s", err)
				return
			}

			err = repo.checkoutFile(checkoutTmp, temp, i+1, len(mergeResult.Conflicts), context)
			if nil != err {
				logging.LogErrorf("checkout file failed: %s", err)
				return
			}

			absPath := filepath.Join(temp, checkoutTmp.Path)
			err = repo.genSyncHistory(now, file.Path, absPath)
			if nil != err {
				logging.LogErrorf("generate sync history failed: %s", err)
				err = ErrCloudGenerateConflictHistory
				return
			}
		}
	}

	// 数据变更后还原文件
	err = repo.restoreFiles(mergeResult, context)
	if nil != err {
		logging.LogErrorf("restore files failed: %s", err)
		return
	}

	// 处理合并
	err = repo.mergeSync(mergeResult, localChanged, false, latest, cloudLatest, cloudChunkIDs, trafficStat, context)
	if nil != err {
		logging.LogErrorf("merge sync failed: %s", err)
		return
	}

	// 统计流量
	go repo.cloud.AddTraffic(&cloud.Traffic{
		DownloadBytes: trafficStat.DownloadBytes,
		APIGet:        trafficStat.APIGet,
	})

	// 移除空目录
	gulu.File.RemoveEmptyDirs(repo.DataPath, removeEmptyDirExcludes...)
	return
}

func (repo *Repo) SyncUpload(context map[string]interface{}) (trafficStat *TrafficStat, err error) {
	finishAudit := BeginSyncAudit(context, "dejavu.sync.upload", nil)
	defer func() { finishAudit(err) }()
	lock.Lock()
	defer lock.Unlock()

	// 锁定云端，防止其他设备并发上传数据
	err = repo.tryLockCloud(repo.DeviceID, context)
	if nil != err {
		return
	}
	defer repo.unlockCloud(context)

	trafficStat = &TrafficStat{m: &sync.Mutex{}}

	latest, err := repo.Latest()
	if nil != err {
		logging.LogErrorf("get latest failed: %s", err)
		return
	}
	latest, _, err = repo.sanitizeStoredIndex(latest, "[Sync Upload] Remove device-local files")
	if nil != err {
		logging.LogErrorf("sanitize latest failed: %s", err)
		return
	}

	// 从云端获取最新索引
	length, latestAPIGet, cloudLatest, err := repo.downloadCloudLatest(context)
	if nil != err {
		if !errors.Is(err, cloud.ErrCloudObjectNotFound) {
			logging.LogErrorf("download cloud latest failed: %s", err)
			return
		}
	}
	trafficStat.DownloadFileCount += latestAPIGet
	trafficStat.DownloadBytes += length
	trafficStat.APIGet += latestAPIGet

	if cloudLatest.ID == latest.ID {
		// 数据一致，直接返回
		return
	}

	availableSize := repo.cloud.GetAvailableSize()
	if availableSize <= cloudLatest.Size || availableSize <= latest.Size {
		err = ErrCloudStorageSizeExceeded
		return
	}

	// 计算云端缺失的文件（包括普通文件和懒加载文件）
	uploadFiles, err := repo.localUpsertFiles(latest, cloudLatest, context)
	if nil != err {
		logging.LogErrorf("get local upsert files failed: %s", err)
		return
	}

	// 从文件列表中得到去重后的分块列表
	uploadChunkIDs := repo.getChunks(uploadFiles)
	uploadFileIDs := make([]string, 0, len(uploadFiles))
	for _, file := range uploadFiles {
		uploadFileIDs = append(uploadFileIDs, file.ID)
	}
	uploadTx, txErr := repo.beginUploadTransaction(latest.ID, uploadChunkIDs, uploadFileIDs)
	if nil != txErr {
		return trafficStat, txErr
	}
	// A durable confirmation is only a resume hint. Revalidate it because the
	// remote object may have disappeared or changed since the previous run.
	_, txErr = repo.verifyAndRecordUploadIDs(uploadTx, true, completedUploadIDs(uploadTx.CompletedChunks), trafficStat, context)
	if nil != txErr {
		return trafficStat, txErr
	}
	_, txErr = repo.verifyAndRecordUploadIDs(uploadTx, false, completedUploadIDs(uploadTx.CompletedFiles), trafficStat, context)
	if nil != txErr {
		return trafficStat, txErr
	}
	uploadChunkIDs, _ = pendingUploadIDs(uploadChunkIDs, uploadTx.CompletedChunks)

	// 这里暂时不计算云端缺失的分块了，因为目前计数云端缺失分块的代价太大
	//uploadChunkIDs, err = repo.cloud.GetChunks(uploadChunkIDs)
	//if nil != err {
	//	logging.LogErrorf("get cloud repo upload chunks failed: %s", err)
	//	return
	//}

	// 上传分块
	uploadResult := repo.uploadChunksDetailed(uploadChunkIDs, context)
	trafficStat.UploadChunkCount += uploadResult.completed
	trafficStat.UploadBytes += uploadResult.bytes
	trafficStat.APIPut += uploadResult.attempted
	verificationErr, saveErr := repo.verifyAndRecordUploadIDs(uploadTx, true, uploadResult.completedIDs, trafficStat, context)
	for id, failure := range uploadResult.failedIDs {
		uploadTx.Failed[id] = failure.Error()
	}
	if nil == saveErr {
		saveErr = repo.saveUploadTransaction(uploadTx)
	}
	if nil != saveErr {
		return trafficStat, saveErr
	}
	if nil != uploadResult.err {
		err = uploadResult.err
		logging.LogErrorf("upload chunks failed: %s", err)
		return
	}
	if nil != verificationErr {
		err = verificationErr
		return
	}

	// 上传文件
	pendingFileIDs, _ := pendingUploadIDs(uploadFileIDs, uploadTx.CompletedFiles)
	pendingFiles := make([]*entity.File, 0, len(pendingFileIDs))
	pendingFileSet := make(map[string]bool, len(pendingFileIDs))
	for _, id := range pendingFileIDs {
		pendingFileSet[id] = true
	}
	for _, file := range uploadFiles {
		if pendingFileSet[file.ID] {
			pendingFiles = append(pendingFiles, file)
		}
	}
	uploadResult = repo.uploadFilesDetailed(pendingFiles, context)
	trafficStat.UploadFileCount += uploadResult.completed
	trafficStat.UploadBytes += uploadResult.bytes
	trafficStat.APIPut += uploadResult.attempted
	verificationErr, saveErr = repo.verifyAndRecordUploadIDs(uploadTx, false, uploadResult.completedIDs, trafficStat, context)
	for id, failure := range uploadResult.failedIDs {
		uploadTx.Failed[id] = failure.Error()
	}
	if nil == saveErr {
		saveErr = repo.saveUploadTransaction(uploadTx)
	}
	if nil != saveErr {
		return trafficStat, saveErr
	}
	if nil != uploadResult.err {
		err = uploadResult.err
		logging.LogErrorf("upload files failed: %s", err)
		return
	}
	if nil != verificationErr {
		err = verificationErr
		return
	}

	// 更新云端索引信息
	err = repo.updateCloudIndexes(latest, trafficStat, context)
	if nil != err {
		logging.LogErrorf("update cloud indexes failed: %s", err)
		return
	}
	if err = repo.completeUploadTransaction(uploadTx); nil != err {
		return
	}

	// 更新本地同步点
	err = repo.UpdateLatestSync(latest)
	if nil != err {
		logging.LogErrorf("update latest sync failed: %s", err)
		return
	}

	// 统计流量
	go repo.cloud.AddTraffic(&cloud.Traffic{
		UploadBytes: trafficStat.UploadBytes,
		APIPut:      trafficStat.APIPut,
	})
	return
}

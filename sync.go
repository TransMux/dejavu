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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/88250/gulu"
	"github.com/88250/lute"
	"github.com/88250/lute/ast"
	"github.com/88250/lute/parse"
	"github.com/panjf2000/ants/v2"
	ignore "github.com/sabhiram/go-gitignore"
	"github.com/siyuan-note/dataparser"
	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/dejavu/util"
	"github.com/siyuan-note/eventbus"
	"github.com/siyuan-note/filelock"
	"github.com/siyuan-note/logging"
)

var (
	ErrCloudStorageSizeExceeded = errors.New("cloud storage limit size exceeded")
	ErrCloudBackupCountExceeded = errors.New("cloud backup count exceeded")

	ErrCloudGenerateConflictHistory = errors.New("generate conflict history failed")
)

const refUsedRepoPath = "/storage/ref-used.json"

type MergeResult struct {
	Time                        time.Time
	Upserts, Removes, Conflicts []*entity.File
	MergedLazyManifest          bool
	MergedPaths                 []string

	UpsertPetals []string // storage/petal/petals.json 中变更的插件，在思源中计算并填充
	RemovePetals []string // storage/petal/petals.json 中删除的插件，在思源中计算并填充
}

func (mr *MergeResult) DataChanged() bool {
	return len(mr.Upserts) > 0 || len(mr.Removes) > 0 || len(mr.Conflicts) > 0 || mr.MergedLazyManifest || len(mr.MergedPaths) > 0
}

type DownloadTrafficStat struct {
	DownloadFileCount      int
	DownloadChunkCount     int
	DownloadBytes          int64
	PeerDownloadFileCount  int
	PeerDownloadChunkCount int
	PeerDownloadBytes      int64
	PeerFallbackCount      int
}

type UploadTrafficStat struct {
	UploadFileCount  int
	UploadChunkCount int
	UploadBytes      int64
}

type APITrafficStat struct {
	APIGet int
	APIPut int
}

type TrafficStat struct {
	DownloadTrafficStat
	UploadTrafficStat
	APITrafficStat

	m *sync.Mutex
}

func (repo *Repo) GetSyncCloudFiles(cloudLatest *entity.Index, context map[string]interface{}) (fetchedFiles []*entity.File, err error) {
	lock.Lock()
	defer lock.Unlock()

	fetchedFiles, _, err = repo.getSyncCloudFiles(cloudLatest, context)
	return
}

func (repo *Repo) GetSyncCloudFilesWithTraffic(cloudLatest *entity.Index, context map[string]interface{}) (fetchedFiles []*entity.File, trafficStat *DownloadTrafficStat, err error) {
	lock.Lock()
	defer lock.Unlock()

	fetchedFiles, trafficStat, err = repo.getSyncCloudFiles(cloudLatest, context)
	return
}

func (repo *Repo) GetCloudLatest(context map[string]interface{}) (cloudLatest *entity.Index, err error) {
	lock.Lock()
	defer lock.Unlock()

	_, _, cloudLatest, err = repo.downloadCloudLatest(context)
	return
}

func (repo *Repo) Sync(context map[string]interface{}) (mergeResult *MergeResult, trafficStat *TrafficStat, err error) {
	unlockProcess := lockSyncProcess()
	defer unlockProcess()
	unlockDeviceLocalFiles := repo.lockDeviceLocalSyncFiles()
	defer unlockDeviceLocalFiles()
	var preflightTraffic *TrafficStat

	skipCloudPreflight := false
	if nil != context {
		skipCloudPreflight, _ = context["skipCloudPreflight"].(bool)
	}
	if !skipCloudPreflight {
		mergeResult = &MergeResult{Time: time.Now()}
		preflightTraffic = &TrafficStat{m: &sync.Mutex{}}
		trafficStat = preflightTraffic
		latest, latestErr := repo.Latest()
		if nil != latestErr {
			logging.LogErrorf("get latest failed: %s", latestErr)
			err = latestErr
			return
		}
		length, latestAPIGet, cloudLatest, latestErr := repo.downloadCloudLatest(context)
		if nil != latestErr {
			if !errors.Is(latestErr, cloud.ErrCloudObjectNotFound) {
				logging.LogErrorf("download cloud latest failed: %s", latestErr)
				err = latestErr
				return
			}
		}
		trafficStat.DownloadFileCount += latestAPIGet
		trafficStat.DownloadBytes += length
		trafficStat.APIGet += latestAPIGet
		latest, latestErr = repo.consumePublishedLazyManifestIfNeeded(latest, cloudLatest, context)
		if nil != latestErr {
			err = latestErr
			return
		}
		latest, _, latestErr = repo.sanitizeStoredIndex(latest, "[Sync Preflight] Remove device-local files")
		if nil != latestErr {
			logging.LogErrorf("sanitize latest failed: %s", latestErr)
			err = latestErr
			return
		}
		lazyManifestRequiresSync, manifestErr := repo.localLazyManifestRequiresSync(latest)
		if nil != manifestErr {
			err = manifestErr
			return
		}
		if cloudLatest.ID == latest.ID && !lazyManifestRequiresSync {
			// 数据一致时不获取云端锁，减少无变更同步的远程请求。
			return
		}
	}

	// 锁定云端，防止其他设备并发上传数据
	err = repo.tryLockCloud(repo.DeviceID, context)
	if nil != err {
		return
	}
	defer repo.unlockCloud(context)

	mergeResult, trafficStat, err = repo.sync(context)
	mergeTrafficStat(trafficStat, preflightTraffic)
	if e, ok := err.(*os.PathError); ok && isNoSuchFileOrDirErr(err) {
		p := e.Path
		if !strings.Contains(p, "objects") {
			return
		}

		// 索引时正常，但是上传时可能因为外部变更导致对象（文件或者分块）不存在，此时需要告知用户数据仓库已经损坏，需要重置数据仓库
		logging.LogErrorf("sync failed: %s", err)
		err = ErrRepoFatal
	}
	return
}

var lockSyncProcess = func() func() {
	lock.Lock()
	return lock.Unlock
}

func (repo *Repo) sync(context map[string]interface{}) (mergeResult *MergeResult, trafficStat *TrafficStat, err error) {
	mergeResult = &MergeResult{Time: time.Now()}
	trafficStat = &TrafficStat{m: &sync.Mutex{}}

	// 获取本地最新索引
	latest, err := repo.Latest()
	if nil != err {
		logging.LogErrorf("get latest failed: %s", err)
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
	latest, err = repo.consumePublishedLazyManifestIfNeeded(latest, cloudLatest, context)
	if nil != err {
		return
	}
	latest, _, err = repo.sanitizeStoredIndex(latest, "[Sync] Remove device-local files")
	if nil != err {
		logging.LogErrorf("sanitize latest failed: %s", err)
		return
	}

	var migrationPublished bool
	latest, migrationPublished, err = repo.publishLazyManifestRepositoryMigration(latest, cloudLatest, trafficStat, context)
	if nil != err {
		return
	}
	if migrationPublished {
		mergeResult = &MergeResult{Time: time.Now()}
		return
	}

	if cloudLatest.ID == latest.ID {
		// 数据一致，直接返回
		return
	}

	availableSize := repo.cloud.GetAvailableSize()
	if availableSize <= cloudLatest.Size || availableSize <= latest.Size {
		err = ErrCloudStorageSizeExceeded
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
	trafficStat.DownloadBytes += downloadResult.bytes
	trafficStat.DownloadFileCount += downloadResult.completed
	trafficStat.APIGet += downloadResult.attempted
	if nil != downloadResult.err {
		err = downloadResult.err
		logging.LogErrorf("download cloud files put failed: %s", err)
		return
	}

	// 执行数据同步
	err = repo.sync0(context, fetchedFiles, cloudLatest, latest, mergeResult, trafficStat)
	return
}

func (repo *Repo) localLazyManifestRequiresSync(latest *entity.Index) (bool, error) {
	if !repo.lazyLoadEnabled || nil == repo.lazyLoader {
		return false, nil
	}
	manifest, err := repo.lazyLoader.getManifest()
	if nil != err {
		return false, err
	}
	if err = validateLazyManifestFormat(manifest); nil != err {
		return false, err
	}
	if lazyManifestFormatCurrent != lazyManifestFormat(manifest) {
		return true, nil
	}
	manifestPath := repo.lazyLoader.getManifestPath()
	info, err := os.Stat(manifestPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if nil != err {
		return false, err
	}
	file := entity.NewFile(repo.relPath(manifestPath), info.Size(), info.ModTime().UnixMilli())
	return nil == latest || latest.LazyManifest != file.ID, nil
}

func (repo *Repo) readPublishedLazyManifest(index *entity.Index, context map[string]interface{}) (*LazyManifest, error) {
	if nil == index || "" == index.ID || "" == index.LazyManifest {
		return &LazyManifest{Version: lazyManifestFormatLegacy, Assets: map[string]*LazyAsset{}}, nil
	}
	files, err := repo.getFilesWithCloudFallback([]string{index.LazyManifest}, context)
	if nil != err {
		return nil, err
	}
	if 1 != len(files) {
		return nil, fmt.Errorf("published lazy manifest metadata [%s] not found", index.LazyManifest)
	}
	missingChunks, err := repo.localNotFoundChunks(files[0].Chunks)
	if nil != err {
		return nil, err
	}
	if 0 < len(missingChunks) {
		result := repo.downloadCloudChunksPutDetailed(missingChunks, context)
		if nil != result.err {
			return nil, result.err
		}
	}
	manifest, err := repo.checkoutLazyManifest(files[0], "published", context)
	if nil != err {
		return nil, fmt.Errorf("read published lazy manifest: %w", err)
	}
	return manifest, nil
}

func (repo *Repo) consumePublishedLazyManifestIfNeeded(latest, cloudLatest *entity.Index,
	context map[string]interface{}) (*entity.Index, error) {
	if !repo.lazyLoadEnabled || nil == repo.lazyLoader || nil == cloudLatest || "" == cloudLatest.ID ||
		repo.isLazyManifestHydrated(cloudLatest.LazyManifest) {
		return latest, nil
	}
	published, err := repo.readPublishedLazyManifest(cloudLatest, context)
	if nil != err {
		return nil, err
	}
	if lazyManifestFormatCurrent != lazyManifestFormat(published) {
		return latest, nil
	}
	if err = validateCurrentLazyManifestCatalog(published); nil != err {
		return nil, err
	}
	local, err := repo.lazyLoader.getManifest()
	if nil != err {
		return nil, err
	}
	merged := mergeLazyManifestAssets(local, published)
	canonicalized, err := repo.canonicalizeLazyManifestLocalDeltas(merged, published)
	if nil != err {
		return nil, err
	}
	identityChanges, err := repo.canonicalizeLazyManifestObjectIdentities(merged)
	if nil != err {
		return nil, err
	}
	canonicalized += identityChanges
	hydrated, err := repo.hydrateLazyManifestMetadata(merged, context)
	if nil != err {
		return nil, err
	}
	if err = repo.saveLazyManifestWithNewIdentity(merged); nil != err {
		return nil, fmt.Errorf("save consumed lazy manifest migration: %w", err)
	}
	if err = repo.advanceLazyManifestIdentityPast(cloudLatest.LazyManifest); nil != err {
		return nil, fmt.Errorf("separate consumed lazy manifest identity: %w", err)
	}
	if err = repo.advanceLazyManifestIdentityPastCloudObjects(); nil != err {
		return nil, fmt.Errorf("reserve consumed lazy manifest identity: %w", err)
	}
	latest, err = repo.index("[Sync] Consume lazy manifest repository migration", false, context)
	if nil != err {
		return nil, err
	}
	if err = repo.markLazyManifestHydrated(cloudLatest.LazyManifest); nil != err {
		return nil, fmt.Errorf("mark consumed lazy manifest migration: %w", err)
	}
	logging.LogInfof("lazy manifest repository migration consumed [assets=%d, hydrated=%d, localDeltas=%d, latest=%s]",
		len(merged.Assets), hydrated, canonicalized, cloudLatest.ID)
	return latest, nil
}

func (repo *Repo) advanceLazyManifestIdentityPastCloudObjects() error {
	manifestPath := repo.lazyLoader.getManifestPath()
	for {
		identity, err := repo.lazyManifestIdentity()
		if nil != err {
			return err
		}
		_, err = repo.cloud.DownloadObject(path.Join("objects", identity[:2], identity[2:]))
		if errors.Is(err, cloud.ErrCloudObjectNotFound) {
			return nil
		}
		if nil != err {
			return err
		}
		info, err := os.Stat(manifestPath)
		if nil != err {
			return err
		}
		updated := info.ModTime().Add(time.Second)
		if err = os.Chtimes(manifestPath, updated, updated); nil != err {
			return err
		}
	}
}

func (repo *Repo) publishLazyManifestRepositoryMigration(latest, cloudLatest *entity.Index, trafficStat *TrafficStat,
	context map[string]interface{}) (ret *entity.Index, published bool, err error) {
	ret = latest
	if !repo.lazyLoadEnabled || nil == repo.lazyLoader || nil == cloudLatest || "" == cloudLatest.ID {
		return
	}
	publishedManifest, err := repo.readPublishedLazyManifest(cloudLatest, context)
	if nil != err {
		return nil, false, err
	}
	if err = validateLazyManifestFormat(publishedManifest); nil != err {
		return nil, false, err
	}
	format := lazyManifestFormat(publishedManifest)
	if lazyManifestFormatCurrent == format {
		ret, err = repo.consumePublishedLazyManifestIfNeeded(latest, cloudLatest, context)
		return ret, false, err
	}

	manifest, err := repo.lazyLoader.getManifest()
	if nil != err {
		return nil, false, err
	}
	if err = validateLazyManifestFormat(manifest); nil != err {
		return nil, false, err
	}
	start := time.Now()
	logging.LogInfof("lazy manifest repository migration detected [from=%s, to=%s, assets=%d]", format,
		lazyManifestFormatCurrent, len(manifest.Assets))
	var migrated int
	if !isCanonicalLazyManifestSuperset(manifest, publishedManifest) {
		// 以云端已发布格式为准，合并旧版云端清单与本地增量后规范化完整候选清单，不能信任新设备本地清单的新版标记。
		candidate := mergeLazyManifestAssets(manifest, publishedManifest)
		candidate.Version = lazyManifestFormatLegacy
		migrated, err = repo.migrateLazyManifestRepository(candidate)
		if nil != err {
			logging.LogWarnf("lazy manifest repository migration failed before publication: %s", err)
			return nil, false, err
		}
		ret, err = repo.index("[Sync] Lazy manifest repository migration", false, context)
		if nil != err {
			return nil, false, err
		}
	}
	cloudFiles, err := repo.getFilesWithCloudFallback(append(append([]string{}, cloudLatest.Files...), cloudLatest.LazyFiles...), context)
	if nil != err {
		return nil, false, err
	}
	cloudChunkIDs := repo.getChunks(cloudFiles)
	if err = repo.uploadCloud(context, ret, cloudLatest, cloudChunkIDs, trafficStat); nil != err {
		logging.LogWarnf("lazy manifest repository migration upload failed before publication: %s", err)
		return nil, false, err
	}
	if err = repo.ensurePublishedLazyManifestMetadata(ret, trafficStat, context); nil != err {
		logging.LogWarnf("lazy manifest repository migration metadata closure failed before publication: %s", err)
		return nil, false, err
	}
	if err = repo.updateCloudIndexes(ret, trafficStat, context); nil != err {
		logging.LogWarnf("lazy manifest repository migration index publication failed: %s", err)
		return nil, false, err
	}
	if err = repo.completeUploadTransactionForIndex(ret.ID); nil != err {
		return nil, false, err
	}
	if err = repo.UpdateLatestSync(ret); nil != err {
		return nil, false, err
	}
	if err = repo.markLazyManifestHydrated(ret.LazyManifest); nil != err {
		return nil, false, fmt.Errorf("mark published lazy manifest migration: %w", err)
	}
	logging.LogInfof("lazy manifest repository migration published [assets=%d, latest=%s, cost=%s]", migrated, ret.ID,
		time.Since(start))
	return ret, true, nil
}

func (repo *Repo) ensurePublishedLazyManifestMetadata(index *entity.Index, trafficStat *TrafficStat,
	context map[string]interface{}) error {
	manifest, err := repo.lazyLoader.getManifest()
	if nil != err {
		return err
	}
	if err = validateCurrentLazyManifestCatalog(manifest); nil != err {
		return err
	}
	ids := make([]string, 0, len(manifest.Assets))
	assetsByID := make(map[string]*LazyAsset, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		ids = append(ids, asset.FileID)
		assetsByID[asset.FileID] = asset
	}
	ids = uniqueUploadIDs(ids)
	sort.Strings(ids)
	files := make([]*entity.File, 0, len(ids))
	for _, id := range ids {
		asset := assetsByID[id]
		file, buildErr := canonicalLazyManifestFile(asset)
		if nil != buildErr {
			return fmt.Errorf("build published lazy metadata [%s]: %w", id, buildErr)
		}
		if putErr := repo.store.PutFile(file); nil != putErr {
			return fmt.Errorf("store published lazy metadata [%s]: %w", id, putErr)
		}
		files = append(files, file)
	}
	byID := make(map[string]*entity.File, len(files))
	for _, file := range files {
		byID[file.ID] = file
	}
	for path, asset := range manifest.Assets {
		file := byID[asset.FileID]
		if nil == file || file.Path != path || file.Size != asset.Size || file.Updated != asset.Modified ||
			!slices.Equal(file.Chunks, asset.Chunks) {
			return fmt.Errorf("published lazy metadata [%s] does not match manifest asset [%s]", asset.FileID, path)
		}
	}

	plannedChunks := []string{}
	plannedFiles := append([]string{}, ids...)
	current := filepath.Join(repo.Path, "upload-transactions", "current.json")
	if data, readErr := os.ReadFile(current); nil == readErr {
		tx := &uploadTransaction{}
		if err = json.Unmarshal(data, tx); nil != err {
			return fmt.Errorf("read migration upload transaction: %w", err)
		}
		if tx.IndexID == index.ID {
			plannedChunks = tx.PlannedChunks
			plannedFiles = uniqueUploadIDs(append(tx.PlannedFiles, ids...))
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	tx, err := repo.beginUploadTransaction(index.ID, plannedChunks, plannedFiles)
	if nil != err {
		return err
	}
	if _, saveErr := repo.verifyAndRecordUploadIDs(tx, false, completedUploadIDs(tx.CompletedFiles), trafficStat, context); nil != saveErr {
		return saveErr
	}
	pendingIDs, _ := pendingUploadIDs(ids, tx.CompletedFiles)
	pendingSet := make(map[string]bool, len(pendingIDs))
	for _, id := range pendingIDs {
		pendingSet[id] = true
	}
	pendingFiles := make([]*entity.File, 0, len(pendingIDs))
	for _, file := range files {
		if pendingSet[file.ID] {
			pendingFiles = append(pendingFiles, file)
		}
	}
	result := repo.uploadFilesDetailed(pendingFiles, context)
	trafficStat.UploadFileCount += result.completed
	trafficStat.UploadBytes += result.bytes
	trafficStat.APIPut += result.attempted
	verificationErr, saveErr := repo.verifyAndRecordUploadIDs(tx, false, result.completedIDs, trafficStat, context)
	for id, failure := range result.failedIDs {
		tx.Failed[id] = failure.Error()
	}
	if nil == saveErr {
		saveErr = repo.saveUploadTransaction(tx)
	}
	if nil != saveErr {
		return saveErr
	}
	if nil != result.err {
		return result.err
	}
	if nil != verificationErr {
		return verificationErr
	}
	for _, id := range ids {
		if !tx.CompletedFiles[id] {
			return fmt.Errorf("published lazy metadata [%s] is not remotely verified", id)
		}
	}
	logging.LogInfof("published lazy manifest metadata closure verified [files=%d, uploaded=%d]", len(ids), result.completed)
	return nil
}

func canonicalLazyManifestFile(asset *LazyAsset) (*entity.File, error) {
	if nil == asset {
		return nil, errors.New("asset is nil")
	}
	file := entity.NewFile(asset.Path, asset.Size, asset.Modified)
	file.Chunks = append([]string(nil), asset.Chunks...)
	if file.ID != asset.FileID {
		return nil, fmt.Errorf("derived file ID [%s] does not match manifest file ID [%s]", file.ID, asset.FileID)
	}
	return file, nil
}

func (repo *Repo) hydrateLazyManifestMetadata(manifest *LazyManifest, context map[string]interface{}) (hydrated int, err error) {
	if err = validateCurrentLazyManifestCatalog(manifest); nil != err {
		return 0, fmt.Errorf("hydrate lazy manifest metadata: %w", err)
	}
	ids := make([]string, 0, len(manifest.Assets))
	assets := make(map[string]*LazyAsset, len(manifest.Assets))
	for path, asset := range manifest.Assets {
		if nil == asset {
			return 0, fmt.Errorf("hydrate lazy manifest metadata: asset [%s] is nil", path)
		}
		if path != normalizeLazyPath(asset.Path) || path != asset.Path {
			return 0, fmt.Errorf("hydrate lazy manifest metadata: invalid asset path [%s]", path)
		}
		if "" == asset.FileID {
			return 0, fmt.Errorf("hydrate lazy manifest metadata: asset [%s] has empty file ID", path)
		}
		if existing, exists := assets[asset.FileID]; exists {
			if existing.Path != asset.Path || existing.Size != asset.Size || existing.Modified != asset.Modified ||
				!slices.Equal(existing.Chunks, asset.Chunks) {
				return 0, fmt.Errorf("hydrate lazy manifest metadata: file ID [%s] is shared by conflicting assets [%s] and [%s]",
					asset.FileID, existing.Path, asset.Path)
			}
		} else {
			ids = append(ids, asset.FileID)
		}
		assets[asset.FileID] = asset
	}
	sort.Strings(ids)
	repaired := 0
	for _, id := range ids {
		asset := assets[id]
		canonical, buildErr := canonicalLazyManifestFile(asset)
		if nil != buildErr {
			return 0, fmt.Errorf("hydrate lazy manifest metadata: file [%s]: %w", id, buildErr)
		}
		local, getErr := repo.store.GetFile(id)
		if nil == getErr && nil != local && local.ID == id && local.Path == canonical.Path && local.Size == canonical.Size &&
			local.Updated == canonical.Updated && slices.Equal(local.Chunks, canonical.Chunks) {
			continue
		}
		if nil != getErr && !os.IsNotExist(getErr) {
			return 0, fmt.Errorf("hydrate lazy manifest metadata: read file [%s]: %w", id, getErr)
		}
		if nil == getErr {
			return 0, fmt.Errorf("hydrate lazy manifest metadata: immutable file [%s] conflicts with manifest asset [%s]", id,
				asset.Path)
		}
		if putErr := repo.store.PutFile(canonical); nil != putErr {
			return 0, fmt.Errorf("hydrate lazy manifest metadata: store file [%s]: %w", id, putErr)
		}
		repaired++
	}
	return repaired, nil
}

func (repo *Repo) prepareLocalLazyManifestClosure(context map[string]interface{}) error {
	if !repo.lazyLoadEnabled || nil == repo.lazyLoader {
		return nil
	}
	if _, statErr := os.Stat(repo.lazyLoader.getManifestPath()); nil != statErr {
		if os.IsNotExist(statErr) {
			return nil
		}
		return statErr
	}
	identity, err := repo.lazyManifestIdentity()
	if nil != err {
		return err
	}
	if repo.isLazyManifestLocalClosurePrepared(identity) {
		return nil
	}
	manifest, err := repo.lazyLoader.getManifest()
	if nil != err {
		return err
	}
	if lazyManifestFormatCurrent != lazyManifestFormat(manifest) {
		return nil
	}
	start := time.Now()
	closureManifest := manifest
	mode := "full"
	var delta LazyManifestDelta
	if latest, latestErr := repo.Latest(); nil == latestErr && "" != latest.LazyManifest &&
		repo.isLazyManifestLocalClosurePrepared(latest.LazyManifest) {
		if previous, baselineErr := repo.lazyManifestByIdentity(latest.LazyManifest, "closure-baseline", context); nil == baselineErr {
			delta, err = diffLazyManifests(previous, manifest)
			if nil != err {
				return fmt.Errorf("diff local lazy manifest closure: %w", err)
			}
			changedAssets := make(map[string]*LazyAsset, len(delta.Adds)+len(delta.Updates)+len(delta.Revives))
			for _, assets := range [][]*LazyAsset{delta.Adds, delta.Updates, delta.Revives} {
				for _, asset := range assets {
					changedAssets[normalizeLazyPath(asset.Path)] = asset
				}
			}
			closureManifest = &LazyManifest{Version: lazyManifestFormatCurrent, Assets: changedAssets}
			mode = "delta"
		} else {
			logging.LogWarnf("load prepared lazy manifest baseline [%s] failed: %s, validating full closure",
				latest.LazyManifest, baselineErr)
		}
	}
	identityChanges, err := repo.canonicalizeLazyManifestObjectIdentities(closureManifest)
	if nil != err {
		return err
	}
	if 0 < identityChanges {
		if err = repo.saveLazyManifestWithNewIdentity(manifest); nil != err {
			return err
		}
	}
	if err = validateCurrentLazyManifestCatalog(manifest); nil != err {
		return err
	}
	identity, err = repo.lazyManifestIdentity()
	if nil != err {
		return err
	}
	if repo.isLazyManifestLocalClosurePrepared(identity) {
		return nil
	}
	if "delta" == mode {
		logging.LogInfof("local lazy manifest closure diff [adds=%d, updates=%d, deletes=%d, revives=%d]",
			len(delta.Adds), len(delta.Updates), len(delta.Deletes), len(delta.Revives))
	}
	hydrated, err := repo.hydrateLazyManifestMetadata(closureManifest, context)
	if nil != err {
		return fmt.Errorf("prepare local lazy manifest closure: %w", err)
	}
	repo.setLazyManifestPrepared(identity)
	// 完成标记由 Index 在本地 latest 成功发布后推进，失败的索引不能提交新的差异基线。
	logging.LogInfof("local lazy manifest closure prepared [mode=%s, assets=%d, hydrated=%d, cost=%s]", mode,
		len(closureManifest.Assets), hydrated, time.Since(start))
	return nil
}

// sync0 实现了数据同步的核心逻辑。
//
// fetchedFiles 已从云端下载的文件
// cloudLatest 云端最新索引
// latest 本地最新索引
// mergeResult 待返回的同步合并结果
// trafficStat 待返回的流量统计
func (repo *Repo) sync0(context map[string]interface{},
	fetchedFiles []*entity.File, cloudLatest *entity.Index, latest *entity.Index, mergeResult *MergeResult, trafficStat *TrafficStat) (err error) {
	type mergedWorkspaceBackup struct {
		before, merged []byte
	}
	mergedWorkspaceBackups := map[string]mergedWorkspaceBackup{}
	defer func() {
		if err == nil {
			return
		}
		for path, backup := range mergedWorkspaceBackups {
			rolledBack, rollbackErr := compareAndWriteSyncFile(path, backup.merged, backup.before)
			if rollbackErr != nil {
				logging.LogErrorf("rollback deterministic conflict merge [%s] failed: %s", path, rollbackErr)
			} else if !rolledBack {
				logging.LogInfof("skip deterministic conflict rollback for concurrently edited file [%s]", path)
			}
		}
	}()

	// 获取云端普通文件列表（不包含懒加载文件）
	cloudLatestFiles, err := repo.getFiles(cloudLatest.Files)
	if nil != err {
		logging.LogErrorf("get cloud latest files failed: %s", err)
		return
	}
	cloudLazyFiles, lazyErr := repo.getFilesWithCloudFallback(cloudLatest.LazyFiles, context)
	if nil != lazyErr {
		logging.LogErrorf("get cloud lazy files failed: %s", lazyErr)
		return lazyErr
	}
	var ignoredCloudDeviceLocalFiles bool
	cloudLatest, ignoredCloudDeviceLocalFiles, err = repo.sanitizeIndexFiles(cloudLatest, cloudLatestFiles, cloudLazyFiles,
		"[Sync] Remove device-local cloud files")
	if nil != err {
		logging.LogErrorf("sanitize cloud latest failed: %s", err)
		return err
	}
	cloudLatestFiles, _ = repo.filterProtectedSyncFiles(cloudLatestFiles)
	cloudLazyFiles, _ = repo.filterProtectedSyncFiles(cloudLazyFiles)

	// 处理懒加载文件：只更新清单，不参与常规同步流程
	if len(cloudLatest.LazyFiles) > 0 && "" == cloudLatest.LazyManifest {
		err = repo.updateLazyManifestFromCloudIndex(cloudLatest.LazyFiles, context)
		if nil != err {
			logging.LogErrorf("update lazy manifest from cloud index failed: %s", err)
			return err
		}
	}

	// 如果本地未启用懒加载，需要将云端的懒加载文件当作普通文件处理
	if !repo.lazyLoadEnabled && len(cloudLatest.LazyFiles) > 0 {
		cloudLatestFiles = append(cloudLatestFiles, cloudLazyFiles...)
	}

	// 所有文件都是普通文件（懒加载文件已单独处理）
	cloudChunkIDs := repo.getChunks(cloudLatestFiles)

	transferTraffic, transferErr := runConcurrentSyncTransfers(func(ret *TrafficStat) error { // 从云端下载缺失分块并入库
		fetchChunkIDs, downloadErr := repo.localNotFoundChunks(cloudChunkIDs)
		if nil != downloadErr {
			logging.LogErrorf("get local not found chunks failed: %s", downloadErr)
			return downloadErr
		}

		downloadResult := repo.downloadCloudChunksPutDetailed(fetchChunkIDs, context)
		ret.DownloadBytes = downloadResult.bytes
		ret.DownloadChunkCount = downloadResult.completed
		ret.APIGet = downloadResult.attempted - downloadResult.peerCount
		ret.PeerDownloadBytes = downloadResult.peerBytes
		ret.PeerDownloadChunkCount = downloadResult.peerCount
		ret.PeerFallbackCount = downloadResult.peerFallbackCount
		if nil != downloadResult.err {
			logging.LogErrorf("download cloud chunks put failed: %s", downloadResult.err)
		}
		return downloadResult.err
	}, func(ret *TrafficStat) error { // 上传差异数据
		uploadErr := repo.uploadCloud(context, latest, cloudLatest, cloudChunkIDs, ret)
		if nil != uploadErr {
			logging.LogErrorf("upload cloud failed: %s", uploadErr)
			return uploadErr
		}
		return nil
	})
	mergeTrafficStat(trafficStat, transferTraffic)
	if nil != transferErr {
		err = transferErr
		return
	}

	// 计算本地相比上一个同步点的 upsert 和 remove 差异
	latestFiles, err := repo.getFiles(latest.Files)
	if nil != err {
		logging.LogErrorf("get latest files failed: %s", err)
		return
	}
	logging.LogInfof("got local latest [%s] files [%d]", latest.ID, len(latestFiles))
	latestSync := repo.latestSync()
	latestSyncFiles, err := repo.getFiles(latestSync.Files)
	if nil != err {
		logging.LogErrorf("get latest sync files failed: %s", err)
		return
	}
	localUpserts, localRemoves := repo.diffUpsertRemove(latestFiles, latestSyncFiles, false)
	localUpserts, _ = repo.filterProtectedSyncFiles(localUpserts)
	localRemoves, _ = repo.filterProtectedSyncFiles(localRemoves)

	latestFileMap := map[string]*entity.File{}
	for _, file := range latestFiles {
		latestFileMap[file.Path] = file
	}

	// 计算云端最新相比本地最新的 upsert 和 remove 差异
	var cloudUpserts, cloudRemoves []*entity.File
	if "" != cloudLatest.ID {
		cloudUpserts, cloudRemoves = repo.diffUpsertRemove(cloudLatestFiles, latestFiles, true)
	}

	// 增加一些诊断日志 https://ld246.com/article/1698370932077
	for _, c := range cloudUpserts {
		logging.LogInfof("cloud upsert [%s, %s, %s]", c.ID, c.Path, time.UnixMilli(c.Updated).Format("2006-01-02 15:04:05"))
	}
	for _, r := range cloudRemoves {
		logging.LogInfof("cloud remove [%s, %s, %s]", r.ID, r.Path, time.UnixMilli(r.Updated).Format("2006-01-02 15:04:05"))
	}
	for _, c := range localUpserts {
		logging.LogInfof("local upsert [%s, %s, %s]", c.ID, c.Path, time.UnixMilli(c.Updated).Format("2006-01-02 15:04:05"))
	}
	for _, r := range localRemoves {
		logging.LogInfof("local remove [%s, %s, %s]", r.ID, r.Path, time.UnixMilli(r.Updated).Format("2006-01-02 15:04:05"))
	}

	// 避免旧的本地数据覆盖云端数据 https://github.com/siyuan-note/siyuan/issues/7403
	localUpserts = repo.filterLocalUpserts(localUpserts, cloudUpserts)
	localChanged := 0 < len(localUpserts) || 0 < len(localRemoves) || ignoredCloudDeviceLocalFiles
	localUpsertsByID := map[string]*entity.File{}
	localUpsertsByPath := map[string]*entity.File{}
	for _, localUpsert := range localUpserts {
		localUpsertsByID[localUpsert.ID] = localUpsert
		localUpsertsByPath[localUpsert.Path] = localUpsert
	}
	localRemovesByID := map[string]*entity.File{}
	localRemovesByPath := map[string]*entity.File{}
	for _, localRemove := range localRemoves {
		localRemovesByID[localRemove.ID] = localRemove
		localRemovesByPath[localRemove.Path] = localRemove
	}
	latestSyncFilesByID := map[string]*entity.File{}
	latestSyncFilesByPath := map[string]*entity.File{}
	for _, latestSyncFile := range latestSyncFiles {
		latestSyncFilesByID[latestSyncFile.ID] = latestSyncFile
		latestSyncFilesByPath[latestSyncFile.Path] = latestSyncFile
	}

	// 记录本地 syncignore 变更
	var localUpsertIgnore *entity.File
	for _, upsert := range localUpserts {
		if "/.siyuan/syncignore" == upsert.Path {
			localUpsertIgnore = upsert
			break
		}
	}

	fetchedFileIDs := map[string]bool{}
	for _, fetchedFile := range fetchedFiles {
		fetchedFileIDs[fetchedFile.ID] = true
	}

	nowStr := mergeResult.Time.Format("2006-01-02-150405")

	// 计算冲突的 upsert 和无冲突能够合并的 upsert
	// 冲突的文件尽量以本地 upsert 和 remove 为准
	var tmpMergeConflicts []*entity.File
	var cloudUpsertIgnore *entity.File
	for _, cloudUpsert := range cloudUpserts {
		if "/.siyuan/syncignore" == cloudUpsert.Path {
			cloudUpsertIgnore = cloudUpsert
		}

		localUpsert := localUpsertsByPath[cloudUpsert.Path]
		if nil == localUpsert {
			localUpsert = localUpsertsByID[cloudUpsert.ID]
		}
		if nil != localUpsert { // 相同的文件本地发生了变更
			if "/.siyuan/lazy_manifest.json" == cloudUpsert.Path {
				if mergeErr := repo.mergeLazyManifestFile(localUpsert, cloudUpsert, context); nil == mergeErr {
					mergeResult.MergedLazyManifest = true
					logging.LogInfof("sync merge lazy manifest [%s, %s, %s]", cloudUpsert.ID, cloudUpsert.Path, time.UnixMilli(cloudUpsert.Updated).Format("2006-01-02 15:04:05"))
					continue
				} else {
					logging.LogWarnf("merge lazy manifest failed: %s", mergeErr)
				}
			}

			latestSyncFile := latestSyncFilesByPath[localUpsert.Path]
			if nil == latestSyncFile {
				latestSyncFile = latestSyncFilesByID[localUpsert.ID]
			}
			if nil != latestSyncFile {
				baseData, baseErr := repo.OpenFile(latestSyncFile)
				localData, localErr := repo.OpenFile(localUpsert)
				remoteData, remoteErr := repo.OpenFile(cloudUpsert)
				if nil == baseErr && nil == localErr && nil == remoteErr {
					if merged, ok := repo.TryConflictMerge(cloudUpsert.Path, baseData, localData, remoteData); ok {
						absPath := filepath.Join(repo.DataPath, strings.TrimPrefix(cloudUpsert.Path, "/"))
						if written, writeErr := compareAndWriteSyncFile(absPath, localData, merged); nil == writeErr && written {
							// 云端原始版本仍进入同步历史，工作区仅写入已验证的合并结果。
							tmpMergeConflicts = append(tmpMergeConflicts, cloudUpsert)
							mergedWorkspaceBackups[absPath] = mergedWorkspaceBackup{before: append([]byte(nil), localData...), merged: append([]byte(nil), merged...)}
							mergeResult.MergedPaths = append(mergeResult.MergedPaths, cloudUpsert.Path)
							logging.LogInfof("sync deterministically merged conflict [%s]", cloudUpsert.Path)
							continue
						} else if writeErr != nil {
							logging.LogWarnf("write deterministic conflict merge [%s] failed: %s", cloudUpsert.Path, writeErr)
						} else {
							logging.LogInfof("deterministic conflict merge abandoned after concurrent edit [%s]", cloudUpsert.Path)
						}
					}
				}
			}

			// 无论是否发生实际下载文件，都需要生成本地历史，以确保任何情况下都能够通过数据历史恢复文件
			tmpMergeConflicts = append(tmpMergeConflicts, cloudUpsert)

			if fetchedFileIDs[cloudUpsert.ID] {
				// 发生实际下载文件的情况，尝试解决冲突

				if repo.ignoreLocalUpsert(localUpsert, latestSyncFile, nowStr, context) {
					// 如果能忽略本地变更的话则不算做冲突，进行正常合并
					mergeResult.Upserts = append(mergeResult.Upserts, cloudUpsert)
					logging.LogInfof("sync merge upsert [%s, %s, %s]", cloudUpsert.ID, cloudUpsert.Path, time.UnixMilli(cloudUpsert.Updated).Format("2006-01-02 15:04:05"))
					continue
				}

				// 云端有更新的 upsert 从而导致了冲突，在外部单独处理生成副本
				mergeResult.Conflicts = append(mergeResult.Conflicts, cloudUpsert)
				logging.LogInfof("sync merge conflict [%s, %s, %s]", cloudUpsert.ID, cloudUpsert.Path, time.UnixMilli(cloudUpsert.Updated).Format("2006-01-02 15:04:05"))
			}
			continue
		}

		localRemove := localRemovesByPath[cloudUpsert.Path]
		if nil == localRemove {
			localRemove = localRemovesByID[cloudUpsert.ID]
		}
		if nil == localRemove {
			if strings.HasSuffix(cloudUpsert.Path, ".tmp") {
				// 数据仓库不迁出 `.tmp` 临时文件 https://github.com/siyuan-note/siyuan/issues/7087
				logging.LogWarnf("ignored tmp file [%s]", cloudUpsert.Path)
				continue
			}

			// 如果云端 upsert 早于本地已经存在的文件 7 分钟，则以本地文件为准
			cloudUpsertTooOld := false
			if localFile := latestFileMap[cloudUpsert.Path]; nil != localFile && localFile.Updated > cloudUpsert.Updated+7*60*1000 {
				logging.LogWarnf("ignored cloud upsert [%s, %s, %s] because local file is newer", cloudUpsert.ID, cloudUpsert.Path, time.UnixMilli(cloudUpsert.Updated).Format("2006-01-02 15:04:05"))
				cloudUpsertTooOld = true
			}
			if !cloudUpsertTooOld {
				mergeResult.Upserts = append(mergeResult.Upserts, cloudUpsert)
				logging.LogInfof("sync merge upsert [%s, %s, %s]", cloudUpsert.ID, cloudUpsert.Path, time.UnixMilli(cloudUpsert.Updated).Format("2006-01-02 15:04:05"))
			}
		}
	}

	// 计算能够无冲突合并的 remove，冲突的文件以本地 upsert 为准
	for _, cloudRemove := range cloudRemoves {
		localUpsert := localUpsertsByPath[cloudRemove.Path]
		if nil == localUpsert {
			localUpsert = localUpsertsByID[cloudRemove.ID]
		}
		if nil == localUpsert {
			mergeResult.Removes = append(mergeResult.Removes, cloudRemove)
		}
	}

	// 云端如果更新了忽略文件则使用其规则过滤 remove，避免后面误删本地文件 https://github.com/siyuan-note/siyuan/issues/5497
	var ignoreLines []string
	if nil != cloudUpsertIgnore {
		coDir := filepath.Join(repo.DataPath)
		if nil != localUpsertIgnore {
			// 本地 syncignore 存在变更，则临时迁出
			coDir = filepath.Join(repo.TempPath, "repo", "sync", "ignore")
		}
		if err = repo.checkoutFile(cloudUpsertIgnore, coDir, 1, 1, context); nil != err {
			logging.LogErrorf("checkout ignore file failed: %s", err)
			return
		}
		data, readErr := filelock.ReadFile(filepath.Join(coDir, cloudUpsertIgnore.Path))
		if nil != readErr {
			logging.LogErrorf("read ignore file failed: %s", readErr)
			err = readErr
			return
		}
		dataStr := string(data)
		dataStr = strings.ReplaceAll(dataStr, "\r\n", "\n")
		ignoreLines = strings.Split(dataStr, "\n")
		//logging.LogInfof("sync merge ignore rules: \n  %s", strings.Join(ignoreLines, "\n  "))
	}

	ignoreMatcher := ignore.CompileIgnoreLines(ignoreLines...)
	var mergeResultRemovesTmp []*entity.File
	for _, remove := range mergeResult.Removes {
		if !ignoreMatcher.MatchesPath(remove.Path) {
			mergeResultRemovesTmp = append(mergeResultRemovesTmp, remove)
			continue
		}
		// logging.LogInfof("sync merge ignore remove [%s]", remove.Path)
	}
	mergeResult.Removes = mergeResultRemovesTmp

	// 冲突文件复制到数据历史文件夹
	repo.filterProtectedMergeResult(mergeResult)
	tmpMergeConflicts, _ = repo.filterProtectedSyncFiles(tmpMergeConflicts)
	if 0 < len(tmpMergeConflicts) {
		temp := filepath.Join(repo.TempPath, "repo", "sync", "conflicts", nowStr)
		for i, file := range tmpMergeConflicts {
			var checkoutTmp *entity.File
			checkoutTmp, err = repo.store.GetFile(file.ID)
			if nil != err {
				logging.LogErrorf("get file failed: %s", err)
				return
			}

			err = repo.checkoutFile(checkoutTmp, temp, i+1, len(tmpMergeConflicts), context)
			if nil != err {
				logging.LogErrorf("checkout file failed: %s", err)
				return
			}

			absPath := filepath.Join(temp, checkoutTmp.Path)
			err = repo.genSyncHistory(nowStr, file.Path, absPath)
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
	err = repo.mergeSync(mergeResult, localChanged, true, latest, cloudLatest, cloudChunkIDs, trafficStat, context)
	if nil != err {
		logging.LogErrorf("merge sync failed: %s", err)
		return
	}

	// 统计流量
	go repo.cloud.AddTraffic(&cloud.Traffic{
		UploadBytes:   trafficStat.UploadBytes,
		DownloadBytes: trafficStat.DownloadBytes,
		APIGet:        trafficStat.APIGet,
		APIPut:        trafficStat.APIPut,
	})

	// 移除空目录
	gulu.File.RemoveEmptyDirs(repo.DataPath, removeEmptyDirExcludes...)
	return
}

func compareAndWriteSyncFile(path string, expected, replacement []byte) (written bool, err error) {
	filelock.Lock(path)
	defer filelock.Unlock(path)
	actual, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(actual, expected) {
		return false, nil
	}
	if err = gulu.File.WriteFileSafer(path, replacement, 0644); err != nil {
		return false, err
	}
	return true, nil
}

func (repo *Repo) ignoreLocalUpsert(localUpsert, latestSyncFile *entity.File, now string, context map[string]interface{}) bool {
	if !strings.HasSuffix(localUpsert.Path, ".sy") {
		return false // 非 .sy 文件目前不做内容对比，直接认为本地 upsert 是最新的
	}

	if nil == latestSyncFile {
		return false // 本地 upsert 是新增的文件
	}

	// 如果是变更 .sy 文件则需要解析并进行内容对比

	luteEngine := lute.New()
	temp := filepath.Join(repo.TempPath, "repo", "sync", "resolves", now)
	localTree, err := repo.checkoutTree(localUpsert, temp, luteEngine, context)
	if nil != err {
		return false
	}
	localLastSyncTree, err := repo.checkoutTree(latestSyncFile, temp, luteEngine, context)
	if nil != err {
		return false
	}

	localNodes, localLastSyncNodes := map[string]*ast.Node{}, map[string]*ast.Node{}
	ast.Walk(localTree.Root, func(node *ast.Node, entering bool) ast.WalkStatus {
		if !entering || !node.IsBlock() || ast.NodeDocument == node.Type {
			return ast.WalkContinue
		}

		localNodes[node.ID] = node
		return ast.WalkContinue
	})
	ast.Walk(localLastSyncTree.Root, func(node *ast.Node, entering bool) ast.WalkStatus {
		if !entering || !node.IsBlock() || ast.NodeDocument == node.Type {
			return ast.WalkContinue
		}

		localLastSyncNodes[node.ID] = node
		return ast.WalkContinue
	})

	if len(localNodes) != len(localLastSyncNodes) {
		return false // 本地变更导致块数量不相同
	}

	for id, localNode := range localNodes {
		if lastSyncNode, ok := localLastSyncNodes[id]; !ok || localNode.ID != lastSyncNode.ID || localNode.Type != lastSyncNode.Type {
			return false // 本地变更导致块不相同
		}

		localLastSyncNode := localLastSyncNodes[id]
		if !onlyChangeFoldIAL(localNode, localLastSyncNode) {
			return false // 本地变更导致块不相同
		}
	}
	return true // 本地仅变更了折叠属性，并且云端也有更新的 upsert，所以忽略本地的折叠变更
}

func onlyChangeFoldIAL(n1, n2 *ast.Node) bool {
	if n1.Content() != n2.Content() {
		return false
	}

	n1Attrs := parse.IAL2Map(n1.KramdownIAL)
	n2Attrs := parse.IAL2Map(n2.KramdownIAL)

	// 移除折叠属性
	delete(n1Attrs, "fold")
	delete(n1Attrs, "heading-fold")
	delete(n2Attrs, "fold")
	delete(n2Attrs, "heading-fold")

	// 移除更新时间
	delete(n1Attrs, "updated")
	delete(n2Attrs, "updated")

	if len(n1Attrs) != len(n2Attrs) {
		return false
	}

	for k, v1 := range n1Attrs {
		if v2, ok := n2Attrs[k]; !ok || v1 != v2 {
			return false
		}
	}
	return true
}

func (repo *Repo) checkoutTree(file *entity.File, checkoutDir string, luteEngine *lute.Lute, context map[string]interface{}) (ret *parse.Tree, err error) {
	checkoutTmp, err := repo.store.GetFile(file.ID)
	if nil != err {
		logging.LogErrorf("get file failed: %s", err)
		return
	}
	if err = repo.checkoutFile(checkoutTmp, checkoutDir, 1, 1, context); nil != err {
		logging.LogErrorf("checkout file failed: %s", err)
		return
	}
	absPath := filepath.Join(checkoutDir, checkoutTmp.Path)
	data, err := os.ReadFile(absPath)
	if nil != err {
		logging.LogErrorf("read file failed: %s", err)
		return
	}
	ret, err = dataparser.ParseJSONWithoutFix(data, luteEngine.ParseOptions)
	if nil != err {
		logging.LogErrorf("parse tree failed: %s", err)
		return
	}
	return
}

func (repo *Repo) restoreFiles(mergeResult *MergeResult, context map[string]interface{}) (err error) {
	repo.filterProtectedMergeResult(mergeResult)
	err = repo.checkoutFiles(mergeResult.Upserts, context)
	if nil != err {
		logging.LogErrorf("checkout files failed: %s", err)
		return
	}
	err = repo.removeFiles(mergeResult.Removes, context)
	if nil != err {
		logging.LogErrorf("remove files failed: %s", err)
		return
	}
	return
}

func (repo *Repo) mergeSync(mergeResult *MergeResult, localChanged, needSyncCloud bool, latest, cloudLatest *entity.Index, cloudChunkIDs []string, trafficStat *TrafficStat, context map[string]interface{}) (err error) {
	if mergeResult.DataChanged() {
		if localChanged { // 如果云端和本地都改变了，则需要创建合并索引并再次同步
			logging.LogInfof("creating merge index [%s]", latest.ID)
			mergeStart := time.Now()
			mergedLatest, mergeIndexErr := repo.index("[Sync] Cloud sync merge", false, context)
			if nil != mergeIndexErr {
				logging.LogErrorf("merge index failed: %s", mergeIndexErr)
				err = mergeIndexErr
				return
			}

			diff, mergeIndexErr := repo.diffIndex(mergedLatest, latest)
			if nil != mergeIndexErr {
				logging.LogErrorf("diff index failed: %s", mergeIndexErr)
				err = mergeIndexErr
				return
			}
			for _, add := range diff.AddsLeft {
				logging.LogInfof("merge index add [%s, %s, %s]", add.ID, add.Path, time.UnixMilli(add.Updated).Format("2006-01-02 15:04:05"))
			}
			for _, update := range diff.UpdatesLeft {
				logging.LogInfof("merge index update [%s, %s, %s]", update.ID, update.Path, time.UnixMilli(update.Updated).Format("2006-01-02 15:04:05"))
			}
			for _, file := range append(diff.AddsLeft, diff.UpdatesLeft...) {
				if slices.Contains(mergeResult.MergedPaths, file.Path) {
					mergeResult.Upserts = append(mergeResult.Upserts, file)
				}
			}

			latest = mergedLatest
			mergeElapsed := time.Since(mergeStart)
			mergeMemo := fmt.Sprintf("[Sync] Cloud sync merge, completed in %.2fs", mergeElapsed.Seconds())
			latest.Memo = mergeMemo
			err = repo.store.PutIndex(latest)
			if nil != err {
				logging.LogErrorf("put merge index failed: %s", err)
				return
			}
			logging.LogInfof("created merge index [%s]", latest.ID)

			if needSyncCloud {
				err = repo.uploadCloud(context, latest, cloudLatest, cloudChunkIDs, trafficStat)
				if nil != err {
					logging.LogErrorf("upload cloud failed: %s", err)
					return
				}
			}
		} else { // 只有云端改变了，本地没有改变，则直接使用云端索引作为本地最新索引
			latest = cloudLatest
		}
	}

	// 检查LazyFiles是否需要更新云端索引
	lazyFilesNeedSync := false
	if len(latest.LazyFiles) != len(cloudLatest.LazyFiles) {
		lazyFilesNeedSync = true
		logging.LogInfof("sync: lazy files count differs (local=%d, cloud=%d), need sync", len(latest.LazyFiles), len(cloudLatest.LazyFiles))
	} else if len(latest.LazyFiles) > 0 {
		// 检查LazyFiles内容是否相同
		cloudLazySet := make(map[string]bool)
		for _, id := range cloudLatest.LazyFiles {
			cloudLazySet[id] = true
		}
		for _, id := range latest.LazyFiles {
			if !cloudLazySet[id] {
				lazyFilesNeedSync = true
				logging.LogInfof("sync: lazy files content differs, need sync")
				break
			}
		}
	}

	if (localChanged && needSyncCloud) || "" == cloudLatest.ID || lazyFilesNeedSync {
		err = repo.updateCloudIndexes(latest, trafficStat, context)
		if nil != err {
			logging.LogErrorf("update cloud indexes failed: %s", err)
			return
		}
		if err = repo.completeUploadTransactionForIndex(latest.ID); nil != err {
			return
		}
		if repo.lazyLoadEnabled && nil != repo.lazyLoader && "" != latest.LazyManifest {
			if err = repo.markLazyManifestHydrated(latest.LazyManifest); nil != err {
				return fmt.Errorf("mark published lazy manifest: %w", err)
			}
		}
		if lazyFilesNeedSync {
			logging.LogInfof("sync: successfully updated cloud indexes with lazy files (count=%d)", len(latest.LazyFiles))
		}
	}
	if repo.lazyLoadEnabled && nil != repo.lazyLoader && "" != latest.LazyManifest {
		if err = repo.lazyLoader.reloadManifest(); nil != err {
			return fmt.Errorf("reload synchronized lazy manifest: %w", err)
		}
	}

	// 更新本地最新索引
	if err = repo.UpdateLatest(latest); nil != err {
		logging.LogErrorf("update latest failed: %s", err)
		return
	}
	if err = repo.store.PutIndex(latest); nil != err {
		logging.LogErrorf("put index failed: %s", err)
		return
	}

	// 更新本地同步点
	err = repo.UpdateLatestSync(latest)
	if nil != err {
		logging.LogErrorf("update latest sync failed: %s", err)
		return
	}
	return
}

func (repo *Repo) updateCloudIndexes(latest *entity.Index, trafficStat *TrafficStat, context map[string]interface{}) (err error) {
	start := time.Now()
	logging.LogInfof("updateCloudIndexes: start latestID=%s files=%d lazyFiles=%d checkIndexID=%s",
		latest.ID, len(latest.Files), len(latest.LazyFiles), latest.CheckIndexID)

	// 校验索引只展开普通文件和懒加载清单根。懒加载元数据的完整闭包在迁移发布事务中单独校验。
	files, getErr := repo.getFiles(latest.Files)
	if nil != getErr {
		logging.LogErrorf("get files failed: %s", getErr)
		err = getErr
		return
	}
	if "" != latest.LazyManifest && !containsUploadID(latest.Files, latest.LazyManifest) {
		manifestFiles, manifestErr := repo.getFiles([]string{latest.LazyManifest})
		if nil != manifestErr {
			logging.LogErrorf("get lazy manifest file failed: %s", manifestErr)
			return manifestErr
		}
		files = append(files, manifestFiles...)
	}

	checkIndex := buildCheckIndex(latest, files)
	if "" != latest.CheckIndexID {
		// An index ID is immutable. Reuse its existing integrity object ID on
		// retries instead of changing CheckIndexID while uploading the index with
		// overwrite=false.
		checkIndex.ID = latest.CheckIndexID
	}

	// 更新本地 latest 的关联的 checkIndexID，后续会将本地 latest 上传到云端
	latest.CheckIndexID = checkIndex.ID
	if err = repo.store.PutIndex(latest); nil != err {
		logging.LogErrorf("put index failed: %s", err)
		return
	}

	// 以下步骤是更新云端相关索引数据

	var errs []error
	errLock := sync.Mutex{}
	waitGroup := &sync.WaitGroup{}

	// Upload the index before publishing it. refs/latest is deliberately updated
	// only after every prerequisite below succeeds, otherwise another device can
	// observe an index whose integrity metadata was never published.
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		length, uploadErr := repo.uploadIndex(latest, context)
		if nil != uploadErr {
			logging.LogErrorf("upload latest index failed: %s", uploadErr)
			errLock.Lock()
			errs = append(errs, uploadErr)
			errLock.Unlock()
			return
		}
		trafficStat.m.Lock()
		trafficStat.UploadFileCount++
		trafficStat.UploadBytes += length
		trafficStat.APIPut++
		trafficStat.m.Unlock()

		downloadLength, uploadedIndex, verifyErr := repo.downloadCloudIndex(latest.ID, context)
		if nil != verifyErr || !sameIndex(latest, uploadedIndex) {
			if nil == verifyErr {
				verifyErr = fmt.Errorf("uploaded index identity mismatch")
			}
			errLock.Lock()
			errs = append(errs, fmt.Errorf("verify uploaded index [%s] failed: %w", latest.ID, verifyErr))
			errLock.Unlock()
			return
		}
		trafficStat.m.Lock()
		trafficStat.DownloadFileCount++
		trafficStat.DownloadBytes += downloadLength
		trafficStat.APIGet++
		trafficStat.m.Unlock()
	}()

	isS3OrSiYuan := repo.isCloudS3() || repo.isCloudSiYuan()

	// 更新云端索引列表
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()

		downloadBytes, uploadBytes, uploadErr := repo.updateCloudIndexesV2(latest, context)
		if nil != uploadErr {
			logging.LogErrorf("update cloud indexes failed: %s", uploadErr)
			errLock.Lock()
			errs = append(errs, uploadErr)
			errLock.Unlock()
			return
		}

		trafficStat.m.Lock()
		trafficStat.DownloadFileCount++
		trafficStat.DownloadBytes += downloadBytes
		trafficStat.UploadFileCount++
		trafficStat.UploadBytes += uploadBytes
		trafficStat.APIGet++
		trafficStat.APIPut++
		trafficStat.m.Unlock()
	}()

	// 上传校验索引
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()

		uploadErr := repo.updateCloudCheckIndex(checkIndex, context)
		if nil != uploadErr {
			logging.LogErrorf("update cloud check index failed: %s", uploadErr)
			errLock.Lock()
			errs = append(errs, uploadErr)
			errLock.Unlock()
			return
		}
	}()

	// 尝试上传修复云端缺失的数据对象
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()

		repo.uploadCloudMissingObjects(trafficStat, context)
	}()

	waitGroup.Wait()

	if 0 < len(errs) {
		err = errs[0]
		logging.LogWarnf("updateCloudIndexes: failed latestID=%s errors=%d cost=%s firstErr=%s",
			latest.ID, len(errs), time.Since(start), err)
		return
	}

	var seqNumLatests []string
	var uploadErr error
	if isS3OrSiYuan {
		_, maxSeqNum, existingSeqNumLatests := repo.getSeqNumLatest()
		seqNumLatests = existingSeqNumLatests
		seqNum := maxSeqNum + 1
		if _, uploadErr = repo.cloud.UploadBytes("refs/latest-"+strconv.Itoa(seqNum)+"-"+latest.ID, []byte(latest.ID), true); nil != uploadErr {
			return fmt.Errorf("update cloud [refs/latest-%d] failed: %w", seqNum, uploadErr)
		}
	}

	// refs/latest is the publication boundary. Keep it last so failures above
	// leave the previously published snapshot available. The cache-busting
	// marker is safe to expose first: readers retry refs/latest until it agrees.
	length, uploadErr := repo.updateCloudRef("refs/latest", context)
	if nil != uploadErr {
		return uploadErr
	}
	trafficStat.m.Lock()
	trafficStat.UploadFileCount++
	trafficStat.UploadBytes += length
	trafficStat.APIPut++
	trafficStat.m.Unlock()

	if isS3OrSiYuan {
		go func() {
			for _, seqNumLatest := range seqNumLatests {
				if deleteErr := repo.cloud.RemoveObject(seqNumLatest); nil != deleteErr {
					logging.LogWarnf("delete cloud [%s] failed: %s", seqNumLatest, deleteErr)
				}
			}
		}()
	}
	logging.LogInfof("updateCloudIndexes: completed latestID=%s files=%d lazyFiles=%d checkIndexID=%s cost=%s",
		latest.ID, len(latest.Files), len(latest.LazyFiles), latest.CheckIndexID, time.Since(start))
	return
}

func sameIndex(expected, actual *entity.Index) bool {
	if nil == expected || nil == actual || expected.ID != actual.ID || expected.Memo != actual.Memo ||
		expected.Created != actual.Created || expected.Count != actual.Count || expected.Size != actual.Size ||
		expected.SystemID != actual.SystemID || expected.SystemName != actual.SystemName || expected.SystemOS != actual.SystemOS ||
		expected.CheckIndexID != actual.CheckIndexID || expected.AesKeyVerifyVal != actual.AesKeyVerifyVal ||
		expected.LazyManifest != actual.LazyManifest || len(expected.Files) != len(actual.Files) ||
		len(expected.LazyFiles) != len(actual.LazyFiles) {
		return false
	}
	for i, id := range expected.Files {
		if id != actual.Files[i] {
			return false
		}
	}
	for i, id := range expected.LazyFiles {
		if id != actual.LazyFiles[i] {
			return false
		}
	}
	return true
}

func buildCheckIndex(index *entity.Index, files []*entity.File) *entity.CheckIndex {
	ret := &entity.CheckIndex{ID: util.RandHash(), IndexID: index.ID}
	for _, file := range files {
		ret.Files = append(ret.Files, &entity.CheckIndexFile{ID: file.ID, Chunks: file.Chunks})
	}
	return ret
}

type syncTransferTask func(traffic *TrafficStat) error

type syncTransferResult struct {
	traffic *TrafficStat
	err     error
}

func runConcurrentSyncTransfers(download, upload syncTransferTask) (traffic *TrafficStat, err error) {
	tasks := [2]syncTransferTask{download, upload}
	results := [2]syncTransferResult{}
	waitGroup := sync.WaitGroup{}
	for i, task := range tasks {
		results[i].traffic = &TrafficStat{m: &sync.Mutex{}}
		waitGroup.Add(1)
		go func(resultIndex int, run syncTransferTask) {
			defer waitGroup.Done()
			defer func() {
				if recovered := recover(); nil != recovered {
					results[resultIndex].err = concurrentTransferPanicError("sync transfer", recovered)
				}
			}()
			results[resultIndex].err = run(results[resultIndex].traffic)
		}(i, task)
	}
	waitGroup.Wait()

	traffic = &TrafficStat{m: &sync.Mutex{}}
	mergeTrafficStat(traffic, results[0].traffic)
	mergeTrafficStat(traffic, results[1].traffic)
	if nil != results[0].err {
		return traffic, results[0].err
	}
	if nil != results[1].err {
		return traffic, results[1].err
	}
	return traffic, nil
}

type concurrentObjectTransferResult struct {
	bytes             int64
	completed         int
	attempted         int
	peerBytes         int64
	peerCount         int
	peerFallbackCount int
	err               error
	completedIDs      []string
	failedIDs         map[string]error
	skippedIDs        []string
}

type concurrentTransferState struct {
	bytes        atomic.Int64
	completed    atomic.Int64
	attempted    atomic.Int64
	errMu        sync.Mutex
	err          error
	resultMu     sync.Mutex
	completedIDs []string
	failedIDs    map[string]error
}

func (state *concurrentTransferState) getErr() error {
	state.errMu.Lock()
	defer state.errMu.Unlock()
	return state.err
}

func (state *concurrentTransferState) setErr(err error) {
	state.errMu.Lock()
	defer state.errMu.Unlock()
	if nil == state.err {
		state.err = err
	}
}

func (state *concurrentTransferState) result() concurrentObjectTransferResult {
	state.resultMu.Lock()
	completedIDs := append([]string(nil), state.completedIDs...)
	failedIDs := make(map[string]error, len(state.failedIDs))
	for id, err := range state.failedIDs {
		failedIDs[id] = err
	}
	state.resultMu.Unlock()
	sort.Strings(completedIDs)
	return concurrentObjectTransferResult{
		bytes:        state.bytes.Load(),
		completed:    int(state.completed.Load()),
		attempted:    int(state.attempted.Load()),
		err:          state.getErr(),
		completedIDs: completedIDs,
		failedIDs:    failedIDs,
	}
}

func (state *concurrentTransferState) recordCompleted(id string) {
	state.resultMu.Lock()
	state.completedIDs = append(state.completedIDs, id)
	state.resultMu.Unlock()
}

func (state *concurrentTransferState) recordFailed(id string, err error) {
	state.resultMu.Lock()
	if nil == state.failedIDs {
		state.failedIDs = map[string]error{}
	}
	state.failedIDs[id] = err
	state.resultMu.Unlock()
}

func concurrentTransferPanicError(objectType string, recovered interface{}) error {
	if recoveredErr, ok := recovered.(error); ok {
		return fmt.Errorf("%s worker panic: %w", objectType, recoveredErr)
	}
	return fmt.Errorf("%s worker panic: %v", objectType, recovered)
}

func runConcurrentObjectTransfers(ids []string, poolSize int, objectType string,
	transfer func(id string, attempt int) (int64, error)) (ret concurrentObjectTransferResult) {
	if 1 > len(ids) {
		return
	}
	if poolSize > len(ids) {
		poolSize = len(ids)
	}

	waitGroup := &sync.WaitGroup{}
	state := &concurrentTransferState{}
	pool, err := ants.NewPoolWithFunc(poolSize, func(arg interface{}) {
		id := arg.(string)
		defer waitGroup.Done()
		defer func() {
			if recovered := recover(); nil != recovered {
				panicErr := concurrentTransferPanicError(objectType, recovered)
				state.recordFailed(id, panicErr)
				state.setErr(panicErr)
			}
		}()
		if nil != state.getErr() {
			return
		}

		attempt := int(state.attempted.Add(1))
		length, transferErr := transfer(id, attempt)
		if nil != transferErr {
			state.recordFailed(id, transferErr)
			state.setErr(transferErr)
			return
		}
		state.bytes.Add(length)
		state.completed.Add(1)
		state.recordCompleted(id)
	})
	if nil != err {
		ret.err = err
		return
	}
	defer pool.Release()

	for _, id := range ids {
		if nil != state.getErr() {
			break
		}
		waitGroup.Add(1)
		if invokeErr := pool.Invoke(id); nil != invokeErr {
			waitGroup.Done()
			logging.LogErrorf("invoke failed: %s", invokeErr)
			state.setErr(invokeErr)
			break
		}
	}
	waitGroup.Wait()
	ret = state.result()
	completed := make(map[string]bool, len(ret.completedIDs))
	for _, id := range ret.completedIDs {
		completed[id] = true
	}
	for _, id := range ids {
		if !completed[id] {
			if _, failed := ret.failedIDs[id]; !failed {
				ret.skippedIDs = append(ret.skippedIDs, id)
			}
		}
	}
	sort.Strings(ret.skippedIDs)
	return ret
}

func mergeTrafficStat(target, delta *TrafficStat) {
	if nil == target || nil == delta {
		return
	}
	target.DownloadFileCount += delta.DownloadFileCount
	target.DownloadChunkCount += delta.DownloadChunkCount
	target.DownloadBytes += delta.DownloadBytes
	target.PeerDownloadChunkCount += delta.PeerDownloadChunkCount
	target.PeerDownloadBytes += delta.PeerDownloadBytes
	target.PeerFallbackCount += delta.PeerFallbackCount
	target.UploadFileCount += delta.UploadFileCount
	target.UploadChunkCount += delta.UploadChunkCount
	target.UploadBytes += delta.UploadBytes
	target.APIGet += delta.APIGet
	target.APIPut += delta.APIPut
}

// filterLocalUpserts 避免旧的本地数据覆盖云端数据 https://github.com/siyuan-note/siyuan/issues/7403
func (repo *Repo) filterLocalUpserts(localUpserts, cloudUpserts []*entity.File) (ret []*entity.File) {
	cloudUpsertsMap := map[string]*entity.File{}
	for _, cloudUpsert := range cloudUpserts {
		cloudUpsertsMap[cloudUpsert.Path] = cloudUpsert
	}

	toRemoveLocalUpsertPaths := map[string]bool{}
	for _, localUpsert := range localUpserts {
		if cloudUpsert := cloudUpsertsMap[localUpsert.Path]; nil != cloudUpsert {
			if localUpsert.Updated < cloudUpsert.Updated-1000*60*7 { // 本地早于云端 7 分钟
				toRemoveLocalUpsertPaths[localUpsert.Path] = true // 使用云端数据覆盖本地数据
				logging.LogWarnf("ignored local upsert [%s, %s, %s] because it is older than cloud upsert [%s, %s, %s]",
					localUpsert.ID, localUpsert.Path, time.UnixMilli(localUpsert.Updated).Format("2006-01-02 15:04:05"),
					cloudUpsert.ID, cloudUpsert.Path, time.UnixMilli(cloudUpsert.Updated).Format("2006-01-02 15:04:05"))
			}
		}
	}

	for _, localUpsert := range localUpserts {
		if !toRemoveLocalUpsertPaths[localUpsert.Path] {
			ret = append(ret, localUpsert)
		}
	}

	if len(localUpserts) != len(ret) {
		buf := bytes.Buffer{}
		buf.WriteString("filtered local upserts from:\n")
		for _, localUpsert := range localUpserts {
			buf.WriteString(fmt.Sprintf("  [%s, %s, %s]\n", localUpsert.ID, localUpsert.Path, time.UnixMilli(localUpsert.Updated).Format("2006-01-02 15:04:05")))
		}
		buf.WriteString("to:\n")
		for _, localUpsert := range ret {
			buf.WriteString(fmt.Sprintf("  [%s, %s, %s]\n", localUpsert.ID, localUpsert.Path, time.UnixMilli(localUpsert.Updated).Format("2006-01-02 15:04:05")))
		}
		if 1 > len(ret) {
			buf.WriteString("  []")
		}
		logging.LogWarn(buf.String())
	}
	return
}

func (repo *Repo) getSyncCloudFiles(cloudLatest *entity.Index, context map[string]interface{}) (fetchedFiles []*entity.File, trafficStat *DownloadTrafficStat, err error) {
	trafficStat = &DownloadTrafficStat{}
	latest, err := repo.Latest()
	if nil != err {
		logging.LogErrorf("get latest failed: %s", err)
		return
	}

	if cloudLatest.ID == latest.ID {
		// 数据一致，直接返回
		return
	}

	availableSize := repo.cloud.GetAvailableSize()
	if availableSize <= cloudLatest.Size || availableSize <= latest.Size {
		err = ErrCloudStorageSizeExceeded
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
	if nil != downloadResult.err {
		err = downloadResult.err
		logging.LogErrorf("download cloud files put failed: %s", err)
		return
	}
	fetchedFiles, _ = repo.filterProtectedSyncFiles(fetchedFiles)
	trafficStat.DownloadBytes += downloadResult.bytes
	trafficStat.DownloadFileCount += len(fetchFileIDs)
	trafficStat.PeerDownloadBytes += downloadResult.peerBytes
	trafficStat.PeerDownloadFileCount += downloadResult.peerCount
	trafficStat.PeerFallbackCount += downloadResult.peerFallbackCount

	// 统计流量
	go repo.cloud.AddTraffic(&cloud.Traffic{
		DownloadBytes: trafficStat.DownloadBytes,
		APIGet:        len(fetchFileIDs) - downloadResult.peerCount,
	})
	return
}

func (repo *Repo) downloadCloudChunksPut(chunkIDs []string, context map[string]interface{}) (stat *chunkDownloadStat, err error) {
	result := repo.downloadCloudChunksPutDetailed(chunkIDs, context)
	stat = &chunkDownloadStat{
		CloudBytes:        result.bytes,
		PeerBytes:         result.peerBytes,
		PeerCount:         result.peerCount,
		PeerFallbackCount: result.peerFallbackCount,
	}
	return stat, result.err
}

func (repo *Repo) downloadCloudChunksPutDetailed(chunkIDs []string, context map[string]interface{}) concurrentObjectTransferResult {
	if 1 > len(chunkIDs) {
		return concurrentObjectTransferResult{}
	}

	peerChunks := map[string]bool{}
	if nil != repo.chunkSource {
		if found, hasErr := repo.chunkSource.HasChunks(chunkIDs); nil == hasErr {
			peerChunks = found
		} else {
			logging.LogWarnf("query chunk source [%s] failed: %s", repo.chunkSource.Name(), hasErr)
		}
	}

	cloudConcurrentReqs := repo.cloud.GetConcurrentReqs()
	if cloudConcurrentReqs < 1 {
		cloudConcurrentReqs = 1
	}
	peerConcurrentReqs := 0
	if nil != repo.chunkSource {
		peerConcurrentReqs = repo.chunkSource.GetConcurrentReqs()
		if peerConcurrentReqs < 1 {
			peerConcurrentReqs = 1
		}
	}
	cloudSemaphore := make(chan struct{}, cloudConcurrentReqs)
	peerSemaphore := make(chan struct{}, peerConcurrentReqs)
	peerBytes := atomic.Int64{}
	peerCount := atomic.Int64{}
	peerFallbackCount := atomic.Int64{}
	total := len(chunkIDs)
	eventbus.Publish(eventbus.EvtCloudBeforeDownloadChunks, context, total)
	ret := runConcurrentObjectTransfers(chunkIDs, cloudConcurrentReqs+peerConcurrentReqs, "download chunk",
		func(chunkID string, attempt int) (int64, error) {
			var length int64
			var chunk *entity.Chunk
			var dccErr error
			downloadedFromPeer := false
			if peerChunks[chunkID] {
				peerSemaphore <- struct{}{}
				length, chunk, dccErr = repo.downloadSourceChunk(chunkID)
				<-peerSemaphore
				if nil == dccErr {
					downloadedFromPeer = true
					peerBytes.Add(length)
					peerCount.Add(1)
				} else {
					peerFallbackCount.Add(1)
					logging.LogWarnf("download chunk [%s] from source [%s] failed, falling back to cloud: %s",
						chunkID, repo.chunkSource.Name(), dccErr)
				}
			}
			if nil == chunk {
				cloudSemaphore <- struct{}{}
				length, chunk, dccErr = repo.downloadCloudChunk(chunkID, attempt, total, context)
				<-cloudSemaphore
			}
			if nil != dccErr {
				return 0, dccErr
			}
			if pcErr := repo.store.PutChunk(chunk); nil != pcErr {
				return 0, pcErr
			}
			if downloadedFromPeer {
				// 对等下载流量单独统计，不计入云端下载字节数。
				return 0, nil
			}
			return length, nil
		})
	ret.peerBytes = peerBytes.Load()
	ret.peerCount = int(peerCount.Load())
	ret.peerFallbackCount = int(peerFallbackCount.Load())
	return ret
}

func (repo *Repo) downloadCloudFilesPut(fileIDs []string, context map[string]interface{}) (downloadBytes int64, ret []*entity.File, err error) {
	result, ret := repo.downloadCloudFilesPutDetailed(fileIDs, context)
	return result.bytes, ret, result.err
}

func (repo *Repo) downloadCloudFilesPutDetailed(fileIDs []string, context map[string]interface{}) (result concurrentObjectTransferResult, ret []*entity.File) {
	peerFiles := map[string]bool{}
	objectSource, hasObjectSource := repo.chunkSource.(ObjectSource)
	if hasObjectSource {
		if found, hasErr := objectSource.HasObjects(fileIDs); nil == hasErr {
			peerFiles = found
		} else {
			logging.LogWarnf("query object source [%s] failed: %s", objectSource.Name(), hasErr)
		}
	}
	cloudConcurrentReqs := repo.cloud.GetConcurrentReqs()
	if cloudConcurrentReqs < 1 {
		cloudConcurrentReqs = 1
	}
	peerConcurrentReqs := 0
	if hasObjectSource {
		peerConcurrentReqs = objectSource.GetConcurrentReqs()
		if peerConcurrentReqs < 1 {
			peerConcurrentReqs = 1
		}
	}
	cloudSemaphore := make(chan struct{}, cloudConcurrentReqs)
	peerSemaphore := make(chan struct{}, peerConcurrentReqs)
	peerBytes := atomic.Int64{}
	peerCount := atomic.Int64{}
	peerFallbackCount := atomic.Int64{}
	retLock := &sync.Mutex{}
	total := len(fileIDs)
	eventbus.Publish(eventbus.EvtCloudBeforeDownloadFiles, context, total)
	result = runConcurrentObjectTransfers(fileIDs, cloudConcurrentReqs+peerConcurrentReqs, "download file",
		func(fileID string, attempt int) (int64, error) {
			var length int64
			var file *entity.File
			var dcfErr error
			downloadedFromPeer := false
			if peerFiles[fileID] {
				peerSemaphore <- struct{}{}
				length, file, dcfErr = repo.downloadSourceFile(fileID)
				<-peerSemaphore
				if nil == dcfErr {
					downloadedFromPeer = true
					peerBytes.Add(length)
					peerCount.Add(1)
				} else {
					peerFallbackCount.Add(1)
					logging.LogWarnf("download file [%s] from source [%s] failed, falling back to cloud: %s",
						fileID, objectSource.Name(), dcfErr)
				}
			}
			if nil == file {
				cloudSemaphore <- struct{}{}
				length, file, dcfErr = repo.downloadCloudFile(fileID, attempt, total, context)
				<-cloudSemaphore
			}
			if nil != dcfErr {
				return 0, dcfErr
			}
			if pfErr := repo.store.PutFile(file); nil != pfErr {
				return 0, pfErr
			}

			retLock.Lock()
			ret = append(ret, file)
			retLock.Unlock()
			if downloadedFromPeer {
				return 0, nil
			}
			return length, nil
		})
	result.peerBytes = peerBytes.Load()
	result.peerCount = int(peerCount.Load())
	result.peerFallbackCount = int(peerFallbackCount.Load())
	return
}

func (repo *Repo) getFile(files []*entity.File, file *entity.File) *entity.File {
	for _, f := range files {
		if f.ID == file.ID || f.Path == file.Path {
			return f
		}
	}
	return nil
}

func (repo *Repo) updateCloudRef(ref string, context map[string]interface{}) (uploadBytes int64, err error) {
	eventbus.Publish(eventbus.EvtCloudBeforeUploadRef, context, ref)
	absFilePath := filepath.Join(repo.cloud.GetConf().RepoPath, ref)
	data, err := os.ReadFile(absFilePath)
	if nil != err {
		logging.LogErrorf("read ref [%s] failed: %s", ref, err)
		return
	}

	length, err := repo.cloud.UploadObject(ref, true)
	uploadBytes += length
	logging.LogInfof("uploaded cloud ref [%s, id=%s]", ref, data)
	return
}

var uploadedCloudMissingObjects = false

func (repo *Repo) uploadCloudMissingObjects(trafficStat *TrafficStat, context map[string]interface{}) {
	if uploadedCloudMissingObjects {
		return
	}
	uploadedCloudMissingObjects = true

	if _, ok := repo.cloud.(*cloud.SiYuan); !ok {
		return
	}

	defer eventbus.Publish(eventbus.EvtCloudAfterFixObjects, context)

	checkReportKey := "check/indexes-report"
	data, err := repo.cloud.DownloadObject(checkReportKey)
	if nil != err {
		if errors.Is(err, cloud.ErrCloudObjectNotFound) {
			return
		}

		logging.LogErrorf("download check report failed: %s", err)
		return
	}
	trafficStat.m.Lock()
	trafficStat.DownloadFileCount++
	trafficStat.DownloadBytes += int64(len(data))
	trafficStat.APIGet++
	trafficStat.m.Unlock()

	data, err = repo.store.compressDecoder.DecodeAll(data, nil)
	if nil != err {
		logging.LogErrorf("decompress check report failed: %s", err)
		return
	}

	checkReport := &entity.CheckReport{}
	if err = gulu.JSON.UnmarshalJSON(data, checkReport); nil != err {
		logging.LogErrorf("unmarshal check report failed: %s", err)
		return
	}

	if 1 > len(checkReport.MissingObjects) {
		return
	}

	var missingObjects []string
	stillMissingObjects := map[string]bool{}
	for _, missingObject := range checkReport.MissingObjects {
		logging.LogInfof("cloud missing object [%s]", missingObject)
		stillMissingObjects[missingObject] = true

		absFilePath := filepath.Join(repo.Path, "objects", missingObject)
		_, statErr := os.Stat(absFilePath)
		if nil != statErr {
			// 本地没有该文件，忽略
			logging.LogWarnf("cloud missing object [%s] not found: %s", missingObject, statErr)
			continue
		}

		missingObjects = append(missingObjects, missingObject)
	}
	missingObjects = gulu.Str.RemoveDuplicatedElem(missingObjects)

	total := len(missingObjects)
	lock := sync.Mutex{}
	result := runConcurrentObjectTransfers(missingObjects, repo.cloud.GetConcurrentReqs(), "upload missing object",
		func(objectPath string, attempt int) (int64, error) {
			filePath := "objects/" + objectPath
			eventbus.Publish(eventbus.EvtCloudBeforeFixObjects, context, attempt, total)
			length, uoErr := repo.cloud.UploadObject(filePath, false)
			if nil != uoErr {
				logging.LogErrorf("upload cloud missing object [%s] failed: %s", filePath, uoErr)
				return 0, uoErr
			}

			lock.Lock()
			delete(stillMissingObjects, objectPath)
			lock.Unlock()
			logging.LogInfof("uploaded cloud missing object [%s]", filePath)
			return length, nil
		})
	trafficStat.m.Lock()
	trafficStat.UploadBytes += result.bytes
	trafficStat.UploadFileCount += result.completed
	trafficStat.APIPut += result.attempted
	trafficStat.m.Unlock()
	if nil != result.err {
		logging.LogWarnf("upload cloud missing objects failed: %s", result.err)
		return
	}

	checkReport.FixCount++
	checkReport.MissingObjects = nil
	for missingObject := range stillMissingObjects {
		checkReport.MissingObjects = append(checkReport.MissingObjects, missingObject)
		logging.LogWarnf("cloud still missing object [%s]", missingObject)
	}

	if 0 < len(checkReport.MissingObjects) {
		eventbus.Publish(eventbus.EvtCloudCorrupted)
		logging.LogWarnf("cloud still missing objects [%d]", len(checkReport.MissingObjects))
	} else {
		logging.LogInfof("cloud missing objects fixed")
	}

	data, err = gulu.JSON.MarshalJSON(checkReport)
	if nil != err {
		logging.LogErrorf("marshal check report failed: %s", err)
		return
	}

	data = repo.store.compressEncoder.EncodeAll(data, nil)

	absPath := filepath.Join(repo.Path, checkReportKey)
	if err = gulu.File.WriteFileSafer(absPath, data, 0644); nil != err {
		logging.LogErrorf("write check report failed: %s", err)
		return
	}

	if _, err = repo.cloud.UploadObject(checkReportKey, true); nil != err {
		logging.LogErrorf("upload check report failed: %s", err)
	}
	return
}

func (repo *Repo) updateCloudCheckIndex(checkIndex *entity.CheckIndex, context map[string]interface{}) (err error) {
	switch repo.cloud.(type) {
	case *cloud.SiYuan, *cloud.Local:
	default:
		// S3/WebDAV 不上传校验索引 S3/WebDAV data sync no longer uploads check index https://github.com/siyuan-note/siyuan/issues/10180
		return
	}

	eventbus.Publish(eventbus.EvtCloudBeforeUploadCheckIndex, context)

	data, marshalErr := gulu.JSON.MarshalIndentJSON(checkIndex, "", "\t")
	if nil != marshalErr {
		logging.LogErrorf("marshal check index failed: %s", marshalErr)
		err = marshalErr
		return
	}

	data = repo.store.compressEncoder.EncodeAll(data, nil)

	dir := filepath.Join(repo.Path, "check", "indexes")
	if err = os.MkdirAll(dir, 0755); nil != err {
		return
	}

	if err = gulu.File.WriteFileSafer(filepath.Join(dir, checkIndex.ID), data, 0644); nil != err {
		logging.LogErrorf("write check index failed: %s", err)
		return
	}

	if _, err = repo.cloud.UploadObject("check/indexes/"+checkIndex.ID, false); nil != err {
		logging.LogErrorf("upload check index failed: %s", err)
		return
	}
	readback, err := repo.cloud.DownloadObject("check/indexes/" + checkIndex.ID)
	if nil != err {
		return fmt.Errorf("read back check index [%s] failed: %w", checkIndex.ID, err)
	}
	readback, err = repo.store.compressDecoder.DecodeAll(readback, nil)
	if nil != err {
		return fmt.Errorf("decode check index [%s] readback failed: %w", checkIndex.ID, err)
	}
	verified := &entity.CheckIndex{}
	if err = gulu.JSON.UnmarshalJSON(readback, verified); nil != err {
		return fmt.Errorf("unmarshal check index [%s] readback failed: %w", checkIndex.ID, err)
	}
	if !sameCheckIndex(checkIndex, verified) {
		return fmt.Errorf("check index [%s] readback identity mismatch", checkIndex.ID)
	}
	return
}

func sameCheckIndex(expected, actual *entity.CheckIndex) bool {
	if nil == expected || nil == actual || expected.ID != actual.ID || expected.IndexID != actual.IndexID || len(expected.Files) != len(actual.Files) {
		return false
	}
	for i, expectedFile := range expected.Files {
		actualFile := actual.Files[i]
		if nil == expectedFile || nil == actualFile || expectedFile.ID != actualFile.ID || len(expectedFile.Chunks) != len(actualFile.Chunks) {
			return false
		}
		for j, chunk := range expectedFile.Chunks {
			if chunk != actualFile.Chunks[j] {
				return false
			}
		}
	}
	return true
}

func (repo *Repo) updateCloudIndexesV2(latest *entity.Index, context map[string]interface{}) (downloadBytes, uploadBytes int64, err error) {
	eventbus.Publish(eventbus.EvtCloudBeforeUploadIndexes, context)

	data, err := repo.cloud.DownloadObject("indexes-v2.json")
	if nil != err {
		if !errors.Is(err, cloud.ErrCloudObjectNotFound) {
			return
		}
		err = nil
	}
	downloadBytes = int64(len(data))

	data, err = repo.store.compressDecoder.DecodeAll(data, nil)
	if nil != err {
		logging.LogErrorf("decompress cloud indexes-v2.json failed: %s", err)
		return
	}

	indexes := &cloud.Indexes{}
	if 0 < len(data) {
		if err = gulu.JSON.UnmarshalJSON(data, &indexes); nil != err {
			logging.LogWarnf("unmarshal cloud indexes-v2.json failed: %s", err)
			return
		}

		// Deduplication when uploading cloud snapshot indexes https://github.com/siyuan-note/siyuan/issues/8424
		found := false
		tmp := &cloud.Indexes{}
		added := map[string]bool{}
		for _, index := range indexes.Indexes {
			if index.ID == latest.ID {
				found = true
			}

			if !added[index.ID] {
				tmp.Indexes = append(tmp.Indexes, index)
				added[index.ID] = true
			}
		}
		if found {
			return
		}
		indexes = tmp
	}

	indexes.Indexes = append([]*cloud.Index{
		{
			ID:         latest.ID,
			SystemID:   latest.SystemID,
			SystemName: latest.SystemName,
			SystemOS:   latest.SystemOS,
		},
	}, indexes.Indexes...)
	if data, err = gulu.JSON.MarshalIndentJSON(indexes, "", "\t"); nil != err {
		logging.LogErrorf("marshal cloud indexes-v2.json failed: %s", err)
		return
	}

	data = repo.store.compressEncoder.EncodeAll(data, nil)

	if err = gulu.File.WriteFileSafer(filepath.Join(repo.Path, "indexes-v2.json"), data, 0644); nil != err {
		logging.LogErrorf("write indexes-v2.json failed: %s", err)
		return
	}

	length, err := repo.cloud.UploadObject("indexes-v2.json", true)
	uploadBytes = length
	return
}

func (repo *Repo) uploadIndex(index *entity.Index, context map[string]interface{}) (uploadBytes int64, err error) {
	eventbus.Publish(eventbus.EvtCloudBeforeUploadIndex, context, index.ID)
	length, err := repo.cloud.UploadObject(path.Join("indexes", index.ID), false)
	uploadBytes += length
	logging.LogInfof("uploaded index [%s]", index.String())
	return
}

func (repo *Repo) uploadFiles(upsertFiles []*entity.File, context map[string]interface{}) (uploadBytes int64, err error) {
	result := repo.uploadFilesDetailed(upsertFiles, context)
	return result.bytes, result.err
}

func (repo *Repo) uploadFilesDetailed(upsertFiles []*entity.File, context map[string]interface{}) concurrentObjectTransferResult {
	ids := make([]string, 0, len(upsertFiles))
	for _, file := range upsertFiles {
		ids = append(ids, file.ID)
	}
	total := len(upsertFiles)
	eventbus.Publish(eventbus.EvtCloudBeforeUploadFiles, context, total)
	return runConcurrentObjectTransfers(ids, repo.cloud.GetConcurrentReqs(), "upload file",
		func(upsertFileID string, attempt int) (int64, error) {
			filePath := path.Join("objects", upsertFileID[:2], upsertFileID[2:])
			eventbus.Publish(eventbus.EvtCloudBeforeUploadFile, context, attempt, total)
			length, uoErr := repo.cloud.UploadObject(filePath, false)
			if nil != uoErr {
				return 0, uoErr
			}
			return length, nil
		})
}

func (repo *Repo) uploadChunks(upsertChunkIDs []string, context map[string]interface{}) (uploadBytes int64, err error) {
	result := repo.uploadChunksDetailed(upsertChunkIDs, context)
	return result.bytes, result.err
}

func (repo *Repo) uploadChunksDetailed(upsertChunkIDs []string, context map[string]interface{}) concurrentObjectTransferResult {
	total := len(upsertChunkIDs)
	eventbus.Publish(eventbus.EvtCloudBeforeUploadChunks, context, total)
	return runConcurrentObjectTransfers(upsertChunkIDs, repo.cloud.GetConcurrentReqs(), "upload chunk",
		func(upsertChunkID string, attempt int) (int64, error) {
			filePath := path.Join("objects", upsertChunkID[:2], upsertChunkID[2:])
			eventbus.Publish(eventbus.EvtCloudBeforeUploadChunk, context, attempt, total)
			length, uoErr := repo.cloud.UploadObject(filePath, false)
			if nil != uoErr {
				return 0, uoErr
			}
			return length, nil
		})
}

func (repo *Repo) localNotFoundChunks(chunkIDs []string) (ret []string, err error) {
	for _, chunkID := range chunkIDs {
		if _, getChunkErr := repo.store.Stat(chunkID); nil != getChunkErr {
			if isNoSuchFileOrDirErr(getChunkErr) {
				ret = append(ret, chunkID)
				continue
			}
			err = getChunkErr
			return
		}
	}
	ret = gulu.Str.RemoveDuplicatedElem(ret)
	return
}

func (repo *Repo) localNotFoundFiles(fileIDs []string) (ret []string, err error) {
	for _, fileID := range fileIDs {
		if _, getFileErr := repo.store.Stat(fileID); nil != getFileErr {
			if isNoSuchFileOrDirErr(getFileErr) {
				ret = append(ret, fileID)
				continue
			}
			err = getFileErr
			return
		}
	}
	ret = gulu.Str.RemoveDuplicatedElem(ret)
	return
}

func (repo *Repo) getChunks(files []*entity.File) (chunkIDs []string) {
	for _, file := range files {
		chunkIDs = append(chunkIDs, file.Chunks...)
	}
	chunkIDs = gulu.Str.RemoveDuplicatedElem(chunkIDs)
	return
}

func (repo *Repo) localUpsertChunkIDs(localFiles []*entity.File, cloudChunkIDs []string) (ret []string, err error) {
	chunks := map[string]bool{}
	for _, file := range localFiles {
		//logging.LogInfof("upsert file [%s, %s, %s] chunk [%s]",
		//	file.ID, file.Path, time.UnixMilli(file.Updated).Format("2006-01-02 15:04:05"), strings.Join(file.Chunks, ","))
		for _, chunkID := range file.Chunks {
			chunks[chunkID] = true
		}
	}

	for _, cloudChunkID := range cloudChunkIDs {
		delete(chunks, cloudChunkID)
	}

	for chunkID := range chunks {
		ret = append(ret, chunkID)
	}

	//for _, c := range ret {
	//	logging.LogInfof("upsert chunk [%s]", c)
	//}
	return
}

func normalizeLazyPath(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(path), "/")
}

func isIgnoredLazyAssetPath(path string) bool {
	return filepath.Base(normalizeLazyPath(path)) == ".DS_Store"
}

func appendSyncSample(samples *[]string, format string, args ...interface{}) {
	const limit = 8
	if len(*samples) >= limit {
		return
	}
	*samples = append(*samples, fmt.Sprintf(format, args...))
}

func (repo *Repo) localUpsertFiles(latest *entity.Index, cloudLatest *entity.Index, context map[string]interface{}) (ret []*entity.File, err error) {
	// 处理普通文件
	files := map[string]bool{}
	for _, file := range latest.Files {
		files[file] = true
	}

	for _, cloudFileID := range cloudLatest.Files {
		delete(files, cloudFileID)
	}

	for fileID := range files {
		file, getErr := repo.store.GetFile(fileID)
		if nil != getErr {
			logging.LogErrorf("get file [%s] failed: %s", fileID, getErr)
			return
		}
		if nil == file {
			logging.LogErrorf("file [%s] not found", fileID)
			err = ErrNotFoundObject
			return
		}

		if !repo.isProtectedSyncPath(file.Path) {
			ret = append(ret, file)
		}
	}
	// format 2 清单是懒加载目录的权威根对象，不属于普通文件或 LazyFiles，变化时仍必须随事务上传其元数据和分块。
	if "" != latest.LazyManifest && latest.LazyManifest != cloudLatest.LazyManifest {
		if !containsUploadID(latest.Files, latest.LazyManifest) {
			manifestFile, getErr := repo.store.GetFile(latest.LazyManifest)
			if nil != getErr {
				return nil, fmt.Errorf("get lazy manifest file [%s] failed: %w", latest.LazyManifest, getErr)
			}
			ret = append(ret, manifestFile)
		}
	}
	lazyFiles := map[string]bool{}
	for _, file := range latest.LazyFiles {
		lazyFiles[file] = true
	}

	for _, cloudFileID := range cloudLatest.LazyFiles {
		delete(lazyFiles, cloudFileID)
	}

	logging.LogInfof("localUpsertFiles: start latestFiles=%d cloudFiles=%d normalCandidates=%d latestLazy=%d cloudLazy=%d lazyCandidatesByID=%d",
		len(latest.Files), len(cloudLatest.Files), len(files), len(latest.LazyFiles), len(cloudLatest.LazyFiles), len(lazyFiles))

	cloudLazyByPath := map[string]*entity.File{}
	var cloudLazyLoadErrors int
	var cloudLazyLoadErrorSamples []string
	if 0 < len(lazyFiles) {
		for _, cloudFileID := range cloudLatest.LazyFiles {
			cloudFile, getErr := repo.store.GetFile(cloudFileID)
			if nil != getErr || nil == cloudFile {
				cloudLazyLoadErrors++
				appendSyncSample(&cloudLazyLoadErrorSamples, "%s:%v", cloudFileID, getErr)
				continue
			}
			cloudLazyByPath[normalizeLazyPath(cloudFile.Path)] = cloudFile
		}
		logging.LogInfof("localUpsertFiles: built cloud lazy path map paths=%d loadErrors=%d samples=%v",
			len(cloudLazyByPath), cloudLazyLoadErrors, cloudLazyLoadErrorSamples)
	}

	normalUpsertCount := len(ret)
	var includedLazy, pathMatchedDifferentID, noCloudPathMatch, skippedMissingChunks int
	var missingChunkCount, emptyChunkCount, rebuiltCount, rebuildFailedCount, rebuiltStillMissingCount int
	var missingChunkFiles []*entity.File
	var pathMismatchSamples, noCloudPathSamples, missingChunkSamples, rebuiltSamples, skippedSamples []string
	for fileID := range lazyFiles {
		file, getErr := repo.store.GetFile(fileID)
		if nil != getErr {
			logging.LogErrorf("get lazy file [%s] failed: %s", fileID, getErr)
			return
		}
		if nil == file {
			logging.LogErrorf("lazy file [%s] not found", fileID)
			err = ErrNotFoundObject
			return
		}

		normalizedPath := normalizeLazyPath(file.Path)
		if cloudFile := cloudLazyByPath[normalizedPath]; nil != cloudFile && cloudFile.ID != file.ID {
			pathMatchedDifferentID++
			appendSyncSample(&pathMismatchSamples, "%s localID=%s cloudID=%s localUpdated=%d cloudUpdated=%d localChunks=%d cloudChunks=%d",
				normalizedPath, file.ID, cloudFile.ID, file.Updated, cloudFile.Updated, len(file.Chunks), len(cloudFile.Chunks))
		} else if nil == cloudFile {
			noCloudPathMatch++
			appendSyncSample(&noCloudPathSamples, "%s localID=%s updated=%d chunks=%d", normalizedPath, file.ID, file.Updated, len(file.Chunks))
		}

		// 验证懒加载文件的chunks是否存在，如果不存在则尝试从本地原始文件重建。
		missingChunks := false
		if 0 == len(file.Chunks) && 0 < file.Size {
			emptyChunkCount++
			missingChunks = true
			appendSyncSample(&missingChunkSamples, "%s id=%s size=%d chunks=0", normalizedPath, file.ID, file.Size)
		}
		for _, chunkID := range file.Chunks {
			_, chunkErr := repo.store.Stat(chunkID)
			if chunkErr != nil {
				logging.LogWarnf("localUpsertFiles: lazy file [%s] has missing chunk [%s]", file.Path, chunkID)
				missingChunkCount++
				missingChunks = true
				appendSyncSample(&missingChunkSamples, "%s id=%s missingChunk=%s chunks=%d", normalizedPath, file.ID, chunkID, len(file.Chunks))
				break
			}
		}

		if missingChunks {
			rebuilt, rebuildErr := repo.rebuildLazyFileChunksIfSourceExists(file, context)
			if rebuildErr != nil {
				rebuildFailedCount++
				logging.LogWarnf("localUpsertFiles: rebuild lazy file chunks [%s] failed: %s", file.Path, rebuildErr)
				appendSyncSample(&skippedSamples, "%s id=%s rebuildErr=%v", normalizedPath, file.ID, rebuildErr)
			}
			if rebuilt {
				rebuiltCount++
				missingChunks = false
				for _, chunkID := range file.Chunks {
					if _, chunkErr := repo.store.Stat(chunkID); chunkErr != nil {
						rebuiltStillMissingCount++
						logging.LogWarnf("localUpsertFiles: rebuilt lazy file [%s] still has missing chunk [%s]", file.Path, chunkID)
						missingChunks = true
						appendSyncSample(&skippedSamples, "%s id=%s rebuiltMissingChunk=%s", normalizedPath, file.ID, chunkID)
						break
					}
				}
				if !missingChunks {
					logging.LogInfof("localUpsertFiles: rebuilt lazy file chunks [%s], allowing upload", file.Path)
					appendSyncSample(&rebuiltSamples, "%s id=%s chunks=%d", normalizedPath, file.ID, len(file.Chunks))
				}
			}
		}

		if repo.isProtectedSyncPath(file.Path) {
			continue
		}
		if !missingChunks {
			includedLazy++
			ret = append(ret, file)
		} else {
			skippedMissingChunks++
			missingChunkFiles = append(missingChunkFiles, file)
			appendSyncSample(&skippedSamples, "%s id=%s chunks=%d", normalizedPath, file.ID, len(file.Chunks))
		}
	}
	if 0 < len(missingChunkFiles) {
		if markErr := repo.markLazyFilesError(missingChunkFiles); nil != markErr {
			logging.LogWarnf("mark lazy files error failed: %s", markErr)
		}
	}
	logging.LogInfof("localUpsertFiles: summary normalCandidates=%d lazyIncluded=%d lazySkippedMissing=%d pathMatchedDifferentID=%d noCloudPathMatch=%d missingChunk=%d emptyChunks=%d rebuilt=%d rebuildFailed=%d rebuiltStillMissing=%d",
		normalUpsertCount, includedLazy, skippedMissingChunks, pathMatchedDifferentID, noCloudPathMatch, missingChunkCount, emptyChunkCount, rebuiltCount, rebuildFailedCount, rebuiltStillMissingCount)
	if 0 < len(pathMismatchSamples) {
		logging.LogInfof("localUpsertFiles: pathMatchedDifferentID samples=%v", pathMismatchSamples)
	}
	if 0 < len(noCloudPathSamples) {
		logging.LogInfof("localUpsertFiles: noCloudPathMatch samples=%v", noCloudPathSamples)
	}
	if 0 < len(missingChunkSamples) {
		logging.LogInfof("localUpsertFiles: missingChunk samples=%v", missingChunkSamples)
	}
	if 0 < len(rebuiltSamples) {
		logging.LogInfof("localUpsertFiles: rebuilt samples=%v", rebuiltSamples)
	}
	if 0 < len(skippedSamples) {
		logging.LogInfof("localUpsertFiles: skipped samples=%v", skippedSamples)
	}
	return
}

func (repo *Repo) lockDeviceLocalSyncFiles() func() {
	refUsedPath := filepath.Join(repo.DataPath, "storage", "ref-used.json")
	filelock.Lock(refUsedPath)
	return func() {
		filelock.Unlock(refUsedPath)
	}
}

func (repo *Repo) resolveSyncPath(filePath string) (absPath string, safe bool) {
	filePath = strings.ReplaceAll(filePath, "\\", "/")
	filePath = strings.TrimLeft(filePath, "/")
	absPath = filepath.Clean(filepath.Join(repo.DataPath, filepath.FromSlash(filePath)))
	relPath, err := filepath.Rel(filepath.Clean(repo.DataPath), absPath)
	if nil != err || "." == relPath || !filepath.IsLocal(relPath) {
		return absPath, false
	}
	return absPath, true
}

func (repo *Repo) isProtectedSyncPath(filePath string) bool {
	absPath, safe := repo.resolveSyncPath(filePath)
	if !safe {
		return true
	}
	refUsedPath := filepath.Join(repo.DataPath, "storage", "ref-used.json")
	if "darwin" == runtime.GOOS || "windows" == runtime.GOOS {
		return strings.EqualFold(refUsedPath, absPath)
	}
	return refUsedPath == absPath
}

func (repo *Repo) filterProtectedSyncFiles(files []*entity.File) (ret []*entity.File, filtered bool) {
	for _, file := range files {
		if nil != file && repo.isProtectedSyncPath(file.Path) {
			filtered = true
			continue
		}
		ret = append(ret, file)
	}
	return
}

func (repo *Repo) filterProtectedMergeResult(mergeResult *MergeResult) {
	if nil == mergeResult {
		return
	}
	mergeResult.Upserts, _ = repo.filterProtectedSyncFiles(mergeResult.Upserts)
	mergeResult.Removes, _ = repo.filterProtectedSyncFiles(mergeResult.Removes)
	mergeResult.Conflicts, _ = repo.filterProtectedSyncFiles(mergeResult.Conflicts)
}

func (repo *Repo) sanitizeStoredIndex(index *entity.Index, memo string) (ret *entity.Index, sanitized bool, err error) {
	if nil == index {
		return index, false, nil
	}
	markerPath := filepath.Join(repo.Path, "sanitized-current-index")
	marker := "1:" + index.ID
	if data, readErr := os.ReadFile(markerPath); nil == readErr && string(data) == marker {
		return index, false, nil
	}
	files, err := repo.getFiles(index.Files)
	if nil != err {
		return nil, false, err
	}
	lazyFiles, err := repo.getFiles(index.LazyFiles)
	if nil != err {
		return nil, false, err
	}
	ret, sanitized, err = repo.sanitizeIndexFiles(index, files, lazyFiles, memo)
	if nil != err {
		return
	}
	if !sanitized {
		if err = gulu.File.WriteFileSafer(markerPath, []byte(marker), 0644); nil != err {
			return nil, false, err
		}
		return
	}
	err = repo.UpdateLatest(ret)
	if nil == err {
		err = gulu.File.WriteFileSafer(markerPath, []byte("1:"+ret.ID), 0644)
	}
	return
}

func (repo *Repo) sanitizeIndexFiles(index *entity.Index, files, lazyFiles []*entity.File, memo string) (ret *entity.Index, sanitized bool, err error) {
	filteredFiles, filesFiltered := repo.filterProtectedSyncFiles(files)
	filteredLazyFiles, lazyFilesFiltered := repo.filterProtectedSyncFiles(lazyFiles)
	if !filesFiltered && !lazyFilesFiltered {
		return index, false, nil
	}

	cleaned := *index
	cleaned.ID = util.RandHash()
	cleaned.Memo = memo
	cleaned.Created = time.Now().UnixMilli()
	cleaned.CheckIndexID = ""
	cleaned.Files = make([]string, 0, len(filteredFiles))
	cleaned.LazyFiles = make([]string, 0, len(filteredLazyFiles))
	cleaned.Size = 0
	for _, file := range filteredFiles {
		cleaned.Files = append(cleaned.Files, file.ID)
		cleaned.Size += file.Size
	}
	for _, file := range filteredLazyFiles {
		cleaned.LazyFiles = append(cleaned.LazyFiles, file.ID)
		cleaned.Size += file.Size
	}
	cleaned.Count = len(cleaned.Files)
	if err = repo.store.PutIndex(&cleaned); nil != err {
		return nil, false, err
	}
	return &cleaned, true, nil
}

func (repo *Repo) UpdateLatestSync(index *entity.Index) (err error) {
	refs := filepath.Join(repo.Path, "refs")
	err = os.MkdirAll(refs, 0755)
	if nil != err {
		return
	}
	err = gulu.File.WriteFileSafer(filepath.Join(refs, "latest-sync"), []byte(index.ID), 0644)
	if nil != err {
		return
	}
	logging.LogInfof("updated latest sync [%s]", index.String())
	return
}

func (repo *Repo) uploadCloud(context map[string]interface{},
	latest, cloudLatest *entity.Index, cloudChunkIDs []string, trafficStat *TrafficStat) (err error) {
	start := time.Now()
	logging.LogInfof("uploadCloud: start latestID=%s cloudLatestID=%s latestFiles=%d latestLazy=%d cloudFiles=%d cloudLazy=%d cloudChunks=%d",
		latest.ID, cloudLatest.ID, len(latest.Files), len(latest.LazyFiles), len(cloudLatest.Files), len(cloudLatest.LazyFiles), len(cloudChunkIDs))

	// 计算待上传云端的本地变更文件
	upsertFiles, err := repo.localUpsertFiles(latest, cloudLatest, context)
	if nil != err {
		logging.LogErrorf("get local upsert files failed: %s", err)
		return
	}

	if 1 > len(upsertFiles) {
		logging.LogInfof("uploadCloud: no upsert files latestID=%s cost=%s", latest.ID, time.Since(start))
		return
	}
	var upsertLazyFiles int
	for _, file := range upsertFiles {
		if nil != file && (strings.HasPrefix(file.Path, "assets/") || strings.HasPrefix(file.Path, "/assets/")) {
			upsertLazyFiles++
		}
	}
	logging.LogInfof("uploadCloud: upsert files total=%d lazy=%d normal=%d",
		len(upsertFiles), upsertLazyFiles, len(upsertFiles)-upsertLazyFiles)

	// 计算待上传云端的分块
	upsertChunkIDs, err := repo.localUpsertChunkIDs(upsertFiles, cloudChunkIDs)
	if nil != err {
		logging.LogErrorf("get local upsert chunk ids failed: %s", err)
		return
	}
	logging.LogInfof("uploadCloud: upsert chunks=%d", len(upsertChunkIDs))
	uploadFileIDs := make([]string, 0, len(upsertFiles))
	for _, file := range upsertFiles {
		uploadFileIDs = append(uploadFileIDs, file.ID)
	}
	uploadTx, txErr := repo.beginUploadTransaction(latest.ID, upsertChunkIDs, uploadFileIDs)
	if nil != txErr {
		return txErr
	}
	if _, saveErr := repo.verifyAndRecordUploadIDs(uploadTx, true, completedUploadIDs(uploadTx.CompletedChunks), trafficStat, context); nil != saveErr {
		return saveErr
	}
	if _, saveErr := repo.verifyAndRecordUploadIDs(uploadTx, false, completedUploadIDs(uploadTx.CompletedFiles), trafficStat, context); nil != saveErr {
		return saveErr
	}
	upsertChunkIDs, _ = pendingUploadIDs(upsertChunkIDs, uploadTx.CompletedChunks)

	// 上传分块
	uploadResult := repo.uploadChunksDetailed(upsertChunkIDs, context)
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
		return saveErr
	}
	if nil != uploadResult.err {
		return uploadResult.err
	}
	if nil != verificationErr {
		return verificationErr
	}
	logging.LogInfof("uploadCloud: uploaded chunks=%d bytes=%d", uploadResult.completed, uploadResult.bytes)

	// 上传文件
	pendingFileIDs, _ := pendingUploadIDs(uploadFileIDs, uploadTx.CompletedFiles)
	pendingFileSet := make(map[string]bool, len(pendingFileIDs))
	for _, id := range pendingFileIDs {
		pendingFileSet[id] = true
	}
	pendingFiles := make([]*entity.File, 0, len(pendingFileIDs))
	for _, file := range upsertFiles {
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
		return saveErr
	}
	if nil != uploadResult.err {
		return uploadResult.err
	}
	if nil != verificationErr {
		return verificationErr
	}
	logging.LogInfof("uploadCloud: uploaded files=%d lazy=%d bytes=%d cost=%s",
		uploadResult.completed, upsertLazyFiles, uploadResult.bytes, time.Since(start))
	return
}

func (repo *Repo) verifyUploadedChunks(ids []string, context map[string]interface{}) (downloadBytes int64, verified, apiGets int, err error) {
	for i, id := range ids {
		apiGets++
		length, _, downloadErr := repo.downloadCloudChunk(id, i+1, len(ids), context)
		downloadBytes += length
		if nil != downloadErr {
			err = fmt.Errorf("verify uploaded chunk [%s] failed: %w", id, downloadErr)
			return
		}
		verified++
	}
	return
}

func (repo *Repo) verifyUploadedFiles(ids []string, context map[string]interface{}) (downloadBytes int64, verified, apiGets int, err error) {
	for i, id := range ids {
		apiGets++
		length, file, downloadErr := repo.downloadCloudFile(id, i+1, len(ids), context)
		downloadBytes += length
		if nil != downloadErr {
			err = fmt.Errorf("verify uploaded file [%s] failed: %w", id, downloadErr)
			return
		}
		if nil == file || file.ID != id {
			err = fmt.Errorf("verify uploaded file [%s] failed: metadata ID mismatch", id)
			return
		}
		expected, getErr := repo.store.GetFile(id)
		if nil != getErr {
			err = fmt.Errorf("load local file metadata [%s] for verification failed: %w", id, getErr)
			return
		}
		if !reflect.DeepEqual(file, expected) {
			err = fmt.Errorf("verify uploaded file [%s] failed: metadata identity mismatch", id)
			return
		}
		verified++
	}
	return
}

func (repo *Repo) latestSync() (ret *entity.Index) {
	ret = &entity.Index{} // 构造一个空的索引表示没有同步点

	latestSync := filepath.Join(repo.Path, "refs", "latest-sync")
	if !filelock.IsExist(latestSync) {
		logging.LogInfof("latest sync index not found, return an empty index")
		return
	}

	data, err := filelock.ReadFile(latestSync)
	if nil != err {
		logging.LogWarnf("read latest sync index failed: %s", err)
		return
	}
	hash := string(data)
	hash = strings.TrimSpace(hash)
	if "" == hash {
		logging.LogWarnf("read latest sync index hash is empty")
		return
	}

	ret, err = repo.store.GetIndex(hash)
	if nil != err {
		logging.LogWarnf("get latest sync index failed: %s", err)
		ret = &entity.Index{}
		return
	}
	logging.LogInfof("got latest sync [%s]", ret.String())
	return
}

func (repo *Repo) downloadCloudChunk(id string, count, total int, context map[string]interface{}) (length int64, ret *entity.Chunk, err error) {
	eventbus.Publish(eventbus.EvtCloudBeforeDownloadChunk, context, count, total)

	if err = validateChunkID(id); nil != err {
		return
	}
	key := path.Join("objects", id[:2], id[2:])
	data, err := repo.downloadCloudObject(key)
	if nil != err {
		logging.LogErrorf("download cloud chunk [%s] failed: %s", id, err)
		return
	}
	if err = validateChunkData(id, data); nil != err {
		logging.LogErrorf("verify cloud chunk [%s] failed: %s", id, err)
		return
	}
	length = int64(len(data))
	ret = &entity.Chunk{ID: id, Data: data}
	return
}

func (repo *Repo) downloadSourceChunk(id string) (length int64, ret *entity.Chunk, err error) {
	key := path.Join("objects", id[:2], id[2:])
	var decoded []byte
	validate := func(data []byte) (validateErr error) {
		decoded, validateErr = repo.decodeDownloadedData(key, data)
		if nil == validateErr && util.Hash(decoded) != id {
			validateErr = fmt.Errorf("%w: source chunk [%s] hash mismatch", ErrRepoFatal, id)
		}
		return
	}
	data, err := repo.downloadSourceObject(id, validate)
	if nil != err {
		return
	}
	length = int64(len(data))
	ret = &entity.Chunk{ID: id, Data: decoded}
	return
}

func (repo *Repo) downloadSourceFile(id string) (length int64, ret *entity.File, err error) {
	source, ok := repo.chunkSource.(ObjectSource)
	if !ok {
		return 0, nil, errors.New("object source unavailable")
	}
	key := path.Join("objects", id[:2], id[2:])
	validate := func(data []byte) (validateErr error) {
		data, validateErr = repo.decodeDownloadedData(key, data)
		if nil != validateErr {
			return
		}
		file := &entity.File{}
		if validateErr = gulu.JSON.UnmarshalJSON(data, file); nil != validateErr {
			return
		}
		if file.ID != id || entity.NewFile(file.Path, file.Size, file.Updated).ID != id {
			return fmt.Errorf("%w: source file [%s] ID mismatch", ErrRepoFatal, id)
		}
		ret = file
		return
	}
	data, err := source.DownloadObjectValidated(id, validate)
	if nil == err {
		length = int64(len(data))
	}
	return
}

func (repo *Repo) downloadSourceObject(id string, validate func(data []byte) error) (data []byte, err error) {
	if source, ok := repo.chunkSource.(ValidatingChunkSource); ok {
		return source.DownloadChunkValidated(id, validate)
	}
	data, err = repo.chunkSource.DownloadChunk(id)
	if nil == err {
		err = validate(data)
	}
	return
}

func (repo *Repo) downloadCloudFile(id string, count, total int, context map[string]interface{}) (length int64, ret *entity.File, err error) {
	eventbus.Publish(eventbus.EvtCloudBeforeDownloadFile, context, count, total)

	key := path.Join("objects", id[:2], id[2:])
	data, err := repo.downloadCloudObject(key)
	if nil != err {
		logging.LogErrorf("download cloud file [%s] failed: %s", id, err)
		return
	}
	length = int64(len(data))
	ret = &entity.File{}
	err = gulu.JSON.UnmarshalJSON(data, ret)
	return
}

func (repo *Repo) downloadCloudObject(filePath string) (ret []byte, err error) {
	data, err := repo.cloud.DownloadObject(filePath)
	if nil != err {
		return
	}

	ret, err = repo.decodeDownloadedData(filePath, data)
	if nil != err {
		return
	}
	//logging.LogInfof("downloaded object [%s]", filePath)
	return
}

func (repo *Repo) decodeDownloadedData(key string, data []byte) (ret []byte, err error) {
	ret = data
	if strings.Contains(key, "objects") {
		ret, err = repo.store.decodeData(ret)
		if nil != err {
			logging.LogErrorf("decode downloaded data [%s] failed: %s", key, err)
			return
		}
	} else if strings.Contains(key, "indexes") {
		ret, err = repo.store.compressDecoder.DecodeAll(ret, nil)
	}
	if nil != err {
		logging.LogErrorf("decode downloaded data [%s] failed: %s", key, err)
		return
	}
	return
}

func (repo *Repo) downloadCloudIndex(id string, context map[string]interface{}) (downloadBytes int64, index *entity.Index, err error) {
	eventbus.Publish(eventbus.EvtCloudBeforeDownloadIndex, context, id)
	index = &entity.Index{}

	key := path.Join("indexes", id)
	data, err := repo.downloadCloudObject(key)
	if nil != err {
		return
	}
	err = gulu.JSON.UnmarshalJSON(data, index)
	if nil != err {
		return
	}
	downloadBytes += int64(len(data))

	if !index.VerifyAESKey(repo.store.AesKey) {
		err = cloud.ErrDecryptFailed
		logging.LogErrorf("cloud index [%s] verify AES key failed", index.String())
		return
	}
	return
}

func (repo *Repo) downloadCloudLatest(context map[string]interface{}) (downloadBytes int64, apiGets int, index *entity.Index, err error) {
	start := time.Now()
	index = &entity.Index{}

	key := path.Join("refs", "latest")
	eventbus.Publish(eventbus.EvtCloudBeforeDownloadRef, context, "refs/latest")
	apiGets++
	data, err := repo.downloadCloudObject(key)
	if nil != err {
		if errors.Is(err, cloud.ErrCloudObjectNotFound) {
			logging.LogWarnf("not found cloud latest")
			err = nil
			return
		}

		logging.LogErrorf("download cloud latest failed: %s", err)
		return
	}

	latestID := strings.TrimSpace(string(data))
	if 40 != len(latestID) {
		err = cloud.ErrCloudObjectNotFound
		logging.LogWarnf("got empty cloud latest")
		return
	}

	isS3OrSiYuan := repo.isCloudS3() || repo.isCloudSiYuan()
	apiGets++
	waitGroup := sync.WaitGroup{}
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()

		downloadBytes, index, err = repo.downloadCloudIndex(latestID, context)
	}()

	var seqNumLatestID string
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()

		if isS3OrSiYuan {
			// 确认下载到的是最新索引 https://github.com/siyuan-note/siyuan/issues/12991
			seqNumLatestID, _, _ = repo.getSeqNumLatest()
		}
	}()
	waitGroup.Wait()

	if isS3OrSiYuan && ("" != seqNumLatestID && "" != index.ID && latestID != seqNumLatestID) {
		logging.LogWarnf("cloud latest [%s] not match seq num latest [%s]", latestID, seqNumLatestID)
		// 以时间较新的为准
		apiGets++
		_, seqNumLatest, downloadErr := repo.downloadCloudIndex(seqNumLatestID, context)
		if nil != downloadErr {
			logging.LogWarnf("download seq num latest [%s] failed: %s", seqNumLatestID, downloadErr)
		} else {
			if seqNumLatest.Created > index.Created {
				logging.LogWarnf("use seq num latest [%s] instead of cloud latest [%s]", seqNumLatest, index)
				index = seqNumLatest
			} else {
				logging.LogWarnf("still use cloud latest [%s] rather than seq num latest [%s]", index, seqNumLatest)
			}
		}
	}

	if !index.VerifyAESKey(repo.store.AesKey) {
		err = cloud.ErrDecryptFailed
		logging.LogErrorf("cloud latest [%s] verify AES key failed", index.String())
		return
	}

	logging.LogInfof("got cloud latest [%s], cost [%s]", index.String(), time.Since(start))
	return
}

func (repo *Repo) getSeqNumLatest() (id string, maxSeqNum int, seqNumLatests []string) {
	refs, listErr := repo.cloud.ListObjects("refs/")
	if nil != listErr {
		logging.LogErrorf("list refs failed: %s", listErr)
		return
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref.Path, "latest-") {
			continue
		}

		p := strings.TrimPrefix(ref.Path, "latest-")
		parts := strings.Split(p, "-")
		if 2 > len(parts) {
			repo.cloud.RemoveObject("refs/" + ref.Path)
			continue
		}

		seqNum, _ := strconv.Atoi(parts[0])
		if seqNum > maxSeqNum {
			maxSeqNum = seqNum
			id = parts[1]
		}

		seqNumLatests = append(seqNumLatests, "refs/"+ref.Path)
	}
	return
}

func (repo *Repo) genSyncHistory(now, relPath, absPath string) (err error) {
	historyDir, err := repo.getHistoryDirNow(now, "sync")
	if nil != err {
		return
	}

	historyPath := filepath.Join(historyDir, relPath)
	if err = gulu.File.Copy(absPath, historyPath); nil != err {
		return
	}
	return
}

func (repo *Repo) getHistoryDirNow(now, suffix string) (ret string, err error) {
	ret = filepath.Join(repo.HistoryPath, now+"-"+suffix)
	err = os.MkdirAll(ret, 0755)
	return
}

func (repo *Repo) CheckoutFilesFromCloud(files []*entity.File, context map[string]interface{}) (stat *DownloadTrafficStat, err error) {
	stat = &DownloadTrafficStat{}
	files, _ = repo.filterProtectedSyncFiles(files)

	chunkIDs := repo.getChunks(files)
	chunkIDs, err = repo.localNotFoundChunks(chunkIDs)
	if nil != err {
		return
	}

	downloadResult := repo.downloadCloudChunksPutDetailed(chunkIDs, context)
	stat.DownloadBytes = downloadResult.bytes
	stat.DownloadChunkCount = downloadResult.completed
	stat.PeerDownloadBytes = downloadResult.peerBytes
	stat.PeerDownloadChunkCount = downloadResult.peerCount
	stat.PeerFallbackCount = downloadResult.peerFallbackCount
	if nil != downloadResult.err {
		err = downloadResult.err
		return
	}

	err = repo.checkoutFiles(files, context)
	return
}

func (repo *Repo) RemoveCloudRepo(name string) (err error) {
	lock.Lock()
	defer lock.Unlock()

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToStatusBar}
	err = repo.tryLockCloud("remove", context)
	if nil != err {
		return
	}
	defer repo.unlockCloud(context)

	return repo.cloud.RemoveRepo(name)
}

func (repo *Repo) CreateCloudRepo(name string) (err error) {
	lock.Lock()
	defer lock.Unlock()

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToStatusBar}
	err = repo.tryLockCloud("create", context)
	if nil != err {
		return
	}
	defer repo.unlockCloud(context)

	return repo.cloud.CreateRepo(name)
}

func (repo *Repo) GetCloudRepos() (repos []*cloud.Repo, size int64, err error) {
	return repo.cloud.GetRepos()
}

func (repo *Repo) GetCloudAvailableSize() (ret int64) {
	return repo.cloud.GetAvailableSize()
}

func (repo *Repo) GetCloudRepoStat() (stat *cloud.Stat, err error) {
	return repo.cloud.GetStat()
}

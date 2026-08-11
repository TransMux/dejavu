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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/88250/gulu"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/dejavu/util"
	"github.com/siyuan-note/logging"
)

// LazyStatus 懒加载状态
type LazyStatus int

const (
	LazyStatusPending LazyStatus = iota
	LazyStatusDownloading
	LazyStatusCached
	LazyStatusError
)

// LazyAsset 懒加载资源描述
type LazyAsset struct {
	Path     string   `json:"path"`
	FileID   string   `json:"fileId"`
	Size     int64    `json:"size"`
	Hash     string   `json:"hash"`
	Modified int64    `json:"mtime"`
	Chunks   []string `json:"chunks"`
	// RevivesDeletion proves that this version was created after observing the
	// indicated tombstone. A later filesystem mtime alone is not such proof.
	RevivesDeletion int64 `json:"revivesDeletion,omitempty"`
	// Status is device-local materialization state. It is derived from local
	// files and must never participate in the synchronized resource catalog.
	Status LazyStatus `json:"-"`
}

// LazyManifest 懒加载清单
type LazyManifest struct {
	Version    string                    `json:"version"`
	Assets     map[string]*LazyAsset     `json:"assets"`
	Tombstones map[string]*LazyTombstone `json:"tombstones,omitempty"`
	Updated    int64                     `json:"updated"`
}

const (
	lazyManifestFormatLegacy       = "1.0"
	lazyManifestFormatCurrent      = "2.0"
	lazyManifestHydratedMarker     = "lazy-manifest-format-2-hydrated"
	lazyManifestLocalClosureMarker = "lazy-manifest-format-2-local-closure"
)

type LazyTombstone struct {
	Path      string `json:"path"`
	FileID    string `json:"fileId"`
	DeletedAt int64  `json:"deletedAt"`
}

// LazyManifestDelta 描述两个懒加载清单之间的共享目录变化。
type LazyManifestDelta struct {
	Adds    []*LazyAsset
	Updates []*LazyAsset
	Deletes []*LazyTombstone
	Revives []*LazyAsset
}

// LazyLoader 懒加载管理器
type LazyLoader struct {
	repo        *Repo
	manifest    *LazyManifest
	cache       map[string]*LazyAsset
	downloading map[string]chan error
	mutex       sync.RWMutex
}

// NewLazyLoader 创建懒加载管理器
func NewLazyLoader(repo *Repo) *LazyLoader {
	return &LazyLoader{
		repo:        repo,
		cache:       make(map[string]*LazyAsset),
		downloading: make(map[string]chan error),
	}
}

// LoadAsset 加载资源文件
func (ll *LazyLoader) LoadAsset(path string) error {
	ll.mutex.Lock()
	defer ll.mutex.Unlock()

	// 检查是否已经在下载中
	if ch, exists := ll.downloading[path]; exists {
		ll.mutex.Unlock()
		err := <-ch
		ll.mutex.Lock()
		return err
	}

	// 检查本地是否存在
	localPath := filepath.Join(ll.repo.DataPath, path)
	if gulu.File.IsExist(localPath) {
		if asset := ll.cache[path]; asset != nil {
			asset.Status = LazyStatusCached
		}
		return nil
	}

	// 获取资源信息
	manifest, err := ll.getManifest()
	if err != nil {
		return fmt.Errorf("get manifest failed: %w", err)
	}

	// 尝试查找资源，支持两种路径格式
	asset, exists := manifest.Assets[path]
	if !exists && !strings.HasPrefix(path, "/") {
		altPath := "/" + path
		asset, exists = manifest.Assets[altPath]
	}
	if !exists && strings.HasPrefix(path, "/") {
		altPath := strings.TrimPrefix(path, "/")
		asset, exists = manifest.Assets[altPath]
	}

	if !exists {
		return fmt.Errorf("asset not found in manifest: %s", path)
	}

	// 创建下载通道
	ch := make(chan error, 1)
	ll.downloading[path] = ch
	asset.Status = LazyStatusDownloading

	// 异步下载
	go func() {
		defer func() {
			ll.mutex.Lock()
			delete(ll.downloading, path)
			ll.mutex.Unlock()
		}()

		err := ll.downloadAsset(asset)
		if err != nil {
			asset.Status = LazyStatusError
		} else {
			asset.Status = LazyStatusCached
		}

		ch <- err
		close(ch)
	}()

	ll.mutex.Unlock()
	err = <-ch
	ll.mutex.Lock()

	return err
}

// downloadAsset 下载单个资源文件
func (ll *LazyLoader) downloadAsset(asset *LazyAsset) error {
	// 创建目标目录
	cleanPath := strings.TrimPrefix(asset.Path, "/")
	localPath := filepath.Join(ll.repo.DataPath, cleanPath)

	dir := filepath.Dir(localPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir failed: %w", err)
	}

	// 下载所有chunks
	tmpPath := localPath + "." + gulu.Rand.String(7) + ".tmp"
	tmpFile, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create temp file failed: %w", err)
	}
	var written int64
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	for _, chunkID := range asset.Chunks {

		chunk, err := ll.repo.store.GetChunk(chunkID)
		if err != nil {
			// 如果本地没有，从云端下载
			if err = validateChunkID(chunkID); err != nil {
				_ = tmpFile.Close()
				return fmt.Errorf("invalid chunk [%s]: %w", chunkID, err)
			}
			chunkPath := fmt.Sprintf("objects/%s/%s", chunkID[:2], chunkID[2:])
			cloudData, downloadErr := ll.repo.cloud.DownloadObject(chunkPath)
			if downloadErr != nil {
				_ = tmpFile.Close()
				return fmt.Errorf("download chunk [%s] failed: %w", chunkID, downloadErr)
			}

			// 解码云端数据（解压缩和解密）
			decodedData, decodeErr := ll.repo.store.DecodeData(cloudData)
			if decodeErr != nil {
				_ = tmpFile.Close()
				return fmt.Errorf("decode chunk [%s] failed: %w", chunkID, decodeErr)
			}
			if verifyErr := validateChunkData(chunkID, decodedData); verifyErr != nil {
				_ = tmpFile.Close()
				return fmt.Errorf("verify chunk [%s] failed: %w", chunkID, verifyErr)
			}

			cloudChunk := &entity.Chunk{
				ID:   chunkID,
				Data: decodedData,
			}

			// 存储解码后的chunk到本地
			if putErr := ll.repo.store.PutChunk(cloudChunk); putErr != nil {
				_ = tmpFile.Close()
				return fmt.Errorf("put chunk [%s] failed: %w", chunkID, putErr)
			}

			chunk = cloudChunk
		}
		n, writeErr := tmpFile.Write(chunk.Data)
		written += int64(n)
		if writeErr != nil {
			_ = tmpFile.Close()
			return fmt.Errorf("write chunk [%s] failed: %w", chunkID, writeErr)
		}
	}
	if asset.Size >= 0 && written != asset.Size {
		_ = tmpFile.Close()
		return fmt.Errorf("asset size mismatch [%s]: manifest=%d written=%d", asset.Path, asset.Size, written)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file failed: %w", err)
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		return fmt.Errorf("rename temp file failed: %w", err)
	}
	cleanupTmp = false

	// 设置文件修改时间
	modTime := time.UnixMilli(asset.Modified)
	if err := os.Chtimes(localPath, modTime, modTime); err != nil {
		logging.LogWarnf("set file time failed: %s", err)
	}

	return nil
}

// IsAssetCached 检查资源是否已缓存
func (ll *LazyLoader) IsAssetCached(path string) bool {
	ll.mutex.RLock()
	defer ll.mutex.RUnlock()

	localPath := filepath.Join(ll.repo.DataPath, path)
	return gulu.File.IsExist(localPath)
}

// ClearCache 清理缓存
func (ll *LazyLoader) ClearCache() error {
	ll.mutex.Lock()
	defer ll.mutex.Unlock()

	manifest, err := ll.getManifest()
	if err != nil {
		return err
	}

	for path := range manifest.Assets {
		localPath := filepath.Join(ll.repo.DataPath, path)
		if gulu.File.IsExist(localPath) {
			if err := os.Remove(localPath); err != nil {
				logging.LogWarnf("remove cached file [%s] failed: %s", localPath, err)
			}
		}
	}
	return nil
}

// getManifest 获取懒加载清单
func (ll *LazyLoader) getManifest() (*LazyManifest, error) {
	if ll.manifest != nil {
		return ll.manifest, nil
	}

	manifestPath := ll.getManifestPath()
	if !gulu.File.IsExist(manifestPath) {
		ll.manifest = &LazyManifest{
			Version: lazyManifestFormatCurrent,
			Assets:  make(map[string]*LazyAsset),
			Updated: time.Now().UnixMilli(),
		}
		return ll.manifest, nil
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest failed: %w", err)
	}

	manifest := &LazyManifest{}
	if err := json.Unmarshal(data, manifest); err != nil {
		return nil, fmt.Errorf("unmarshal manifest failed: %w", err)
	}

	ll.manifest = manifest
	return manifest, nil
}

// saveManifest 保存懒加载清单
func (ll *LazyLoader) saveManifest(manifest *LazyManifest) error {
	manifest.Updated = time.Now().UnixMilli()

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest failed: %w", err)
	}

	manifestPath := ll.getManifestPath()
	previousInfo, statErr := os.Stat(manifestPath)
	if nil != statErr && !os.IsNotExist(statErr) {
		return statErr
	}
	dir := filepath.Dir(manifestPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create manifest dir failed: %w", err)
	}

	if err = gulu.File.WriteFileSafer(manifestPath, data, 0644); nil != err {
		return err
	}
	if nil != previousInfo {
		updated := time.Now()
		minimum := previousInfo.ModTime().Add(time.Second)
		if updated.Before(minimum) {
			updated = minimum
		}
		if err = os.Chtimes(manifestPath, updated, updated); nil != err {
			return err
		}
	}
	ll.manifest = manifest
	return nil
}

func (ll *LazyLoader) reloadManifest() error {
	ll.mutex.Lock()
	defer ll.mutex.Unlock()
	ll.manifest = nil
	_, err := ll.getManifest()
	return err
}

// getManifestPath 获取清单文件路径
func (ll *LazyLoader) getManifestPath() string {
	return filepath.Join(ll.repo.DataPath, ".siyuan", "lazy_manifest.json")
}

// updateLazyManifest 更新懒加载清单
func (repo *Repo) updateLazyManifest(lazyFiles []*entity.File) error {
	if !repo.lazyLoadEnabled || repo.lazyLoader == nil {
		return nil
	}

	manifest, err := repo.lazyLoader.getManifest()
	if err != nil {
		return fmt.Errorf("get manifest failed: %w", err)
	}

	// 记录冲突处理统计
	var conflictCount, mergedCount, newCount, leadingSlashPathCount int
	var normalizedPathSamples []string

	// 更新资源信息
	for _, file := range lazyFiles {
		if isIgnoredLazyAssetPath(file.Path) {
			delete(manifest.Assets, normalizeLazyPath(file.Path))
			continue
		}

		// 检查chunks是否有效
		if len(file.Chunks) == 0 && file.Size > 0 {
			logging.LogWarnf("updateLazyManifest: file [%s] has no chunks but size is %d bytes!", file.Path, file.Size)
			continue
		}

		// 尝试两种路径格式查找现有资源
		asset := manifest.Assets[file.Path]
		if asset == nil && !strings.HasPrefix(file.Path, "/") {
			// 尝试加前导斜杠查找
			altPath := "/" + file.Path
			asset = manifest.Assets[altPath]
		}
		if asset == nil && strings.HasPrefix(file.Path, "/") {
			// 尝试去掉前导斜杠查找
			altPath := strings.TrimPrefix(file.Path, "/")
			asset = manifest.Assets[altPath]
		}

		if asset == nil {
			asset = &LazyAsset{}
			newCount++
		} else {
			// 检测并处理冲突
			if repo.hasLazyFileConflict(asset, file) {
				conflictCount++
				// 冲突解决策略：优先使用更新的版本
				if file.Updated > asset.Modified {
					mergedCount++
				} else if file.Updated < asset.Modified {
					// 保留现有asset，不更新
					continue
				} else {
					// 时间相同，比较大小和chunks
					if file.Size != asset.Size || len(file.Chunks) != len(asset.Chunks) {
						mergedCount++
					} else {
						continue
					}
				}
			}
		}

		normalizedPath := normalizeLazyPath(file.Path)
		if tombstone := manifest.Tombstones[normalizedPath]; nil != tombstone {
			if file.Updated <= tombstone.DeletedAt {
				continue
			}
			asset.RevivesDeletion = tombstone.DeletedAt
			delete(manifest.Tombstones, normalizedPath)
		}
		if file.Path != normalizedPath {
			leadingSlashPathCount++
			appendSyncSample(&normalizedPathSamples, "%s -> %s", file.Path, normalizedPath)
		}

		canonical := entity.NewFile(normalizedPath, file.Size, file.Updated)
		canonical.Chunks = append([]string(nil), file.Chunks...)
		asset.Path = normalizedPath
		asset.FileID = canonical.ID
		asset.Size = file.Size
		asset.Modified = file.Updated
		asset.Chunks = file.Chunks

		// 检查本地是否存在，更新状态
		cleanPath := strings.TrimPrefix(file.Path, "/")
		localPath := filepath.Join(repo.DataPath, cleanPath)

		if gulu.File.IsExist(localPath) {
			asset.Status = LazyStatusCached
		} else {
			asset.Status = LazyStatusPending
		}
		repo.setLazyManifestAsset(manifest, asset)
	}

	logging.LogInfof("updateLazyManifest: updated %d assets (new: %d, conflicts: %d, merged: %d, normalizedLeadingSlash=%d, samples=%v)",
		len(lazyFiles), newCount, conflictCount, mergedCount, leadingSlashPathCount, normalizedPathSamples)
	if lazyManifestFormatCurrent == lazyManifestFormat(manifest) {
		if _, err = repo.canonicalizeLazyManifestObjectIdentities(manifest); nil != err {
			return fmt.Errorf("canonicalize lazy manifest metadata: %w", err)
		}
	}

	return repo.lazyLoader.saveManifest(manifest)
}

func mergeLazyManifestAssets(local, cloud *LazyManifest) *LazyManifest {
	merged := &LazyManifest{
		Version:    lazyManifestFormatLegacy,
		Assets:     map[string]*LazyAsset{},
		Tombstones: map[string]*LazyTombstone{},
		Updated:    time.Now().UnixMilli(),
	}
	if lazyManifestFormat(local) == lazyManifestFormatCurrent || lazyManifestFormat(cloud) == lazyManifestFormatCurrent {
		merged.Version = lazyManifestFormatCurrent
	}
	add := func(asset *LazyAsset) {
		if nil == asset || isIgnoredLazyAssetPath(asset.Path) {
			return
		}
		candidate := cloneLazyAsset(asset)
		candidate.Path = normalizeLazyPath(candidate.Path)
		existing := merged.Assets[candidate.Path]
		if nil == existing || shouldUseLazyAsset(candidate, existing) {
			merged.Assets[candidate.Path] = candidate
		}
	}
	if nil != local {
		for _, asset := range local.Assets {
			add(asset)
		}
	}
	if nil != cloud {
		for _, asset := range cloud.Assets {
			add(asset)
		}
	}
	mergeTombstones := func(manifest *LazyManifest) {
		if nil == manifest {
			return
		}
		for _, tombstone := range manifest.Tombstones {
			if nil == tombstone {
				continue
			}
			candidate := *tombstone
			candidate.Path = normalizeLazyPath(candidate.Path)
			existing := merged.Tombstones[candidate.Path]
			if nil == existing || candidate.DeletedAt > existing.DeletedAt {
				merged.Tombstones[candidate.Path] = &candidate
			}
		}
	}
	mergeTombstones(local)
	mergeTombstones(cloud)
	for path, tombstone := range merged.Tombstones {
		if nil == tombstone {
			continue
		}
		if asset := merged.Assets[path]; nil != asset && asset.RevivesDeletion >= tombstone.DeletedAt {
			delete(merged.Tombstones, path)
		} else {
			delete(merged.Assets, path)
		}
	}
	return merged
}

func lazyManifestFormat(manifest *LazyManifest) string {
	if nil == manifest || "" == manifest.Version || lazyManifestFormatLegacy == manifest.Version {
		return lazyManifestFormatLegacy
	}
	return manifest.Version
}

func validateLazyManifestFormat(manifest *LazyManifest) error {
	format := lazyManifestFormat(manifest)
	if lazyManifestFormatLegacy != format && lazyManifestFormatCurrent != format {
		return fmt.Errorf("unsupported lazy manifest repository format [%s]", format)
	}
	return nil
}

func validateCurrentLazyManifestCatalog(manifest *LazyManifest) error {
	if lazyManifestFormatCurrent != lazyManifestFormat(manifest) {
		return nil
	}
	paths := make(map[string]string, len(manifest.Assets))
	for key, asset := range manifest.Assets {
		if nil == asset {
			return fmt.Errorf("current lazy manifest asset [%s] is nil", key)
		}
		path := normalizeLazyPath(asset.Path)
		if key != path || asset.Path != path {
			return fmt.Errorf("current lazy manifest asset path is not canonical [key=%s, path=%s]", key, asset.Path)
		}
		if previous, exists := paths[path]; exists {
			return fmt.Errorf("duplicate current lazy manifest path [%s] from [%s] and [%s]", path, previous, key)
		}
		paths[path] = key
		if err := validateUploadTransactionID(asset.FileID); nil != err {
			return fmt.Errorf("current lazy manifest asset [%s] has invalid file ID: %w", path, err)
		}
		if expected := entity.NewFile(path, asset.Size, asset.Modified).ID; asset.FileID != expected {
			return fmt.Errorf("current lazy manifest asset [%s] file ID mismatch [got=%s, expected=%s]", path, asset.FileID, expected)
		}
	}
	return nil
}

func (repo *Repo) isLazyManifestHydrated(identity string) bool {
	if "" == identity {
		return false
	}
	data, err := os.ReadFile(filepath.Join(repo.Path, lazyManifestHydratedMarker))
	return nil == err && identity == string(data)
}

func (repo *Repo) markLazyManifestHydrated(identity string) error {
	if err := validateUploadTransactionID(identity); nil != err {
		return fmt.Errorf("mark consumed lazy manifest identity: %w", err)
	}
	marker := filepath.Join(repo.Path, lazyManifestHydratedMarker)
	temp := marker + ".tmp"
	if err := os.WriteFile(temp, []byte(identity), 0600); nil != err {
		return err
	}
	if err := os.Rename(temp, marker); nil != err {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

func (repo *Repo) lazyManifestIdentity() (string, error) {
	manifestPath := repo.lazyLoader.getManifestPath()
	info, err := os.Stat(manifestPath)
	if nil != err {
		return "", err
	}
	return entity.NewFile(repo.relPath(manifestPath), info.Size(), info.ModTime().UnixMilli()).ID, nil
}

func (repo *Repo) isLazyManifestLocalClosurePrepared(identity string) bool {
	data, err := os.ReadFile(filepath.Join(repo.Path, lazyManifestLocalClosureMarker))
	return nil == err && identity == string(data)
}

func (repo *Repo) markLazyManifestLocalClosurePrepared(identity string) error {
	marker := filepath.Join(repo.Path, lazyManifestLocalClosureMarker)
	temp := marker + ".tmp"
	if err := os.WriteFile(temp, []byte(identity), 0600); nil != err {
		return err
	}
	if err := os.Rename(temp, marker); nil != err {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

func (repo *Repo) saveLazyManifestWithNewIdentity(candidate *LazyManifest) error {
	manifestPath := repo.lazyLoader.getManifestPath()
	previousInfo, statErr := os.Stat(manifestPath)
	if nil != statErr && !os.IsNotExist(statErr) {
		return statErr
	}
	if err := repo.lazyLoader.saveManifest(candidate); nil != err {
		return err
	}
	if nil != previousInfo {
		updated := time.Now()
		minimum := previousInfo.ModTime().Add(time.Second)
		if updated.Before(minimum) {
			updated = minimum
		}
		if err := os.Chtimes(manifestPath, updated, updated); nil != err {
			return err
		}
	}
	return nil
}

func (repo *Repo) advanceLazyManifestIdentityPast(forbidden string) error {
	if "" == forbidden {
		return nil
	}
	manifestPath := repo.lazyLoader.getManifestPath()
	for {
		identity, err := repo.lazyManifestIdentity()
		if nil != err || identity != forbidden {
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

// canonicalizeLazyManifestObjectIdentities 为发生秒级标识冲突的清单项分配新的不可变对象标识。
func (repo *Repo) canonicalizeLazyManifestObjectIdentities(manifest *LazyManifest) (changed int, err error) {
	keys := make([]string, 0, len(manifest.Assets))
	for key := range manifest.Assets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	normalized := make(map[string]*LazyAsset, len(keys))
	legacyPathIdentity := make(map[string]bool, len(keys))
	for _, key := range keys {
		asset := manifest.Assets[key]
		if nil == asset {
			normalized[key] = nil
			continue
		}
		path := normalizeLazyPath(asset.Path)
		if _, exists := normalized[path]; exists {
			return changed, fmt.Errorf("duplicate lazy manifest path after normalization [%s]", path)
		}
		pathChanged := key != path || asset.Path != path
		if pathChanged {
			changed++
		}
		// 只允许修复由未规范化旧路径生成的标识，其他标识不匹配必须按清单损坏处理。
		legacyPathIdentity[path] = pathChanged && asset.FileID == entity.NewFile(asset.Path, asset.Size, asset.Modified).ID
		asset.Path = path
		normalized[path] = asset
	}
	manifest.Assets = normalized
	keys = keys[:0]
	for key := range manifest.Assets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		asset := manifest.Assets[key]
		if nil == asset {
			continue
		}
		desired := entity.NewFile(asset.Path, asset.Size, asset.Modified)
		desired.Chunks = append([]string(nil), asset.Chunks...)
		if desired.ID != asset.FileID {
			provenLegacyIdentity := legacyPathIdentity[asset.Path]
			if !provenLegacyIdentity {
				if nil == validateUploadTransactionID(asset.FileID) {
					legacy, getErr := repo.store.GetFile(asset.FileID)
					if nil == getErr && nil != legacy && legacy.ID == asset.FileID && normalizeLazyPath(legacy.Path) == asset.Path &&
						legacy.Size == asset.Size && legacy.Updated == asset.Modified && slices.Equal(legacy.Chunks, asset.Chunks) {
						provenLegacyIdentity = true
					}
				}
			}
			if !provenLegacyIdentity {
				return changed, fmt.Errorf("lazy manifest asset [%s] file ID mismatch [got=%s, expected=%s]", asset.Path,
					asset.FileID, desired.ID)
			}
			// 历史清单可能使用了未规范化路径生成的旧 ID，保留旧对象并切换到规范对象。
			existing, getErr := repo.store.GetFile(desired.ID)
			if os.IsNotExist(getErr) {
				if err = repo.store.PutFile(desired); nil != err {
					return changed, err
				}
				asset.FileID = desired.ID
				changed++
				continue
			}
			if nil != getErr {
				return changed, getErr
			}
			if existing.Path == desired.Path && existing.Size == desired.Size && existing.Updated == desired.Updated &&
				slices.Equal(existing.Chunks, desired.Chunks) {
				asset.FileID = desired.ID
				changed++
				continue
			}
		}
		_, objectPath := repo.store.AbsPath(desired.ID)
		if _, statErr := os.Stat(objectPath); os.IsNotExist(statErr) {
			if err = repo.store.PutFile(desired); nil != err {
				return changed, err
			}
			continue
		} else if nil != statErr {
			return changed, statErr
		}
		existing, getErr := repo.store.GetFile(desired.ID)
		if nil != getErr {
			return changed, getErr
		}
		if existing.Path == desired.Path && existing.Size == desired.Size && existing.Updated == desired.Updated &&
			slices.Equal(existing.Chunks, desired.Chunks) {
			continue
		}
		// File ID 仅绑定路径和秒级更新时间。同一秒内内容变化时必须生成新 ID，不能覆盖历史对象。
		for updated := (asset.Modified/1000 + 1) * 1000; ; updated += 1000 {
			candidate := entity.NewFile(asset.Path, asset.Size, updated)
			candidate.Chunks = append([]string(nil), asset.Chunks...)
			_, candidatePath := repo.store.AbsPath(candidate.ID)
			if _, statErr := os.Stat(candidatePath); os.IsNotExist(statErr) {
				asset.Modified = updated
				asset.FileID = candidate.ID
				if err = repo.store.PutFile(candidate); nil != err {
					return changed, err
				}
				changed++
				break
			} else if nil != statErr {
				return changed, statErr
			}
		}
	}
	return
}

func (repo *Repo) migrateLazyManifestRepository(source *LazyManifest) (migrated int, err error) {
	if !repo.lazyLoadEnabled || nil == repo.lazyLoader {
		return
	}
	repo.lazyLoader.mutex.Lock()
	defer repo.lazyLoader.mutex.Unlock()
	if nil == source {
		return 0, fmt.Errorf("lazy manifest migration source is nil")
	}
	candidate := cloneLazyManifest(source)
	keys := make([]string, 0, len(candidate.Assets))
	for key := range candidate.Assets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	normalized := make(map[string]*LazyAsset, len(keys))
	for _, key := range keys {
		asset := candidate.Assets[key]
		if nil == asset || isIgnoredLazyAssetPath(asset.Path) {
			continue
		}
		path := normalizeLazyPath(asset.Path)
		if _, exists := normalized[path]; exists {
			return 0, fmt.Errorf("duplicate lazy manifest path after normalization [%s]", path)
		}
		file := entity.NewFile(path, asset.Size, asset.Modified)
		file.Chunks = append([]string{}, asset.Chunks...)
		if err = repo.store.PutFile(file); nil != err {
			return 0, fmt.Errorf("store migrated lazy metadata [%s]: %w", path, err)
		}
		copy := cloneLazyAsset(asset)
		copy.Path = path
		copy.FileID = file.ID
		normalized[path] = copy
		migrated++
	}
	candidate.Assets = normalized
	candidate.Version = lazyManifestFormatCurrent
	if _, err = repo.canonicalizeLazyManifestObjectIdentities(candidate); nil != err {
		return 0, fmt.Errorf("canonicalize migrated lazy metadata identities: %w", err)
	}
	if err = repo.saveLazyManifestWithNewIdentity(candidate); nil != err {
		return 0, fmt.Errorf("save migrated lazy manifest: %w", err)
	}
	repo.lazyManifestMigrations.Add(1)
	return
}

func (repo *Repo) canonicalizeLazyManifestLocalDeltas(merged, published *LazyManifest) (canonicalized int, err error) {
	for path, asset := range merged.Assets {
		if nil == asset {
			continue
		}
		if cloudAsset := published.Assets[path]; nil != cloudAsset && cloudAsset.FileID == asset.FileID {
			continue
		}
		file := entity.NewFile(path, asset.Size, asset.Modified)
		file.Chunks = append([]string{}, asset.Chunks...)
		if err = repo.store.PutFile(file); nil != err {
			return canonicalized, fmt.Errorf("store canonical local lazy metadata [%s]: %w", path, err)
		}
		asset.FileID = file.ID
		canonicalized++
	}
	return
}

func isCanonicalLazyManifestSuperset(candidate, legacy *LazyManifest) bool {
	if lazyManifestFormatCurrent != lazyManifestFormat(candidate) {
		return false
	}
	for path, asset := range candidate.Assets {
		if nil == asset || path != normalizeLazyPath(asset.Path) {
			return false
		}
		file := entity.NewFile(path, asset.Size, asset.Modified)
		if asset.FileID != file.ID {
			return false
		}
	}
	for _, asset := range legacy.Assets {
		if nil == asset {
			continue
		}
		path := normalizeLazyPath(asset.Path)
		current := candidate.Assets[path]
		if nil == current || current.Modified != asset.Modified || current.Size != asset.Size ||
			!slices.Equal(current.Chunks, asset.Chunks) {
			return false
		}
	}
	for _, tombstone := range legacy.Tombstones {
		if nil == tombstone {
			continue
		}
		current := candidate.Tombstones[normalizeLazyPath(tombstone.Path)]
		if nil == current || current.FileID != tombstone.FileID || current.DeletedAt != tombstone.DeletedAt {
			return false
		}
	}
	return true
}

func (repo *Repo) DeleteLazyAsset(path, fileID string) error {
	if !repo.lazyLoadEnabled || nil == repo.lazyLoader {
		return nil
	}
	repo.lazyLoader.mutex.Lock()
	defer repo.lazyLoader.mutex.Unlock()
	manifest, err := repo.lazyLoader.getManifest()
	if nil != err {
		return err
	}
	manifest = cloneLazyManifest(manifest)
	path = normalizeLazyPath(path)
	asset := manifest.Assets[path]
	if nil == asset || asset.FileID != fileID {
		return fmt.Errorf("lazy asset version changed [%s]", path)
	}
	if nil == manifest.Tombstones {
		manifest.Tombstones = map[string]*LazyTombstone{}
	}
	manifest.Tombstones[path] = &LazyTombstone{Path: path, FileID: fileID, DeletedAt: time.Now().UnixMilli()}
	delete(manifest.Assets, path)
	delete(manifest.Assets, "/"+path)
	return repo.lazyLoader.saveManifest(manifest)
}

func cloneLazyManifest(manifest *LazyManifest) *LazyManifest {
	ret := &LazyManifest{Version: manifest.Version, Updated: manifest.Updated,
		Assets: make(map[string]*LazyAsset, len(manifest.Assets)), Tombstones: make(map[string]*LazyTombstone, len(manifest.Tombstones))}
	for path, asset := range manifest.Assets {
		if nil != asset {
			ret.Assets[path] = cloneLazyAsset(asset)
		}
	}
	for path, tombstone := range manifest.Tombstones {
		if nil != tombstone {
			copy := *tombstone
			ret.Tombstones[path] = &copy
		}
	}
	return ret
}

func (repo *Repo) LazyAssetID(path string) string {
	if !repo.lazyLoadEnabled || nil == repo.lazyLoader {
		return ""
	}
	repo.lazyLoader.mutex.RLock()
	defer repo.lazyLoader.mutex.RUnlock()
	manifest, err := repo.lazyLoader.getManifest()
	if nil != err {
		return ""
	}
	if asset := manifest.Assets[normalizeLazyPath(path)]; nil != asset {
		return asset.FileID
	}
	return ""
}

func cloneLazyAsset(asset *LazyAsset) *LazyAsset {
	ret := *asset
	ret.Chunks = append([]string{}, asset.Chunks...)
	return &ret
}

func equalLazyAssetCatalog(left, right *LazyAsset) bool {
	if nil == left || nil == right {
		return left == right
	}
	return normalizeLazyPath(left.Path) == normalizeLazyPath(right.Path) && left.FileID == right.FileID &&
		left.Size == right.Size && left.Hash == right.Hash && left.Modified == right.Modified &&
		left.RevivesDeletion == right.RevivesDeletion && slices.Equal(left.Chunks, right.Chunks)
}

func equalLazyTombstone(left, right *LazyTombstone) bool {
	if nil == left || nil == right {
		return left == right
	}
	return normalizeLazyPath(left.Path) == normalizeLazyPath(right.Path) && left.FileID == right.FileID &&
		left.DeletedAt == right.DeletedAt
}

func diffLazyManifests(previous, current *LazyManifest) (ret LazyManifestDelta, err error) {
	if nil == previous {
		previous = &LazyManifest{}
	}
	if nil == current {
		current = &LazyManifest{}
	}
	for path, currentAsset := range current.Assets {
		if nil == currentAsset {
			continue
		}
		path = normalizeLazyPath(path)
		previousAsset := previous.Assets[path]
		if nil == previousAsset {
			if tombstone := previous.Tombstones[path]; nil != tombstone &&
				currentAsset.RevivesDeletion >= tombstone.DeletedAt {
				ret.Revives = append(ret.Revives, currentAsset)
			} else {
				ret.Adds = append(ret.Adds, currentAsset)
			}
		} else if !equalLazyAssetCatalog(previousAsset, currentAsset) {
			ret.Updates = append(ret.Updates, currentAsset)
		}
	}
	for path, currentTombstone := range current.Tombstones {
		if nil == currentTombstone {
			continue
		}
		path = normalizeLazyPath(path)
		if previousAsset := previous.Assets[path]; nil != previousAsset ||
			!equalLazyTombstone(previous.Tombstones[path], currentTombstone) {
			ret.Deletes = append(ret.Deletes, currentTombstone)
		}
	}
	for path, previousAsset := range previous.Assets {
		path = normalizeLazyPath(path)
		if nil == previousAsset || nil != current.Assets[path] {
			continue
		}
		currentTombstone := current.Tombstones[path]
		if nil == currentTombstone || currentTombstone.FileID != previousAsset.FileID {
			return ret, fmt.Errorf("lazy manifest removed asset [%s] without a matching tombstone", path)
		}
	}
	for path, previousTombstone := range previous.Tombstones {
		path = normalizeLazyPath(path)
		if nil == previousTombstone || nil != current.Tombstones[path] {
			continue
		}
		currentAsset := current.Assets[path]
		if nil == currentAsset || currentAsset.RevivesDeletion < previousTombstone.DeletedAt {
			return ret, fmt.Errorf("lazy manifest removed tombstone [%s] without revival proof", path)
		}
	}
	return
}

func shouldUseLazyAsset(candidate, existing *LazyAsset) bool {
	if candidate.Modified != existing.Modified {
		return candidate.Modified > existing.Modified
	}
	if len(candidate.Chunks) != len(existing.Chunks) {
		return len(candidate.Chunks) > len(existing.Chunks)
	}
	return candidate.Size >= existing.Size
}

func (repo *Repo) mergeLazyManifestFile(localFile, cloudFile *entity.File, context map[string]interface{}) (err error) {
	if !repo.lazyLoadEnabled || repo.lazyLoader == nil {
		return nil
	}
	localManifest, err := repo.checkoutLazyManifest(localFile, "local", context)
	if nil != err {
		return fmt.Errorf("checkout local lazy manifest failed: %w", err)
	}
	cloudManifest, err := repo.checkoutLazyManifest(cloudFile, "cloud", context)
	if nil != err {
		return fmt.Errorf("checkout cloud lazy manifest failed: %w", err)
	}

	merged := mergeLazyManifestAssets(localManifest, cloudManifest)
	return repo.lazyLoader.saveManifest(merged)
}

func (repo *Repo) checkoutLazyManifest(file *entity.File, name string, context map[string]interface{}) (*LazyManifest, error) {
	if nil == file {
		return nil, fmt.Errorf("manifest file is nil")
	}
	dir := filepath.Join(repo.TempPath, "repo", "sync", "lazy-manifest", name)
	if err := repo.checkoutFile(file, dir, 1, 1, context); nil != err {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, ".siyuan", "lazy_manifest.json"))
	if nil != err {
		return nil, err
	}
	manifest := &LazyManifest{}
	if err = json.Unmarshal(data, manifest); nil != err {
		return nil, err
	}
	return manifest, nil
}

// updateLazyManifestFromCloudIndex 从云端索引更新懒加载清单，不下载文件内容
func (repo *Repo) updateLazyManifestFromCloudIndex(cloudLazyFileIDs []string, context map[string]interface{}) error {
	if !repo.lazyLoadEnabled || repo.lazyLoader == nil {
		logging.LogInfof("updateLazyManifestFromCloudIndex: lazy loading disabled, skipping")
		return nil
	}

	if len(cloudLazyFileIDs) == 0 {
		logging.LogInfof("updateLazyManifestFromCloudIndex: no cloud lazy files to process")
		return nil
	}

	logging.LogInfof("updateLazyManifestFromCloudIndex: processing %d cloud lazy files", len(cloudLazyFileIDs))

	manifest, err := repo.lazyLoader.getManifest()
	if err != nil {
		return fmt.Errorf("get manifest failed: %w", err)
	}
	if lazyManifestContainsFileIDs(manifest, cloudLazyFileIDs) {
		logging.LogInfof("updateLazyManifestFromCloudIndex: manifest already contains %d cloud lazy files, skipping", len(cloudLazyFileIDs))
		return nil
	}

	// 检查本地是否已有这些文件的元数据
	var missingFileIDs []string
	existingCount := 0

	for _, fileID := range cloudLazyFileIDs {
		file, getErr := repo.store.GetFile(fileID)
		if nil != getErr {
			// 本地没有元数据，需要从云端获取
			missingFileIDs = append(missingFileIDs, fileID)
		} else {
			if isIgnoredLazyAssetPath(file.Path) {
				delete(manifest.Assets, normalizeLazyPath(file.Path))
				continue
			}

			// 本地有元数据，直接更新清单
			existingCount++
			asset := &LazyAsset{
				Path:     normalizeLazyPath(file.Path),
				FileID:   file.ID,
				Size:     file.Size,
				Modified: file.Updated,
				Chunks:   file.Chunks,
				Status:   LazyStatusPending, // 默认设为待下载状态
			}

			// 检查本地文件是否存在
			cleanPath := strings.TrimPrefix(file.Path, "/")
			localPath := filepath.Join(repo.DataPath, cleanPath)
			if gulu.File.IsExist(localPath) {
				asset.Status = LazyStatusCached
			}

			repo.setLazyManifestAsset(manifest, asset)
		}
	}

	// 只下载缺失的元数据（不下载chunks内容）
	if len(missingFileIDs) > 0 {
		logging.LogInfof("updateLazyManifestFromCloudIndex: downloading metadata for %d missing files (no content)", len(missingFileIDs))

		_, cloudFiles, downloadErr := repo.downloadCloudFilesPut(missingFileIDs, context)
		if downloadErr != nil {
			logging.LogWarnf("updateLazyManifestFromCloudIndex: failed to download some metadata: %s", downloadErr)
			// 不中断流程，继续处理已有的
		} else {
			// 将下载的元数据添加到清单
			for _, cloudFile := range cloudFiles {
				if isIgnoredLazyAssetPath(cloudFile.Path) {
					delete(manifest.Assets, normalizeLazyPath(cloudFile.Path))
					continue
				}

				asset := &LazyAsset{
					Path:     normalizeLazyPath(cloudFile.Path),
					FileID:   cloudFile.ID,
					Size:     cloudFile.Size,
					Modified: cloudFile.Updated,
					Chunks:   cloudFile.Chunks,
					Status:   LazyStatusPending,
				}
				repo.setLazyManifestAsset(manifest, asset)
			}
			logging.LogInfof("updateLazyManifestFromCloudIndex: downloaded metadata for %d files", len(cloudFiles))
		}
	}

	logging.LogInfof("updateLazyManifestFromCloudIndex: updated manifest with %d existing + %d downloaded = %d total files",
		existingCount, len(missingFileIDs), len(cloudLazyFileIDs))

	return repo.lazyLoader.saveManifest(manifest)
}

func lazyManifestContainsFileIDs(manifest *LazyManifest, fileIDs []string) bool {
	if nil == manifest || len(manifest.Assets) != len(fileIDs) {
		return false
	}
	ids := make(map[string]struct{}, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		if nil == asset || "" == asset.FileID {
			return false
		}
		ids[asset.FileID] = struct{}{}
	}
	if len(ids) != len(fileIDs) {
		return false
	}
	for _, id := range fileIDs {
		if _, ok := ids[id]; !ok {
			return false
		}
	}
	return true
}

func (repo *Repo) setLazyManifestAsset(manifest *LazyManifest, asset *LazyAsset) {
	if nil == manifest.Assets {
		manifest.Assets = map[string]*LazyAsset{}
	}
	asset.Path = normalizeLazyPath(asset.Path)
	if nil != manifest.Tombstones[asset.Path] {
		return
	}
	delete(manifest.Assets, "/"+asset.Path)
	manifest.Assets[asset.Path] = asset
}

func (repo *Repo) markLazyFilesError(files []*entity.File) error {
	if !repo.lazyLoadEnabled || repo.lazyLoader == nil || 1 > len(files) {
		return nil
	}
	manifest, err := repo.lazyLoader.getManifest()
	if nil != err {
		return fmt.Errorf("get manifest failed: %w", err)
	}
	for _, file := range files {
		if nil == file {
			continue
		}
		path := normalizeLazyPath(file.Path)
		asset := manifest.Assets[path]
		if nil == asset {
			asset = manifest.Assets["/"+path]
		}
		if nil == asset {
			asset = &LazyAsset{
				Path:     path,
				FileID:   file.ID,
				Size:     file.Size,
				Modified: file.Updated,
				Chunks:   file.Chunks,
			}
		}
		asset.Status = LazyStatusError
		repo.setLazyManifestAsset(manifest, asset)
	}
	return repo.lazyLoader.saveManifest(manifest)
}

func (repo *Repo) rebuildLazyFileChunksIfSourceExists(file *entity.File, context map[string]interface{}) (rebuilt bool, err error) {
	if !repo.lazyLoadEnabled || repo.lazyLoader == nil || nil == file {
		return false, nil
	}
	if !strings.HasPrefix(file.Path, "assets/") && !strings.HasPrefix(file.Path, "/assets/") {
		return false, nil
	}

	absPath := repo.absPath(file.Path)
	if !gulu.File.IsExist(absPath) {
		return false, nil
	}

	logging.LogInfof("rebuildLazyFileChunksIfSourceExists: rebuilding lazy file chunks [%s]", file.Path)
	if err = repo.putFileChunks(file, context, 1, 1); nil != err {
		return false, err
	}
	if err = repo.updateLazyManifest([]*entity.File{file}); nil != err {
		return false, err
	}
	return true, nil
}

// hasLazyFileConflict 检测懒加载文件是否有冲突
func (repo *Repo) hasLazyFileConflict(asset *LazyAsset, file *entity.File) bool {
	// 检查是否为有意义的冲突
	if asset.Modified != file.Updated {
		return true // 修改时间不同
	}

	if asset.Size != file.Size {
		return true // 文件大小不同
	}

	if len(asset.Chunks) != len(file.Chunks) {
		return true // chunks数量不同
	}

	// 深度比较chunks
	for i, chunkID := range file.Chunks {
		if i >= len(asset.Chunks) || asset.Chunks[i] != chunkID {
			return true // chunks内容不同
		}
	}

	return false // 没有冲突
}

// getLazyFilesForIndex 获取懒加载文件的索引条目
func (repo *Repo) getLazyFilesForIndex() ([]*entity.File, error) {
	if !repo.lazyLoadEnabled || repo.lazyLoader == nil {
		logging.LogInfof("[DEBUG] getLazyFilesForIndex: lazy loading not enabled or loader is nil")
		return nil, nil
	}

	logging.LogInfof("[DEBUG] getLazyFilesForIndex: getting manifest...")
	manifest, err := repo.lazyLoader.getManifest()
	if err != nil {
		logging.LogErrorf("[DEBUG] getLazyFilesForIndex: get manifest failed: %s", err)
		return nil, fmt.Errorf("get manifest failed: %w", err)
	}

	logging.LogInfof("[DEBUG] getLazyFilesForIndex: manifest has %d assets", len(manifest.Assets))
	return repo.lazyManifestFilesForIndex(manifest)
}

func (repo *Repo) lazyManifestFilesForIndex(manifest *LazyManifest) ([]*entity.File, error) {
	var files []*entity.File
	assetKeys := make([]string, 0, len(manifest.Assets))
	for key := range manifest.Assets {
		assetKeys = append(assetKeys, key)
	}
	sort.Strings(assetKeys)
	canonicalPaths := make(map[string]string, len(assetKeys))
	for _, key := range assetKeys {
		asset := manifest.Assets[key]
		if nil == asset {
			continue
		}
		canonicalPath := normalizeLazyPath(asset.Path)
		if previous, exists := canonicalPaths[canonicalPath]; exists && previous != key {
			return nil, fmt.Errorf("duplicate lazy manifest path after normalization [%s]: keys [%s] and [%s]", canonicalPath, previous, key)
		}
		canonicalPaths[canonicalPath] = key
	}
	for _, key := range assetKeys {
		asset := manifest.Assets[key]
		if isIgnoredLazyAssetPath(asset.Path) {
			continue
		}

		// The manifest is the last fully chunked immutable version. A local file
		// may already have changed and will be handled by the upsert path; never
		// combine its new stat with the manifest's old chunks.
		localPath := filepath.Join(repo.DataPath, strings.TrimPrefix(asset.Path, "/"))
		if len(asset.Chunks) > 0 || gulu.File.IsExist(localPath) {
			file := &entity.File{ID: asset.FileID, Path: asset.Path, Size: asset.Size, Updated: asset.Modified,
				Chunks: append([]string{}, asset.Chunks...)}
			files = append(files, file)
		}
	}

	return files, nil
}

// isLazyFile 检查是否是懒加载文件
func (repo *Repo) isLazyFile(filePath string) bool {
	if !repo.lazyLoadEnabled || repo.lazyLoader == nil {
		return false
	}

	manifest, err := repo.lazyLoader.getManifest()
	if err != nil {
		return false
	}

	_, exists := manifest.Assets[filePath]
	return exists
}

// repairLazyDataConsistency 修复懒加载数据一致性问题
// 检查并修复：索引中有但清单中缺失的懒加载文件
// 如果索引中没有懒加载文件，则扫描本地assets文件夹并添加到清单中
func (repo *Repo) repairLazyDataConsistency(files *[]*entity.File) error {
	logging.LogInfof("repairLazyDataConsistency: starting repair process")

	if !repo.lazyLoadEnabled || repo.lazyLoader == nil {
		logging.LogWarnf("repairLazyDataConsistency: lazy loading not enabled or loader is nil")
		return nil
	}

	// 获取最新索引
	logging.LogInfof("repairLazyDataConsistency: getting latest index")
	latest, err := repo.Latest()
	if err != nil {
		if err == ErrNotFoundIndex {
			// 没有索引，尝试扫描本地assets文件夹
			logging.LogInfof("repairLazyDataConsistency: no index found, scanning local assets folder")
			_, scanErr := repo.scanLocalAssetsForRepair(files)
			return scanErr
		}
		return fmt.Errorf("get latest index failed: %w", err)
	}
	logging.LogInfof("repairLazyDataConsistency: latest index ID=%s, LazyFiles count=%d", latest.ID, len(latest.LazyFiles))

	// 获取当前清单
	logging.LogInfof("repairLazyDataConsistency: getting current manifest")
	manifest, err := repo.lazyLoader.getManifest()
	if err != nil {
		return fmt.Errorf("get manifest failed: %w", err)
	}
	logging.LogInfof("repairLazyDataConsistency: manifest has %d assets", len(manifest.Assets))

	// 构建清单中已有文件的ID映射和路径映射
	manifestFileIDs := make(map[string]bool)
	manifestPaths := make(map[string]bool)
	for _, asset := range manifest.Assets {
		manifestFileIDs[asset.FileID] = true
		manifestPaths[asset.Path] = true
	}
	logging.LogInfof("repairLazyDataConsistency: built manifest maps (fileIDs=%d, paths=%d)", len(manifestFileIDs), len(manifestPaths))

	var missingFiles []*entity.File
	var repairedCount int

	// 如果索引中有懒加载文件，检查索引中的文件是否在清单中缺失
	if len(latest.LazyFiles) > 0 {
		logging.LogInfof("repairLazyDataConsistency: checking %d lazy files from index", len(latest.LazyFiles))
		checkedCount := 0
		missingCount := 0
		skippedCount := 0

		for _, lazyFileID := range latest.LazyFiles {
			checkedCount++
			if !manifestFileIDs[lazyFileID] {
				missingCount++
				// 索引中有但清单中缺失，尝试从存储中恢复
				logging.LogInfof("repairLazyDataConsistency: file ID [%s] found in index but missing in manifest", lazyFileID)
				file, getErr := repo.store.GetFile(lazyFileID)
				if getErr != nil {
					logging.LogWarnf("repairLazyDataConsistency: cannot get file [%s] from store: %s", lazyFileID, getErr)
					skippedCount++
					continue
				}

				// 检查是否是assets文件
				if strings.HasPrefix(file.Path, "assets/") || strings.HasPrefix(file.Path, "/assets/") {
					logging.LogInfof("repairLazyDataConsistency: found missing lazy file in manifest [%s] (ID=%s, Size=%d), attempting repair", file.Path, file.ID, file.Size)

					// 检查本地文件是否存在
					cleanPath := strings.TrimPrefix(file.Path, "/")
					localPath := filepath.Join(repo.DataPath, cleanPath)

					if gulu.File.IsExist(localPath) {
						// 本地文件存在，更新文件信息并添加到files列表
						info, statErr := os.Stat(localPath)
						if statErr == nil {
							oldSize := file.Size
							oldUpdated := file.Updated
							file.Size = info.Size()
							file.Updated = info.ModTime().UnixMilli()
							logging.LogInfof("repairLazyDataConsistency: updated file info [%s]: size %d->%d, updated %d->%d",
								file.Path, oldSize, file.Size, oldUpdated, file.Updated)
						} else {
							logging.LogWarnf("repairLazyDataConsistency: failed to stat local file [%s]: %s", localPath, statErr)
						}
					} else {
						// 本地文件不存在，保持原有的元数据
						logging.LogWarnf("repairLazyDataConsistency: local file [%s] not found, using stored metadata (Size=%d, Updated=%d)",
							file.Path, file.Size, file.Updated)
					}

					missingFiles = append(missingFiles, file)
					*files = append(*files, file)
					repairedCount++
					logging.LogInfof("repairLazyDataConsistency: added file [%s] to repair list (total repaired: %d)", file.Path, repairedCount)
				} else {
					logging.LogInfof("repairLazyDataConsistency: file [%s] is not an assets file, skipping", file.Path)
					skippedCount++
				}
			}
		}
		logging.LogInfof("repairLazyDataConsistency: index check completed: checked=%d, missing=%d, repaired=%d, skipped=%d",
			checkedCount, missingCount, repairedCount, skippedCount)
	} else {
		logging.LogInfof("repairLazyDataConsistency: no lazy files in index, skipping index check")
	}

	// 总是扫描本地assets文件夹，查找清单中缺失的文件
	// 这样可以发现手动添加的文件或清单中遗漏的文件
	logging.LogInfof("repairLazyDataConsistency: starting to scan local assets folder for missing files")
	scannedCount, scanErr := repo.scanLocalAssetsForRepair(files)
	if scanErr != nil {
		logging.LogWarnf("repairLazyDataConsistency: scan local assets failed: %s", scanErr)
		// 不返回错误，继续处理已找到的文件
	} else {
		logging.LogInfof("repairLazyDataConsistency: scan completed, found %d new files", scannedCount)
		repairedCount += scannedCount
	}

	// 如果有修复的文件，更新清单
	if len(missingFiles) > 0 {
		logging.LogInfof("repairLazyDataConsistency: adding %d missing files from index to manifest", len(missingFiles))

		// 添加到清单中
		for i, file := range missingFiles {
			asset := &LazyAsset{
				Path:     normalizeLazyPath(file.Path),
				FileID:   file.ID,
				Size:     file.Size,
				Modified: file.Updated,
				Chunks:   file.Chunks,
			}
			repo.setLazyManifestAsset(manifest, asset)
			logging.LogInfof("repairLazyDataConsistency: added asset [%d/%d] to manifest: %s (ID=%s, Size=%d, Chunks=%d)",
				i+1, len(missingFiles), file.Path, file.ID, file.Size, len(file.Chunks))
		}
	}

	// 保存更新后的清单（scanLocalAssetsForRepair已经保存了，但这里需要保存索引中缺失的文件）
	if len(missingFiles) > 0 || repairedCount > 0 {
		logging.LogInfof("repairLazyDataConsistency: saving updated manifest (total assets: %d)", len(manifest.Assets))
		saveErr := repo.lazyLoader.saveManifest(manifest)
		if saveErr != nil {
			logging.LogErrorf("repairLazyDataConsistency: failed to save manifest: %s", saveErr)
			return fmt.Errorf("save repaired manifest failed: %w", saveErr)
		}
		logging.LogInfof("repairLazyDataConsistency: manifest saved successfully")
		logging.LogInfof("repairLazyDataConsistency: repair completed successfully: total repaired=%d (from index=%d, from scan=%d)",
			repairedCount, len(missingFiles), scannedCount)
	} else {
		logging.LogInfof("repairLazyDataConsistency: no files to repair, manifest unchanged")
	}

	logging.LogInfof("repairLazyDataConsistency: repair process finished, total files in manifest: %d", len(manifest.Assets))
	return nil
}

// scanLocalAssetsForRepair 扫描本地assets文件夹，将找到的文件添加到清单中
func (repo *Repo) scanLocalAssetsForRepair(files *[]*entity.File) (int, error) {
	logging.LogInfof("scanLocalAssetsForRepair: starting scan process")

	manifest, err := repo.lazyLoader.getManifest()
	if err != nil {
		return 0, fmt.Errorf("get manifest failed: %w", err)
	}

	// 构建清单中已有路径的映射
	manifestPaths := make(map[string]bool)
	for _, asset := range manifest.Assets {
		manifestPaths[asset.Path] = true
	}
	logging.LogInfof("scanLocalAssetsForRepair: manifest has %d existing assets", len(manifestPaths))

	assetsDir := filepath.Join(repo.DataPath, "assets")
	if !gulu.File.IsExist(assetsDir) {
		logging.LogInfof("scanLocalAssetsForRepair: assets directory not found: %s", assetsDir)
		return 0, nil
	}
	logging.LogInfof("scanLocalAssetsForRepair: scanning assets directory: %s", assetsDir)

	var scannedFiles []*entity.File
	var repairedCount int
	var existingCount int

	// 扫描assets文件夹
	err = filepath.Walk(assetsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		// 计算相对路径
		relPath, relErr := filepath.Rel(repo.DataPath, path)
		if relErr != nil {
			return relErr
		}

		// 转换为统一格式（使用斜杠）
		relPath = filepath.ToSlash(relPath)

		// 确保路径以assets/开头
		if !strings.HasPrefix(relPath, "assets/") {
			return nil
		}
		if isIgnoredLazyAssetPath(relPath) {
			return nil
		}

		// 检查是否已在清单中
		if manifestPaths[relPath] {
			existingCount++
			if existingCount%100 == 0 {
				logging.LogInfof("scanLocalAssetsForRepair: scanned %d files, %d already in manifest", existingCount+repairedCount, existingCount)
			}
			return nil
		}

		// 使用和索引创建时相同的方式生成文件ID，确保一致性
		file := entity.NewFile(relPath, info.Size(), info.ModTime().UnixMilli())
		logging.LogInfof("scanLocalAssetsForRepair: processing file [%s] (Size=%d, ID=%s)", relPath, file.Size, file.ID)

		// 检查存储中是否已存在这个文件ID（可能之前已经索引过）
		existingFile, getErr := repo.store.GetFile(file.ID)
		if getErr == nil && existingFile != nil {
			// 文件已存在，使用已有的文件信息（包括chunks）
			file = existingFile
			logging.LogInfof("scanLocalAssetsForRepair: file [%s] already exists in store with ID [%s], reusing (Chunks=%d)",
				relPath, file.ID, len(file.Chunks))
		} else {
			// 文件不存在，需要处理chunks
			logging.LogInfof("scanLocalAssetsForRepair: file [%s] not in store, creating new entry", relPath)
			// 如果文件很小，直接读取并计算chunks
			if file.Size < 1024*1024 { // 小于1MB的文件
				logging.LogInfof("scanLocalAssetsForRepair: reading small file [%s] to compute chunks", relPath)
				data, readErr := os.ReadFile(path)
				if readErr == nil {
					fileHash := util.Hash(data)
					file.Chunks = []string{fileHash}
					logging.LogInfof("scanLocalAssetsForRepair: computed chunk hash [%s] for file [%s]", fileHash, relPath)
					// 保存chunk到存储
					chunk := &entity.Chunk{
						ID:   fileHash,
						Data: data,
					}
					if putErr := repo.store.PutChunk(chunk); putErr != nil {
						logging.LogWarnf("scanLocalAssetsForRepair: failed to save chunk for [%s]: %s", relPath, putErr)
					} else {
						logging.LogInfof("scanLocalAssetsForRepair: saved chunk [%s] to store", fileHash)
					}
				} else {
					logging.LogWarnf("scanLocalAssetsForRepair: failed to read file [%s]: %s", relPath, readErr)
				}
			} else {
				logging.LogInfof("scanLocalAssetsForRepair: file [%s] is large (%d bytes), skipping chunk computation", relPath, file.Size)
			}

			// 保存文件元数据到存储
			if putErr := repo.store.PutFile(file); putErr != nil {
				logging.LogWarnf("scanLocalAssetsForRepair: failed to save file metadata for [%s]: %s", relPath, putErr)
			} else {
				logging.LogInfof("scanLocalAssetsForRepair: saved file metadata [%s] to store", relPath)
			}
		}

		// 添加到清单
		asset := &LazyAsset{
			Path:     normalizeLazyPath(file.Path),
			FileID:   file.ID,
			Size:     file.Size,
			Modified: file.Updated,
			Chunks:   file.Chunks,
			Status:   LazyStatusCached, // 本地文件存在，标记为已缓存
		}
		repo.setLazyManifestAsset(manifest, asset)

		scannedFiles = append(scannedFiles, file)
		*files = append(*files, file)
		repairedCount++

		if repairedCount%10 == 0 {
			logging.LogInfof("scanLocalAssetsForRepair: progress: repaired=%d, existing=%d",
				repairedCount, existingCount)
		}

		logging.LogInfof("scanLocalAssetsForRepair: added file [%s] to manifest (ID=%s, Size=%d, Chunks=%d)",
			relPath, file.ID, file.Size, len(file.Chunks))
		return nil
	})

	if err != nil {
		logging.LogErrorf("scanLocalAssetsForRepair: walk assets directory failed: %s", err)
		return repairedCount, fmt.Errorf("walk assets directory failed: %w", err)
	}

	logging.LogInfof("scanLocalAssetsForRepair: scan completed: total scanned=%d, repaired=%d, existing=%d",
		repairedCount+existingCount, repairedCount, existingCount)

	// 保存更新后的清单
	if repairedCount > 0 {
		logging.LogInfof("scanLocalAssetsForRepair: saving manifest with %d total assets", len(manifest.Assets))
		saveErr := repo.lazyLoader.saveManifest(manifest)
		if saveErr != nil {
			logging.LogErrorf("scanLocalAssetsForRepair: failed to save manifest: %s", saveErr)
			return repairedCount, fmt.Errorf("save manifest failed: %w", saveErr)
		}
		logging.LogInfof("scanLocalAssetsForRepair: successfully saved manifest with %d new files added", repairedCount)
	} else {
		logging.LogInfof("scanLocalAssetsForRepair: no new files found, manifest unchanged")
	}

	logging.LogInfof("scanLocalAssetsForRepair: scan process finished, returning %d repaired files", repairedCount)
	return repairedCount, nil
}

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
	"strings"
	"sync"
	"time"

	"github.com/88250/gulu"
	"github.com/siyuan-note/dejavu/entity"
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
	Path     string     `json:"path"`
	FileID   string     `json:"fileId"`
	Size     int64      `json:"size"`
	Hash     string     `json:"hash"`
	Modified int64      `json:"mtime"`
	Chunks   []string   `json:"chunks"`
	Status   LazyStatus `json:"status"`
}

// LazyManifest 懒加载清单
type LazyManifest struct {
	Version string                `json:"version"`
	Assets  map[string]*LazyAsset `json:"assets"`
	Updated int64                 `json:"updated"`
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
	logging.LogInfof("LazyLoader.LoadAsset: starting load for path [%s]", path)

	ll.mutex.Lock()
	defer ll.mutex.Unlock()

	// 检查是否已经在下载中
	if ch, exists := ll.downloading[path]; exists {
		logging.LogInfof("LazyLoader.LoadAsset: asset [%s] is already downloading, waiting...", path)
		ll.mutex.Unlock()
		err := <-ch
		ll.mutex.Lock()
		if err != nil {
			logging.LogErrorf("LazyLoader.LoadAsset: concurrent download failed for [%s]: %s", path, err.Error())
		} else {
			logging.LogInfof("LazyLoader.LoadAsset: concurrent download succeeded for [%s]", path)
		}
		return err
	}

	// 检查本地是否存在
	localPath := filepath.Join(ll.repo.DataPath, path)
	logging.LogInfof("LazyLoader.LoadAsset: checking local path [%s]", localPath)

	if gulu.File.IsExist(localPath) {
		logging.LogInfof("LazyLoader.LoadAsset: asset [%s] already exists locally", path)
		if asset := ll.cache[path]; asset != nil {
			asset.Status = LazyStatusCached
			logging.LogInfof("LazyLoader.LoadAsset: updated cache status for [%s] to cached", path)
		}
		return nil
	}

	// 获取资源信息
	logging.LogInfof("LazyLoader.LoadAsset: getting manifest for [%s]", path)
	manifest, err := ll.getManifest()
	if err != nil {
		logging.LogErrorf("LazyLoader.LoadAsset: get manifest failed: %s", err.Error())
		return fmt.Errorf("get manifest failed: %w", err)
	}

	logging.LogInfof("LazyLoader.LoadAsset: manifest has %d assets", len(manifest.Assets))

	// 尝试查找资源，支持两种路径格式
	asset, exists := manifest.Assets[path]
	if !exists && !strings.HasPrefix(path, "/") {
		// 如果没找到且路径不以/开头，尝试加上/查找
		altPath := "/" + path
		logging.LogInfof("LazyLoader.LoadAsset: trying alternative path [%s]", altPath)
		asset, exists = manifest.Assets[altPath]
	}
	if !exists && strings.HasPrefix(path, "/") {
		// 如果没找到且路径以/开头，尝试去掉/查找
		altPath := strings.TrimPrefix(path, "/")
		logging.LogInfof("LazyLoader.LoadAsset: trying alternative path [%s]", altPath)
		asset, exists = manifest.Assets[altPath]
	}

	if !exists {
		logging.LogErrorf("LazyLoader.LoadAsset: asset [%s] not found in manifest", path)
		// 列出manifest中所有assets用于调试
		for assetPath := range manifest.Assets {
			logging.LogInfof("LazyLoader.LoadAsset: manifest contains asset [%s]", assetPath)
		}
		return fmt.Errorf("asset not found in manifest: %s", path)
	}

	logging.LogInfof("LazyLoader.LoadAsset: found asset [%s] in manifest with %d chunks", path, len(asset.Chunks))

	// 创建下载通道
	ch := make(chan error, 1)
	ll.downloading[path] = ch
	asset.Status = LazyStatusDownloading
	logging.LogInfof("LazyLoader.LoadAsset: starting async download for [%s]", path)

	// 异步下载
	go func() {
		defer func() {
			ll.mutex.Lock()
			delete(ll.downloading, path)
			ll.mutex.Unlock()
		}()

		logging.LogInfof("LazyLoader.LoadAsset: goroutine starting download for [%s]", path)
		err := ll.downloadAsset(asset)
		if err != nil {
			asset.Status = LazyStatusError
			logging.LogErrorf("LazyLoader.LoadAsset: download asset [%s] failed: %s", path, err)
		} else {
			asset.Status = LazyStatusCached
			logging.LogInfof("downloaded asset [%s] successfully", path)
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
	logging.LogInfof("downloadAsset: starting download for [%s] with %d chunks", asset.Path, len(asset.Chunks))

	// 创建目标目录（确保路径格式正确）
	cleanPath := strings.TrimPrefix(asset.Path, "/")
	localPath := filepath.Join(ll.repo.DataPath, cleanPath)
	logging.LogInfof("downloadAsset: target local path [%s]", localPath)

	dir := filepath.Dir(localPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		logging.LogErrorf("downloadAsset: create dir [%s] failed: %s", dir, err)
		return fmt.Errorf("create dir failed: %w", err)
	}

	// 下载所有chunks
	var data []byte
	for i, chunkID := range asset.Chunks {
		logging.LogInfof("downloadAsset: processing chunk %d/%d [%s]", i+1, len(asset.Chunks), chunkID)

		chunk, err := ll.repo.store.GetChunk(chunkID)
		if err != nil {
			// 如果本地没有，从云端下载
			logging.LogInfof("downloadAsset: chunk [%s] not found locally, downloading from cloud", chunkID)
			
			chunkPath := fmt.Sprintf("objects/%s/%s", chunkID[:2], chunkID[2:])
			cloudData, downloadErr := ll.repo.cloud.DownloadObject(chunkPath)
			if downloadErr != nil {
				logging.LogErrorf("downloadAsset: download chunk [%s] from cloud failed: %s", chunkID, downloadErr)
				return fmt.Errorf("download chunk [%s] failed: %w", chunkID, downloadErr)
			}

			cloudChunk := &entity.Chunk{
				ID:   chunkID,
				Data: cloudData,
			}
			
			logging.LogInfof("downloadAsset: chunk [%s] downloaded successfully, size: %d bytes", chunkID, len(cloudChunk.Data))

			// 存储到本地
			if putErr := ll.repo.store.PutChunk(cloudChunk); putErr != nil {
				logging.LogErrorf("downloadAsset: store chunk [%s] locally failed: %s", chunkID, putErr)
				return fmt.Errorf("put chunk [%s] failed: %w", chunkID, putErr)
			}

			logging.LogInfof("downloadAsset: chunk [%s] stored locally", chunkID)
			chunk = cloudChunk
		} else {
			logging.LogInfof("downloadAsset: chunk [%s] found locally, size: %d bytes", chunkID, len(chunk.Data))
		}
		data = append(data, chunk.Data...)
	}

	// 写入文件
	logging.LogInfof("downloadAsset: writing file [%s], total size: %d bytes", localPath, len(data))
	if err := gulu.File.WriteFileSafer(localPath, data, 0644); err != nil {
		logging.LogErrorf("downloadAsset: write file [%s] failed: %s", localPath, err)
		return fmt.Errorf("write file failed: %w", err)
	}

	logging.LogInfof("downloadAsset: successfully downloaded and saved [%s]", asset.Path)

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

	for path, asset := range manifest.Assets {
		localPath := filepath.Join(ll.repo.DataPath, path)
		if gulu.File.IsExist(localPath) {
			if err := os.Remove(localPath); err != nil {
				logging.LogWarnf("remove cached file [%s] failed: %s", localPath, err)
			} else {
				asset.Status = LazyStatusPending
			}
		}
	}

	return ll.saveManifest(manifest)
}

// getManifest 获取懒加载清单
func (ll *LazyLoader) getManifest() (*LazyManifest, error) {
	if ll.manifest != nil {
		return ll.manifest, nil
	}

	manifestPath := ll.getManifestPath()
	if !gulu.File.IsExist(manifestPath) {
		ll.manifest = &LazyManifest{
			Version: "1.0",
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
	ll.manifest = manifest

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest failed: %w", err)
	}

	manifestPath := ll.getManifestPath()
	dir := filepath.Dir(manifestPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create manifest dir failed: %w", err)
	}

	return gulu.File.WriteFileSafer(manifestPath, data, 0644)
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

	logging.LogInfof("updateLazyManifest: updating manifest with %d lazy files", len(lazyFiles))
	
	// 记录冲突处理统计
	var conflictCount, mergedCount, newCount int

	// 更新资源信息
	for _, file := range lazyFiles {
		logging.LogInfof("updateLazyManifest: processing file [%s] with %d chunks, size %d bytes", file.Path, len(file.Chunks), file.Size)

		// 检查chunks是否有效 - 现在这应该很少发生，因为chunks在索引阶段就被正确计算了
		if len(file.Chunks) == 0 && file.Size > 0 {
			logging.LogWarnf("updateLazyManifest: file [%s] has no chunks but size is %d bytes!", file.Path, file.Size)
			logging.LogWarnf("updateLazyManifest: this indicates an invalid file object, likely from failed putFileChunks or incomplete sync")
			logging.LogInfof("updateLazyManifest: skipping invalid file object [%s] from lazy manifest", file.Path)
			continue
		}

		// 尝试两种路径格式查找现有资源
		asset := manifest.Assets[file.Path]
		if asset == nil && !strings.HasPrefix(file.Path, "/") {
			// 尝试加前导斜杠查找
			altPath := "/" + file.Path
			asset = manifest.Assets[altPath]
			logging.LogInfof("updateLazyManifest: trying alternative path [%s] for [%s]", altPath, file.Path)
		}
		if asset == nil && strings.HasPrefix(file.Path, "/") {
			// 尝试去掉前导斜杠查找
			altPath := strings.TrimPrefix(file.Path, "/")
			asset = manifest.Assets[altPath]
			logging.LogInfof("updateLazyManifest: trying alternative path [%s] for [%s]", altPath, file.Path)
		}

		if asset == nil {
			logging.LogInfof("updateLazyManifest: creating new asset for [%s]", file.Path)
			asset = &LazyAsset{}
			manifest.Assets[file.Path] = asset
			newCount++
		} else {
			logging.LogInfof("updateLazyManifest: found existing asset for [%s], old chunks: %d, new chunks: %d", file.Path, len(asset.Chunks), len(file.Chunks))
			
			// 检测并处理冲突
			if repo.hasLazyFileConflict(asset, file) {
				conflictCount++
				logging.LogWarnf("updateLazyManifest: detected conflict for [%s], resolving...", file.Path)
				
				// 冲突解决策略：优先使用更新的版本
				if file.Updated > asset.Modified {
					logging.LogInfof("updateLazyManifest: using newer file version for [%s] (file: %d > asset: %d)", file.Path, file.Updated, asset.Modified)
					mergedCount++
				} else if file.Updated < asset.Modified {
					logging.LogInfof("updateLazyManifest: keeping existing asset version for [%s] (asset: %d > file: %d)", file.Path, asset.Modified, file.Updated)
					// 保留现有asset，不更新
					continue
				} else {
					// 时间相同，比较大小和chunks
					if file.Size != asset.Size || len(file.Chunks) != len(asset.Chunks) {
						logging.LogInfof("updateLazyManifest: same timestamp but different content for [%s], using file version", file.Path)
						mergedCount++
					} else {
						logging.LogInfof("updateLazyManifest: identical file [%s], no update needed", file.Path)
						continue
					}
				}
			} else {
				logging.LogInfof("updateLazyManifest: no conflict for [%s], updating normally", file.Path)
			}
		}

		asset.Path = file.Path
		asset.FileID = file.ID
		asset.Size = file.Size
		asset.Modified = file.Updated
		asset.Chunks = file.Chunks
		
		// 确保chunks已上传到云端
		if len(file.Chunks) > 0 {
			logging.LogInfof("updateLazyManifest: uploading %d chunks for [%s] to cloud", len(file.Chunks), file.Path)
			if uploadErr := repo.uploadLazyFileChunks(file); uploadErr != nil {
				logging.LogErrorf("updateLazyManifest: failed to upload chunks for [%s]: %s", file.Path, uploadErr)
				// 不中断流程，只记录错误
			} else {
				logging.LogInfof("updateLazyManifest: successfully uploaded chunks for [%s]", file.Path)
			}
		}

		// 检查本地是否存在，更新状态
		// 注意：这里需要去掉前导斜杠来构建本地路径
		cleanPath := strings.TrimPrefix(file.Path, "/")
		localPath := filepath.Join(repo.DataPath, cleanPath)
		logging.LogInfof("updateLazyManifest: checking local path [%s] for file [%s]", localPath, file.Path)

		if gulu.File.IsExist(localPath) {
			logging.LogInfof("updateLazyManifest: file [%s] exists locally, status = cached", file.Path)
			asset.Status = LazyStatusCached
		} else {
			asset.Status = LazyStatusPending
		}
	}

	// 记录冲突处理结果
	logging.LogInfof("updateLazyManifest: manifest update complete - new: %d, conflicts: %d, merged: %d", newCount, conflictCount, mergedCount)

	return repo.lazyLoader.saveManifest(manifest)
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

// uploadLazyFileChunks 上传懒加载文件的chunks到云端
func (repo *Repo) uploadLazyFileChunks(file *entity.File) error {
	if repo.cloud == nil {
		return fmt.Errorf("cloud storage not configured")
	}
	
	logging.LogInfof("uploadLazyFileChunks: uploading %d chunks for [%s]", len(file.Chunks), file.Path)
	
	for i, chunkID := range file.Chunks {
		logging.LogInfof("uploadLazyFileChunks: processing chunk %d/%d [%s]", i+1, len(file.Chunks), chunkID)
		
		// 构建chunk的云端路径
		chunkPath := fmt.Sprintf("objects/%s/%s", chunkID[:2], chunkID[2:])
		
		// UploadObject 方法会自动从仓库路径读取文件，所以chunk应该已经在正确位置
		// 上传到云端
		if _, uploadErr := repo.cloud.UploadObject(chunkPath, false); uploadErr != nil {
			logging.LogErrorf("uploadLazyFileChunks: failed to upload chunk [%s]: %s", chunkID, uploadErr)
			return fmt.Errorf("upload chunk %s failed: %w", chunkID, uploadErr)
		}
		
		logging.LogInfof("uploadLazyFileChunks: successfully uploaded chunk [%s]", chunkID)
	}
	
	logging.LogInfof("uploadLazyFileChunks: all chunks uploaded for [%s]", file.Path)
	return nil
}

// getLazyFilesForIndex 获取懒加载文件的索引条目
func (repo *Repo) getLazyFilesForIndex() ([]*entity.File, error) {
	if !repo.lazyLoadEnabled || repo.lazyLoader == nil {
		return nil, nil
	}

	manifest, err := repo.lazyLoader.getManifest()
	if err != nil {
		return nil, fmt.Errorf("get manifest failed: %w", err)
	}

	var files []*entity.File
	for _, asset := range manifest.Assets {
		// 检查本地文件是否存在
		cleanPath := strings.TrimPrefix(asset.Path, "/")
		localPath := filepath.Join(repo.DataPath, cleanPath)
		
		if gulu.File.IsExist(localPath) {
			// 本地文件存在，使用实际文件信息
			logging.LogInfof("getLazyFilesForIndex: local lazy file exists [%s]", asset.Path)
			info, statErr := os.Stat(localPath)
			if statErr == nil {
				// 使用本地文件的实际信息
				file := &entity.File{
					ID:      asset.FileID,
					Path:    asset.Path,
					Size:    info.Size(),
					Updated: info.ModTime().UnixMilli(),
					Chunks:  asset.Chunks, // 保留现有的chunks信息
				}
				files = append(files, file)
			}
		} else {
			// 本地文件不存在，使用清单中的元数据创建虚拟条目
			logging.LogInfof("getLazyFilesForIndex: local lazy file missing [%s], using manifest metadata", asset.Path)
			
			// 只有当chunks存在时才添加虚拟条目
			if len(asset.Chunks) > 0 {
				file := &entity.File{
					ID:      asset.FileID,
					Path:    asset.Path,
					Size:    asset.Size,
					Updated: asset.Modified,
					Chunks:  asset.Chunks,
				}
				files = append(files, file)
				logging.LogInfof("getLazyFilesForIndex: added virtual lazy file [%s] with %d chunks", asset.Path, len(asset.Chunks))
			} else {
				logging.LogWarnf("getLazyFilesForIndex: skipping lazy file [%s] - no chunks available", asset.Path)
			}
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

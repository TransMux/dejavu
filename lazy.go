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
	var data []byte
	for _, chunkID := range asset.Chunks {

		chunk, err := ll.repo.store.GetChunk(chunkID)
		if err != nil {
			// 如果本地没有，从云端下载
			chunkPath := fmt.Sprintf("objects/%s/%s", chunkID[:2], chunkID[2:])
			cloudData, downloadErr := ll.repo.cloud.DownloadObject(chunkPath)
			if downloadErr != nil {
				return fmt.Errorf("download chunk [%s] failed: %w", chunkID, downloadErr)
			}

			// 解码云端数据（解压缩和解密）
			decodedData, decodeErr := ll.repo.store.DecodeData(cloudData)
			if decodeErr != nil {
				return fmt.Errorf("decode chunk [%s] failed: %w", chunkID, decodeErr)
			}

			cloudChunk := &entity.Chunk{
				ID:   chunkID,
				Data: decodedData,
			}

			// 存储解码后的chunk到本地
			if putErr := ll.repo.store.PutChunk(cloudChunk); putErr != nil {
				return fmt.Errorf("put chunk [%s] failed: %w", chunkID, putErr)
			}

			chunk = cloudChunk
		}
		data = append(data, chunk.Data...)
	}

	// 写入文件
	if err := gulu.File.WriteFileSafer(localPath, data, 0644); err != nil {
		return fmt.Errorf("write file failed: %w", err)
	}

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

	
	// 记录冲突处理统计
	var conflictCount, mergedCount, newCount int

	// 更新资源信息
	for _, file := range lazyFiles {
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
			manifest.Assets[file.Path] = asset
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

		asset.Path = file.Path
		asset.FileID = file.ID
		asset.Size = file.Size
		asset.Modified = file.Updated
		asset.Chunks = file.Chunks
		
		// 确保chunks已上传到云端
		if len(file.Chunks) > 0 {
			if uploadErr := repo.uploadLazyFileChunks(file); uploadErr != nil {
				logging.LogErrorf("updateLazyManifest: failed to upload chunks for [%s]: %s", file.Path, uploadErr)
			}
		}

		// 检查本地是否存在，更新状态
		cleanPath := strings.TrimPrefix(file.Path, "/")
		localPath := filepath.Join(repo.DataPath, cleanPath)

		if gulu.File.IsExist(localPath) {
			asset.Status = LazyStatusCached
		} else {
			asset.Status = LazyStatusPending
		}
	}

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
	
	for _, chunkID := range file.Chunks {
		// 构建chunk的云端路径
		chunkPath := fmt.Sprintf("objects/%s/%s", chunkID[:2], chunkID[2:])
		
		// 上传到云端
		if _, uploadErr := repo.cloud.UploadObject(chunkPath, false); uploadErr != nil {
			logging.LogErrorf("uploadLazyFileChunks: failed to upload chunk [%s]: %s", chunkID, uploadErr)
			return fmt.Errorf("upload chunk %s failed: %w", chunkID, uploadErr)
		}
	}
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
			info, statErr := os.Stat(localPath)
			if statErr == nil {
				file := &entity.File{
					ID:      asset.FileID,
					Path:    asset.Path,
					Size:    info.Size(),
					Updated: info.ModTime().UnixMilli(),
					Chunks:  asset.Chunks,
				}
				files = append(files, file)
			}
		} else {
			// 本地文件不存在，使用清单中的元数据创建虚拟条目
			if len(asset.Chunks) > 0 {
				file := &entity.File{
					ID:      asset.FileID,
					Path:    asset.Path,
					Size:    asset.Size,
					Updated: asset.Modified,
					Chunks:  asset.Chunks,
				}
				files = append(files, file)
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

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
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/88250/gulu"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/logging"
)

// LazyIndexManager 管理懒加载文件的索引
// 核心思想：将懒加载文件索引与普通文件索引分离，避免在索引构建时的复杂补丁操作
type LazyIndexManager struct {
	repoPath    string                  // 仓库路径
	patterns    []string                // 懒加载模式
	lazyFiles   map[string]*entity.File // 懒加载文件映射 path -> file
	mutex       sync.RWMutex            // 读写锁
	lastCloudID string                  // 最后同步的云端索引ID
}

// NewLazyIndexManager 创建懒加载索引管理器
func NewLazyIndexManager(repoPath string, patterns []string) *LazyIndexManager {
	manager := &LazyIndexManager{
		repoPath:  repoPath,
		patterns:  patterns,
		lazyFiles: make(map[string]*entity.File),
	}

	// 加载现有的懒加载索引
	if err := manager.load(); err != nil {
		logging.LogWarnf("failed to load lazy index: %s", err)
	}

	return manager
}

// GetLazyFiles 获取所有懒加载文件
func (m *LazyIndexManager) GetLazyFiles() []*entity.File {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	var files []*entity.File
	for _, file := range m.lazyFiles {
		files = append(files, file)
	}
	return files
}

// UpdateFromCloudIndex 从云端索引更新懒加载文件信息
func (m *LazyIndexManager) UpdateFromCloudIndex(cloudIndex *entity.Index, cloudFiles []*entity.File) error {
	if cloudIndex.ID == m.lastCloudID {
		// 云端索引没有变化，无需更新
		return nil
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	// 记录变化
	added := 0
	updated := 0

	// 处理云端索引中的懒加载文件：新增或更新
	for _, file := range cloudFiles {
		if m.isLazyLoadingFile(file.Path) {
			if oldFile, exists := m.lazyFiles[file.Path]; exists {
				if oldFile.Updated != file.Updated {
					updated++
					m.lazyFiles[file.Path] = file
				}
			} else {
				added++
				m.lazyFiles[file.Path] = file
			}
		}
	}

	// 重要修复：不删除不在当前云端索引中的文件记录
	// 这些文件可能来自历史快照，仍可能需要懒加载
	// 只记录但不删除，以支持从历史快照懒加载文件

	m.lastCloudID = cloudIndex.ID

	// 保存到磁盘
	if err := m.save(); err != nil {
		return err
	}

	logging.LogInfof("[Lazy Index] updated from cloud: +%d ~%d files (preserved historical files for lazy loading)", added, updated)
	return nil
}

// AddLazyFile 添加懒加载文件到索引
func (m *LazyIndexManager) AddLazyFile(file *entity.File) {
	if !m.isLazyLoadingFile(file.Path) {
		return
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.lazyFiles[file.Path] = file
	m.save() // 异步保存，忽略错误

	logging.LogInfof("[Lazy Index] added file: %s", file.Path)
}

// RemoveLazyFile 从索引中移除懒加载文件
func (m *LazyIndexManager) RemoveLazyFile(path string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if _, exists := m.lazyFiles[path]; exists {
		delete(m.lazyFiles, path)
		m.save() // 异步保存，忽略错误
		logging.LogInfof("[Lazy Index] removed file: %s", path)
	}
}

// MergeWithLocalFiles 将懒加载文件与本地文件合并，返回完整的文件列表
func (m *LazyIndexManager) MergeWithLocalFiles(localFiles []*entity.File) []*entity.File {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	// 创建本地文件路径映射
	localFileMap := make(map[string]*entity.File)
	for _, file := range localFiles {
		localFileMap[file.Path] = file
	}

	// 合并文件列表
	var mergedFiles []*entity.File
	mergedFiles = append(mergedFiles, localFiles...) // 首先添加所有本地文件

	// 添加不在本地的懒加载文件
	addedLazy := 0
	for path, lazyFile := range m.lazyFiles {
		if _, existsLocally := localFileMap[path]; !existsLocally {
			mergedFiles = append(mergedFiles, lazyFile)
			addedLazy++
		}
	}

	if addedLazy > 0 {
		logging.LogInfof("[Lazy Index] merged %d lazy files with %d local files", addedLazy, len(localFiles))
	}

	return mergedFiles
}

// isLazyLoadingFile 检查文件是否为懒加载文件
func (m *LazyIndexManager) isLazyLoadingFile(filePath string) bool {
	if len(m.patterns) == 0 {
		return false
	}

	// 使用与repo.go相同的逻辑
	for _, pattern := range m.patterns {
		// 简化的匹配逻辑
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if strings.HasPrefix(filePath, "/"+prefix) || strings.Contains(filePath, "/"+prefix) {
				return true
			}
		} else if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(filePath, suffix) {
				return true
			}
		} else if filePath == "/"+pattern || strings.HasSuffix(filePath, "/"+pattern) {
			return true
		}
	}

	return false
}

// save 保存懒加载索引到磁盘
func (m *LazyIndexManager) save() error {
	data := struct {
		LastCloudID string                  `json:"lastCloudID"`
		LazyFiles   map[string]*entity.File `json:"lazyFiles"`
	}{
		LastCloudID: m.lastCloudID,
		LazyFiles:   m.lazyFiles,
	}

	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}

	lazyIndexPath := filepath.Join(m.repoPath, "lazy-index.json")
	return gulu.File.WriteFileSafer(lazyIndexPath, bytes, 0644)
}

// load 从磁盘加载懒加载索引
func (m *LazyIndexManager) load() error {
	lazyIndexPath := filepath.Join(m.repoPath, "lazy-index.json")

	if !gulu.File.IsExist(lazyIndexPath) {
		return nil // 文件不存在是正常的
	}

	bytes, err := os.ReadFile(lazyIndexPath)
	if err != nil {
		return err
	}

	var data struct {
		LastCloudID string                  `json:"lastCloudID"`
		LazyFiles   map[string]*entity.File `json:"lazyFiles"`
	}

	if err := json.Unmarshal(bytes, &data); err != nil {
		return err
	}

	m.lastCloudID = data.LastCloudID
	if data.LazyFiles != nil {
		m.lazyFiles = data.LazyFiles
	}

	logging.LogInfof("[Lazy Index] loaded %d lazy files (last cloud ID: %s)", len(m.lazyFiles), m.lastCloudID)
	return nil
}

// GetStats 获取懒加载索引统计信息
func (m *LazyIndexManager) GetStats() (count int, size int64) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	count = len(m.lazyFiles)
	for _, file := range m.lazyFiles {
		size += file.Size
	}
	return
}

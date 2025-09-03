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
	"github.com/sabhiram/go-gitignore"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/logging"
)

// LazyIndexManager 管理懒加载文件的索引
// 核心思想：将懒加载文件索引与普通文件索引分离，避免在索引构建时的复杂补丁操作
type LazyIndexManager struct {
	repoPath    string                  // 仓库路径
	dataPath    string                  // 数据文件夹路径
	patterns    []string                // 懒加载模式
	matcher     *ignore.GitIgnore       // 懒加载匹配器
	lazyFiles   map[string]*entity.File // 懒加载文件映射 path -> file
	mutex       sync.RWMutex            // 读写锁
	lastCloudID string                  // 最后同步的云端索引ID
}

// NewLazyIndexManager 创建懒加载索引管理器
func NewLazyIndexManager(repoPath, dataPath string, patterns []string) *LazyIndexManager {
	// 创建匹配器，使用与repo相同的逻辑
	var matcher *ignore.GitIgnore
	if len(patterns) == 0 {
		matcher = ignore.CompileIgnoreLines() // 返回空匹配器
	} else {
		// 统一移除前导 '/'，以消除路径格式差异
		var normalized []string
		for _, p := range patterns {
			if strings.HasPrefix(p, "/") {
				normalized = append(normalized, p[1:])
			} else {
				normalized = append(normalized, p)
			}
		}
		matcher = ignore.CompileIgnoreLines(normalized...)
	}

	manager := &LazyIndexManager{
		repoPath:  repoPath,
		dataPath:  dataPath,
		patterns:  patterns,
		matcher:   matcher,
		lazyFiles: make(map[string]*entity.File),
	}

	// 加载现有的懒加载索引
	if err := manager.load(); err != nil {
		logging.LogWarnf("failed to load lazy index: %s", err)
	}

	logging.LogInfof("[Lazy Index] initialized with %d files, patterns: %v", len(manager.lazyFiles), patterns)
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

	m.lastCloudID = cloudIndex.ID

	// 保存到磁盘
	if err := m.save(); err != nil {
		return err
	}

	logging.LogInfof("[Lazy Index] updated from cloud: +%d ~%d files", added, updated)
	return nil
}

// AddLazyFilesFromIndex 从索引中添加懒加载文件（不删除现有记录）
func (m *LazyIndexManager) AddLazyFilesFromIndex(files []*entity.File) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	added := 0
	updated := 0

	for _, file := range files {
		if m.isLazyLoadingFile(file.Path) {
			if existingFile, exists := m.lazyFiles[file.Path]; exists {
				// 只更新更新时间更新的文件
				if file.Updated > existingFile.Updated {
					m.lazyFiles[file.Path] = file
					updated++
				}
			} else {
				m.lazyFiles[file.Path] = file
				added++
			}
		}
	}

	if added > 0 || updated > 0 {
		m.save() // 保存更改
		logging.LogInfof("[Lazy Index] added %d new files, updated %d files from index", added, updated)
	}
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

	// 添加不在本地的懒加载文件，但只有在文件系统中实际存在时才添加
	addedLazy := 0
	skippedLazy := 0
	for path, lazyFile := range m.lazyFiles {
		if _, existsLocally := localFileMap[path]; !existsLocally {
			// 检查文件在本地文件系统中是否实际存在
			// 这防止了已删除的懒加载文件被重新加入索引
			if fsPath := filepath.Join(m.dataPath, path); gulu.File.IsExist(fsPath) {
				mergedFiles = append(mergedFiles, lazyFile)
				addedLazy++
			} else {
				// 文件已被删除，不应该加入索引，但保留在LazyIndexManager中以支持历史快照的懒加载
				skippedLazy++
				logging.LogInfof("[Lazy Index] skip deleted lazy file [%s] from index merge", path)
			}
		}
	}

	if addedLazy > 0 {
		logging.LogInfof("[Lazy Index] merged %d lazy files with %d local files", addedLazy, len(localFiles))
	}
	if skippedLazy > 0 {
		logging.LogInfof("[Lazy Index] skipped %d deleted lazy files from index merge", skippedLazy)
	}

	return mergedFiles
}

// isLazyLoadingFile 检查文件是否为懒加载文件，使用与repo.go完全相同的逻辑
func (m *LazyIndexManager) isLazyLoadingFile(filePath string) bool {
	if len(m.patterns) == 0 {
		return false
	}
	// 去除被检测路径的前导 '/'
	normalized := filePath
	if strings.HasPrefix(normalized, "/") {
		normalized = normalized[1:]
	}
	return m.matcher.MatchesPath(normalized)
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

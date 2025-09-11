# DejaVu 懒加载系统改造文档

## 改造背景

原有的懒加载系统存在以下关键问题：

1. **上传流程不完整**：懒加载文件的chunks没有被正确上传到云端
2. **下载流程混乱**：同步时处理逻辑复杂，容易出错  
3. **日志不足**：调试困难
4. **状态管理不一致**：本地状态与云端状态不同步

## 改造目标

确保以下三个核心流程正常工作：

1. **上传流程**：只处理本地变化或更新的文件，重新制作chunks，更新清单，上传
2. **下载流程**：更新清单即可，如果出现冲突，那么update一下
3. **懒加载流程**：从云端下载对应chunks

## 改造方案

### 1. 新增核心文件

#### `lazy_improved.go` - 改进的懒加载管理器
- `ImprovedLazyLoader`: 新的懒加载管理器
- `ProcessLocalChanges()`: 处理本地变化文件（上传流程）
- `ProcessCloudChanges()`: 处理云端变化文件（下载流程）
- `LoadAssetOnDemand()`: 按需加载资源（懒加载流程）

#### `repo_lazy_helpers.go` - 仓库级别的懒加载辅助方法
- `formatChunkPath()`: 格式化chunk路径
- `uploadChunkToCloud()`: 上传chunk到云端
- `downloadChunkFromCloud()`: 从云端下载chunk
- `ProcessLazyFilesForIndex()`: 为索引处理懒加载文件
- `ProcessLazyFilesForSync()`: 为同步处理懒加载文件

### 2. 核心流程重构

#### 上传流程 (`ProcessLocalChanges`)
```go
1. 遍历本地文件，识别懒加载文件
2. 检查文件是否需要重新处理（shouldReprocessFile）
3. 重新计算chunks（putFileChunks）
4. 上传chunks到云端（uploadFileChunks）
5. 更新懒加载清单（updateManifest）
```

#### 下载流程 (`ProcessCloudChanges`)
```go  
1. 获取云端懒加载文件信息
2. 检查是否有冲突（hasConflict）
3. 更新本地清单，不下载实际文件
4. 冲突时使用云端版本
```

#### 懒加载流程 (`LoadAssetOnDemand`)
```go
1. 检查本地是否已存在
2. 从清单中查找资源信息
3. 并发控制，防止重复下载
4. 异步下载所有chunks
5. 组装文件并保存到本地
```

### 3. 关键改进点

#### A. 完整的Chunks管理
- **上传时**: 确保所有懒加载文件的chunks都上传到云端
- **下载时**: 只更新清单，不预下载chunks
- **使用时**: 按需从云端下载chunks

#### B. 冲突处理机制
```go
func (ll *ImprovedLazyLoader) hasConflict(cloudFile *entity.File, manifest *LazyManifest) bool {
    // 检查本地文件vs云端文件的修改时间
    // 如果本地更新且云端也更新，则判定为冲突
    // 冲突时优先使用云端版本
}
```

#### C. 状态一致性保证  
- `LazyStatus`: 统一的状态管理（Pending/Downloading/Cached/Error）
- 本地清单与云端状态同步
- 实时更新文件状态

#### D. 详细的日志记录
所有关键操作都添加了详细日志：
- 文件处理过程
- Chunks上传/下载状态
- 冲突检测结果
- 错误详情

### 4. 集成点修改

#### `repo.go` 修改
- 使用`ImprovedLazyLoader`替代原有`LazyLoader`
- 更新索引流程使用`ProcessLazyFilesForIndex`
- 更新接口方法使用改进版本

#### `sync.go` 修改  
- 同步流程使用`ProcessLazyFilesForSync`
- 只下载普通文件的chunks

#### `store.go` 增强
- 添加`GetChunkPath()`方法支持chunk路径获取

## 使用方式

### 1. 创建支持懒加载的仓库
```go
repo, err := NewRepoWithLazyLoad(dataPath, repoPath, historyPath, tempPath, 
    deviceID, deviceName, deviceOS, aesKey, ignoreLines, cloud, true)
```

### 2. 按需加载资源
```go
err := repo.LoadAssetOnDemand("assets/image.png")
```

### 3. 检查缓存状态
```go
cached := repo.IsAssetCached("assets/image.png")
```

### 4. 清理缓存
```go
err := repo.ClearLazyCache()
```

## 数据流向

### 上传时（Index阶段）
```
本地assets文件 → 检查变化 → 计算chunks → 上传chunks到云端 → 更新清单 → 存储到Index.LazyFiles
```

### 下载时（Sync阶段）  
```
云端LazyFiles → 获取元数据 → 检查冲突 → 更新本地清单 → 跳过chunks下载
```

### 使用时（LoadAssetOnDemand）
```
请求资源 → 查找清单 → 下载chunks → 组装文件 → 保存本地 → 更新状态
```

## 关键改进总结

1. **完整性**: 确保所有懒加载文件的chunks都正确上传和管理
2. **一致性**: 本地清单与云端状态保持同步  
3. **效率性**: 只在需要时下载chunks，避免不必要的带宽消耗
4. **健壮性**: 完善的错误处理和冲突解决机制
5. **可观测性**: 详细的日志记录便于调试和监控

## 向后兼容性

- 保持原有API接口不变
- 支持现有的清单文件格式
- 兼容不同的路径格式（`assets/` 和 `/assets/`）

这个改造确保了懒加载系统的每个环节都能正常工作，提供了一个完整、可靠的按需资源加载解决方案。
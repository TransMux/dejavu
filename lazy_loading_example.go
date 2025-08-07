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
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/88250/gulu"
	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/encryption"
	"github.com/siyuan-note/eventbus"
)

// RunLazyLoadingExample 懒加载功能示例
func RunLazyLoadingExample() {
	// 清理和创建示例目录
	examplePath := "example"
	os.RemoveAll(examplePath)
	defer os.RemoveAll(examplePath)

	// 创建示例数据
	setupExampleData(examplePath)

	// 创建仓库1（主设备）
	repo1 := createExampleRepo(examplePath, "device1")
	defer cleanupRepo(repo1)

	// 演示索引和上传
	demonstrateIndexAndUpload(repo1)

	// 创建仓库2（从设备）
	repo2 := createExampleRepo(examplePath, "device2")
	defer cleanupRepo(repo2)

	// 演示下载和检出
	demonstrateDownloadAndCheckout(repo2)

	// 演示懒加载
	demonstrateLazyLoading(repo2)

	fmt.Println("\n=== 懒加载功能演示完成 ===")
}

func setupExampleData(basePath string) {
	fmt.Println("=== 创建示例数据 ===")

	dataPath := filepath.Join(basePath, "data")
	os.MkdirAll(dataPath, 0755)

	// 创建目录结构
	dirs := []string{
		"docs",
		"media/videos",
		"media/images",
		"cache",
		"backup",
		"large-files",
	}

	for _, dir := range dirs {
		os.MkdirAll(filepath.Join(dataPath, dir), 0755)
	}

	// 创建普通文件（会被正常同步）
	normalFiles := map[string]string{
		"docs/readme.md":     "# 项目文档\n\n这是一个示例项目。",
		"docs/config.json":   `{"app": "example", "version": "1.0"}`,
		"docs/changelog.txt": "v1.0 - 初始版本",
		"src/main.go":        "package main\n\nfunc main() {\n\tfmt.Println(\"Hello\")\n}",
	}

	// 创建懒加载文件（只记录元数据，不下载内容）
	lazyFiles := map[string]string{
		"media/videos/demo.mp4":       strings.Repeat("VIDEO_DATA_", 100),
		"media/videos/tutorial.mov":   strings.Repeat("MOVIE_DATA_", 150),
		"media/images/screenshot.png": strings.Repeat("PNG_DATA_", 50),
		"large-files/dataset.csv":     generateLargeData("CSV", 200),
		"large-files/backup.zip":      generateLargeData("ZIP", 300),
		"cache/temp_data.cache":       generateLargeData("CACHE", 80),
		"backup/full_backup.backup":   generateLargeData("BACKUP", 250),
		"backup/incremental.backup":   generateLargeData("INCR", 100),
	}

	// 写入普通文件
	for path, content := range normalFiles {
		fullPath := filepath.Join(dataPath, path)
		dir := filepath.Dir(fullPath)
		os.MkdirAll(dir, 0755)
		err := gulu.File.WriteFileSafer(fullPath, []byte(content), 0644)
		if err != nil {
			log.Fatalf("写入普通文件失败 [%s]: %v", path, err)
		}
		fmt.Printf("  创建普通文件: %s (%d 字节)\n", path, len(content))
	}

	// 写入懒加载文件
	for path, content := range lazyFiles {
		fullPath := filepath.Join(dataPath, path)
		dir := filepath.Dir(fullPath)
		os.MkdirAll(dir, 0755)
		err := gulu.File.WriteFileSafer(fullPath, []byte(content), 0644)
		if err != nil {
			log.Fatalf("写入懒加载文件失败 [%s]: %v", path, err)
		}
		fmt.Printf("  创建懒加载文件: %s (%d 字节)\n", path, len(content))
	}
}

func generateLargeData(prefix string, factor int) string {
	return strings.Repeat(fmt.Sprintf("%s_CONTENT_", prefix), factor)
}

func createExampleRepo(basePath, deviceID string) *Repo {
	// 路径配置
	dataPath := filepath.Join(basePath, "data")
	repoPath := filepath.Join(basePath, "repo", deviceID)
	historyPath := filepath.Join(basePath, "history", deviceID)
	tempPath := filepath.Join(basePath, "temp", deviceID)
	cloudPath := filepath.Join(basePath, "cloud")

	// 创建目录
	for _, dir := range []string{repoPath, historyPath, tempPath, cloudPath} {
		os.MkdirAll(dir, 0755)
	}

	// 生成AES密钥
	aesKey, err := encryption.KDF("example_password", "example_salt")
	if err != nil {
		log.Fatalf("生成AES密钥失败: %v", err)
	}

	// 配置云存储（使用本地文件系统模拟）
	baseCloud := &cloud.BaseCloud{
		Conf: &cloud.Conf{
			RepoPath: repoPath,
			Local: &cloud.ConfLocal{
				Endpoint: cloudPath,
			},
		},
	}
	localCloud := cloud.NewLocal(baseCloud)

	// 忽略规则
	ignoreLines := []string{
		"*.log",
		"*.tmp",
		".DS_Store",
		"node_modules/",
	}

	// 懒加载模式配置
	lazyLoadingPatterns := []string{
		"media/videos/*",  // 视频文件目录
		"*.mov",           // MOV格式视频
		"*.png",           // PNG图片
		"large-files/*",   // 大文件目录
		"cache/*",         // 缓存文件
		"backup/*.backup", // 备份文件
	}

	// 创建仓库
	repo, err := NewRepoWithLazyLoading(
		dataPath,
		repoPath,
		historyPath,
		tempPath,
		deviceID,
		fmt.Sprintf("ExampleDevice_%s", deviceID),
		"linux",
		aesKey,
		ignoreLines,
		lazyLoadingPatterns,
		localCloud,
	)
	if err != nil {
		log.Fatalf("创建仓库失败: %v", err)
	}

	return repo
}

func cleanupRepo(repo *Repo) {
	// 这里可以添加清理逻辑，如果需要的话
}

func demonstrateIndexAndUpload(repo *Repo) {
	fmt.Println("\n=== 演示索引和上传（主设备）===")

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToNone}

	// 创建索引
	fmt.Println("  正在创建索引...")
	index, err := repo.Index("初始数据索引", false, context)
	if err != nil {
		log.Fatalf("创建索引失败: %v", err)
	}

	fmt.Printf("  索引创建成功: %s\n", index.ID)
	fmt.Printf("  文件总数: %d\n", index.Count)
	fmt.Printf("  总大小: %d 字节\n", index.Size)

	// 分析文件类型
	files, err := repo.GetFiles(index)
	if err != nil {
		log.Fatalf("获取文件列表失败: %v", err)
	}

	var normalFiles, lazyFiles []string
	for _, file := range files {
		if repo.isLazyLoadingFile(file.Path) {
			lazyFiles = append(lazyFiles, file.Path)
		} else {
			normalFiles = append(normalFiles, file.Path)
		}
	}

	fmt.Printf("  普通文件数量: %d\n", len(normalFiles))
	fmt.Printf("  懒加载文件数量: %d\n", len(lazyFiles))

	// 上传到云端
	fmt.Println("  正在上传到云端...")
	_, err = repo.SyncUpload(context)
	if err != nil {
		log.Fatalf("上传失败: %v", err)
	}
	fmt.Println("  上传完成")
}

func demonstrateDownloadAndCheckout(repo *Repo) {
	fmt.Println("\n=== 演示下载和检出（从设备）===")

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToNone}

	// 获取最新索引
	fmt.Println("  正在从云端获取最新索引...")
	latest, err := repo.Latest()
	if err != nil {
		// 如果本地没有索引，从云端下载
		fmt.Println("  本地无索引，正在从云端下载...")

		// 这里简化处理，实际使用中需要知道索引ID
		// 为了演示，我们直接创建一个索引
		index, indexErr := repo.Index("本地索引", false, context)
		if indexErr == nil {
			latest = index
		} else {
			log.Fatalf("获取索引失败: %v", err)
		}
	}

	// 清空数据目录来模拟新设备
	dataPath := repo.DataPath
	checkoutPath := strings.Replace(dataPath, "data", "data-checkout", 1)
	os.RemoveAll(checkoutPath)
	os.MkdirAll(checkoutPath, 0755)

	// 暂时修改数据路径进行检出
	originalPath := repo.DataPath
	repo.DataPath = checkoutPath + string(os.PathSeparator)

	fmt.Println("  正在检出文件...")
	upserts, removes, err := repo.Checkout(latest.ID, context)
	if err != nil {
		log.Fatalf("检出失败: %v", err)
	}

	// 恢复原始路径
	repo.DataPath = originalPath

	fmt.Printf("  检出完成: %d 个文件被创建, %d 个文件被删除\n", len(upserts), len(removes))

	// 检查哪些文件被检出
	fmt.Println("  检查检出结果:")
	checkFiles := []string{
		"docs/readme.md",
		"docs/config.json",
		"media/videos/demo.mp4",
		"large-files/dataset.csv",
	}

	for _, file := range checkFiles {
		fullPath := filepath.Join(checkoutPath, file)
		exists := gulu.File.IsExist(fullPath)
		status := "✗ 不存在"
		if exists {
			status = "✓ 存在"
		}
		fmt.Printf("    %s: %s\n", file, status)
	}
}

func demonstrateLazyLoading(repo *Repo) {
	fmt.Println("\n=== 演示懒加载功能 ===")

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToNone}

	// 获取懒加载文件列表
	fmt.Println("  获取懒加载文件列表...")
	lazyFiles, err := repo.GetLazyLoadingFiles()
	if err != nil {
		log.Fatalf("获取懒加载文件失败: %v", err)
	}

	fmt.Printf("  找到 %d 个懒加载文件:\n", len(lazyFiles))
	for i, file := range lazyFiles {
		fmt.Printf("    %d. %s (%d 字节)\n", i+1, file.Path, file.Size)
		if i >= 4 { // 只显示前5个
			fmt.Printf("    ... 还有 %d 个文件\n", len(lazyFiles)-5)
			break
		}
	}

	if len(lazyFiles) == 0 {
		fmt.Println("  没有懒加载文件，演示结束")
		return
	}

	// 演示单文件懒加载
	fmt.Println("\n  演示单文件懒加载...")
	targetFile := lazyFiles[0]
	filePath := filepath.Join(repo.DataPath, targetFile.Path)

	fmt.Printf("  加载文件: %s\n", targetFile.Path)

	// 检查文件是否存在（应该不存在）
	exists := gulu.File.IsExist(filePath)
	fmt.Printf("  加载前文件存在: %v\n", exists)

	// 懒加载文件
	err = repo.LazyLoadFile(filePath, context)
	if err != nil {
		fmt.Printf("  懒加载失败: %v\n", err)
	} else {
		// 检查文件现在是否存在
		exists = gulu.File.IsExist(filePath)
		fmt.Printf("  加载后文件存在: %v\n", exists)

		if exists {
			// 读取文件内容验证
			content, readErr := os.ReadFile(filePath)
			if readErr == nil {
				fmt.Printf("  文件大小: %d 字节\n", len(content))
			}
		}
	}

	// 演示批量懒加载
	if len(lazyFiles) > 1 {
		fmt.Println("\n  演示批量懒加载...")

		// 选择几个文件进行批量加载
		batchSize := 3
		if len(lazyFiles) < batchSize {
			batchSize = len(lazyFiles)
		}

		var filePaths []string
		for i := 1; i < batchSize; i++ { // 从第二个文件开始（第一个已经加载了）
			filePath := filepath.Join(repo.DataPath, lazyFiles[i].Path)
			filePaths = append(filePaths, filePath)
		}

		if len(filePaths) > 0 {
			fmt.Printf("  批量加载 %d 个文件...\n", len(filePaths))
			for _, path := range filePaths {
				fmt.Printf("    - %s\n", filepath.Base(path))
			}

			err = repo.LazyLoadFiles(filePaths, context)
			if err != nil {
				fmt.Printf("  批量加载失败: %v\n", err)
			} else {
				fmt.Println("  批量加载完成")

				// 验证文件
				for _, path := range filePaths {
					exists := gulu.File.IsExist(path)
					status := "✗"
					if exists {
						status = "✓"
					}
					fmt.Printf("    %s %s\n", status, filepath.Base(path))
				}
			}
		}
	}

	// 演示错误处理
	fmt.Println("\n  演示错误处理...")

	// 尝试加载非懒加载文件
	normalFilePath := filepath.Join(repo.DataPath, "docs/readme.md")
	err = repo.LazyLoadFile(normalFilePath, context)
	if err != nil {
		fmt.Printf("  预期错误（非懒加载文件）: %v\n", err)
	}

	// 尝试加载不存在的文件
	nonExistentPath := filepath.Join(repo.DataPath, "large-files/nonexistent.dat")
	err = repo.LazyLoadFile(nonExistentPath, context)
	if err != nil {
		fmt.Printf("  预期错误（文件不存在）: %v\n", err)
	}

	fmt.Println("\n  懒加载演示完成")
}

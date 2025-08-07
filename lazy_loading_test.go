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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/88250/gulu"
	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/encryption"
	"github.com/siyuan-note/eventbus"
)

const (
	testLazyDataPath         = "testdata/lazy-data"
	testLazyRepoPath         = "testdata/lazy-repo"
	testLazyHistoryPath      = "testdata/lazy-history"
	testLazyTempPath         = "testdata/lazy-temp"
	testLazyCloudPath        = "testdata/lazy-cloud"
	testLazyDataCheckoutPath = "testdata/lazy-data-checkout"
)

func setupLazyLoadingTest(t *testing.T) (repo *Repo, localCloud *cloud.Local) {
	clearLazyTestdata(t)
	createLazyTestData(t)

	aesKey, err := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	if nil != err {
		t.Fatalf("init aes key failed: %s", err)
	}

	baseCloud := &cloud.BaseCloud{
		Conf: &cloud.Conf{
			RepoPath: testLazyRepoPath,
			Local: &cloud.ConfLocal{
				Endpoint: testLazyCloudPath,
			},
		},
	}
	localCloud = cloud.NewLocal(baseCloud)

	ignoreLines := []string{
		"*.log",
		"temp/",
	}

	lazyLoadingPatterns := []string{
		"large-files/*",   // 大文件目录
		"*.mp4",           // 视频文件
		"cache/**",        // 缓存目录及其子目录
		"backup/*.backup", // 备份文件
	}

	repo, err = NewRepoWithLazyLoading(testLazyDataPath, testLazyRepoPath, testLazyHistoryPath, testLazyTempPath, deviceID, deviceName, deviceOS, aesKey, ignoreLines, lazyLoadingPatterns, localCloud)
	if nil != err {
		t.Fatalf("create repo failed: %s", err)
	}

	return repo, localCloud
}

func clearLazyTestdata(t *testing.T) {
	os.RemoveAll(testLazyDataPath)
	os.RemoveAll(testLazyRepoPath)
	os.RemoveAll(testLazyHistoryPath)
	os.RemoveAll(testLazyTempPath)
	os.RemoveAll(testLazyCloudPath)
	os.RemoveAll(testLazyDataCheckoutPath)
}

func createLazyTestData(t *testing.T) {
	// 创建基础目录结构
	dirs := []string{
		testLazyDataPath,
		filepath.Join(testLazyDataPath, "docs"),
		filepath.Join(testLazyDataPath, "large-files"),
		filepath.Join(testLazyDataPath, "cache", "subdir"),
		filepath.Join(testLazyDataPath, "backup"),
		filepath.Join(testLazyDataPath, "temp"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); nil != err {
			t.Fatalf("create dir [%s] failed: %s", dir, err)
		}
	}

	// 创建普通文件（非懒加载）
	normalFiles := map[string]string{
		"docs/readme.txt":        "This is a normal file",
		"docs/config.json":       `{"setting": "value"}`,
		"normal.txt":             "Normal file content",
		"temp/should_ignore.log": "This should be ignored",
	}

	// 创建懒加载文件
	lazyFiles := map[string]string{
		"large-files/big1.dat":         strings.Repeat("A", 1000), // 匹配 large-files/*
		"large-files/big2.dat":         strings.Repeat("B", 2000), // 匹配 large-files/*
		"video.mp4":                    strings.Repeat("V", 500),  // 匹配 *.mp4
		"cache/cached_data.json":       `{"cache": true}`,         // 匹配 cache/**
		"cache/subdir/cached_file.txt": "Cached content",          // 匹配 cache/**
		"backup/data.backup":           "Backup data",             // 匹配 backup/*.backup
	}

	// 写入普通文件
	for path, content := range normalFiles {
		fullPath := filepath.Join(testLazyDataPath, path)
		if err := gulu.File.WriteFileSafer(fullPath, []byte(content), 0644); nil != err {
			t.Fatalf("write normal file [%s] failed: %s", fullPath, err)
		}
	}

	// 写入懒加载文件
	for path, content := range lazyFiles {
		fullPath := filepath.Join(testLazyDataPath, path)
		if err := gulu.File.WriteFileSafer(fullPath, []byte(content), 0644); nil != err {
			t.Fatalf("write lazy file [%s] failed: %s", fullPath, err)
		}
	}
}

func TestLazyLoadingPatternMatching(t *testing.T) {
	repo, _ := setupLazyLoadingTest(t)
	defer clearLazyTestdata(t)

	testCases := []struct {
		path   string
		isLazy bool
		reason string
	}{
		{"/docs/readme.txt", false, "normal file"},
		{"/large-files/big1.dat", true, "matches large-files/*"},
		{"/large-files/big2.dat", true, "matches large-files/*"},
		{"/video.mp4", true, "matches *.mp4"},
		{"/cache/cached_data.json", true, "matches cache/**"},
		{"/cache/subdir/cached_file.txt", true, "matches cache/**"},
		{"/backup/data.backup", true, "matches backup/*.backup"},
		{"/backup/readme.txt", false, "doesn't match backup/*.backup"},
		{"/normal.txt", false, "normal file"},
	}

	for _, tc := range testCases {
		result := repo.isLazyLoadingFile(tc.path)
		if result != tc.isLazy {
			t.Errorf("path [%s] lazy loading check failed: expected %v, got %v (%s)", tc.path, tc.isLazy, result, tc.reason)
		}
	}
}

func TestIndexWithLazyLoading(t *testing.T) {
	repo, _ := setupLazyLoadingTest(t)
	defer clearLazyTestdata(t)

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToNone}

	// 创建索引
	index, err := repo.Index("Test lazy loading index", false, context)
	if nil != err {
		t.Fatalf("create index failed: %s", err)
	}

	if 9 != index.Count {
		t.Errorf("expected 9 files in index, got %d", index.Count)
	}

	// 验证所有文件都被记录
	files, err := repo.GetFiles(index)
	if nil != err {
		t.Fatalf("get files failed: %s", err)
	}

	lazyFileCount := 0
	normalFileCount := 0

	for _, file := range files {
		if repo.isLazyLoadingFile(file.Path) {
			lazyFileCount++
			// 懒加载文件应该有chunks（用于云端存储）
			if len(file.Chunks) == 0 {
				t.Errorf("lazy loading file [%s] should have chunks for cloud storage", file.Path)
			}
		} else {
			normalFileCount++
			// 普通文件应该有chunks
			if len(file.Chunks) == 0 {
				t.Errorf("normal file [%s] should have chunks", file.Path)
			}
		}
	}

	t.Logf("Index created with %d normal files and %d lazy files", normalFileCount, lazyFileCount)
}

func TestCheckoutWithLazyLoading(t *testing.T) {
	repo, _ := setupLazyLoadingTest(t)
	defer clearLazyTestdata(t)

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToNone}

	// 创建索引
	index, err := repo.Index("Test checkout", false, context)
	if nil != err {
		t.Fatalf("create index failed: %s", err)
	}

	// 清空数据目录
	os.RemoveAll(testLazyDataPath)
	os.MkdirAll(testLazyDataPath, 0755)

	// 检出到新目录
	checkoutPath := testLazyDataCheckoutPath
	os.MkdirAll(checkoutPath, 0755)

	// 修改repo的DataPath来检出到不同位置
	originalDataPath := repo.DataPath
	repo.DataPath = checkoutPath + string(os.PathSeparator)

	upserts, removes, err := repo.Checkout(index.ID, context)
	if nil != err {
		t.Fatalf("checkout failed: %s", err)
	}

	// 恢复原始路径
	repo.DataPath = originalDataPath

	t.Logf("Checkout completed: %d upserts, %d removes", len(upserts), len(removes))

	// 验证只有普通文件被检出
	normalFiles := []string{
		"docs/readme.txt",
		"docs/config.json",
		"normal.txt",
	}

	lazyFiles := []string{
		"large-files/big1.dat",
		"large-files/big2.dat",
		"video.mp4",
		"cache/cached_data.json",
		"cache/subdir/cached_file.txt",
		"backup/data.backup",
	}

	// 检查普通文件是否存在
	for _, file := range normalFiles {
		fullPath := filepath.Join(checkoutPath, file)
		if !gulu.File.IsExist(fullPath) {
			t.Errorf("normal file [%s] should exist after checkout", file)
		}
	}

	// 检查懒加载文件是否不存在
	for _, file := range lazyFiles {
		fullPath := filepath.Join(checkoutPath, file)
		if gulu.File.IsExist(fullPath) {
			t.Errorf("lazy loading file [%s] should not exist after checkout", file)
		}
	}
}

func TestLazyLoadFile(t *testing.T) {
	repo, localCloud := setupLazyLoadingTest(t)
	defer clearLazyTestdata(t)

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToNone}

	// 创建索引并上传到云端
	index, err := repo.Index("Test lazy load", false, context)
	if nil != err {
		t.Fatalf("create index failed: %s", err)
	}

	// 上传索引到云端
	_, err = repo.SyncUpload(context)
	if nil != err {
		t.Fatalf("upload failed: %s", err)
	}

	// 清空本地数据
	os.RemoveAll(testLazyDataPath)
	os.MkdirAll(testLazyDataPath, 0755)

	// 重新创建repo以模拟新设备
	aesKey, _ := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	lazyLoadingPatterns := []string{
		"large-files/*",
		"*.mp4",
		"cache/**",
		"backup/*.backup",
	}

	repo2, err := NewRepoWithLazyLoading(testLazyDataPath, testLazyRepoPath, testLazyHistoryPath, testLazyTempPath, deviceID, deviceName, deviceOS, aesKey, []string{}, lazyLoadingPatterns, localCloud)
	if nil != err {
		t.Fatalf("create repo2 failed: %s", err)
	}

	// 从云端下载索引
	_, _, _, err = repo2.DownloadIndex(index.ID, context)
	if nil != err {
		t.Fatalf("download index failed: %s", err)
	}

	// 正常检出（应该跳过懒加载文件）
	_, _, err = repo2.Checkout(index.ID, context)
	if nil != err {
		t.Fatalf("checkout failed: %s", err)
	}

	// 验证懒加载文件不存在
	lazyFilePath := "large-files/big1.dat"
	fullPath := filepath.Join(testLazyDataPath, lazyFilePath)
	if gulu.File.IsExist(fullPath) {
		t.Errorf("lazy file [%s] should not exist before lazy loading", lazyFilePath)
	}

	// 按需加载懒加载文件
	err = repo2.LazyLoadFile(fullPath, context)
	if nil != err {
		t.Fatalf("lazy load file failed: %s", err)
	}

	// 验证文件现在存在
	if !gulu.File.IsExist(fullPath) {
		t.Errorf("lazy file [%s] should exist after lazy loading", lazyFilePath)
	}

	// 验证文件内容正确
	content, err := os.ReadFile(fullPath)
	if nil != err {
		t.Fatalf("read lazy loaded file failed: %s", err)
	}

	expectedContent := strings.Repeat("A", 1000)
	if string(content) != expectedContent {
		t.Errorf("lazy loaded file content mismatch")
	}
}

func TestLazyLoadFileErrors(t *testing.T) {
	repo, _ := setupLazyLoadingTest(t)
	defer clearLazyTestdata(t)

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToNone}

	// 测试加载非懒加载文件
	err := repo.LazyLoadFile(filepath.Join(testLazyDataPath, "docs/readme.txt"), context)
	if nil == err {
		t.Error("should fail when trying to lazy load a non-lazy file")
	}

	// 测试加载不存在的文件
	err = repo.LazyLoadFile(filepath.Join(testLazyDataPath, "large-files/nonexistent.dat"), context)
	if nil == err {
		t.Error("should fail when trying to lazy load a non-existent file")
	}

	// 测试无云存储的情况
	// 首先确保文件不存在本地
	testFilePath := filepath.Join(testLazyDataPath, "large-files/big2.dat")
	os.Remove(testFilePath) // 删除可能存在的文件
	repo.cloud = nil
	err = repo.LazyLoadFile(testFilePath, context)
	if nil == err {
		t.Error("should fail when no cloud storage is available")
	}
}

func TestLazyLoadFiles(t *testing.T) {
	repo, localCloud := setupLazyLoadingTest(t)
	defer clearLazyTestdata(t)

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToNone}

	// 创建索引并上传
	index, err := repo.Index("Test batch lazy load", false, context)
	if nil != err {
		t.Fatalf("create index failed: %s", err)
	}

	_, err = repo.SyncUpload(context)
	if nil != err {
		t.Fatalf("upload failed: %s", err)
	}

	// 清空并重新设置
	os.RemoveAll(testLazyDataPath)
	os.MkdirAll(testLazyDataPath, 0755)

	aesKey, _ := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	lazyLoadingPatterns := []string{
		"large-files/*",
		"*.mp4",
		"cache/**",
		"backup/*.backup",
	}

	repo2, err := NewRepoWithLazyLoading(testLazyDataPath, testLazyRepoPath, testLazyHistoryPath, testLazyTempPath, deviceID, deviceName, deviceOS, aesKey, []string{}, lazyLoadingPatterns, localCloud)
	if nil != err {
		t.Fatalf("create repo2 failed: %s", err)
	}

	_, _, _, err = repo2.DownloadIndex(index.ID, context)
	if nil != err {
		t.Fatalf("download index failed: %s", err)
	}

	_, _, err = repo2.Checkout(index.ID, context)
	if nil != err {
		t.Fatalf("checkout failed: %s", err)
	}

	// 批量加载多个懒加载文件
	filePaths := []string{
		filepath.Join(testLazyDataPath, "large-files/big1.dat"),
		filepath.Join(testLazyDataPath, "large-files/big2.dat"),
		filepath.Join(testLazyDataPath, "video.mp4"),
	}

	err = repo2.LazyLoadFiles(filePaths, context)
	if nil != err {
		t.Fatalf("batch lazy load failed: %s", err)
	}

	// 验证所有文件都已加载
	for _, filePath := range filePaths {
		if !gulu.File.IsExist(filePath) {
			t.Errorf("batch lazy loaded file [%s] should exist", filePath)
		}
	}
}

func TestGetLazyLoadingFiles(t *testing.T) {
	repo, _ := setupLazyLoadingTest(t)
	defer clearLazyTestdata(t)

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToNone}

	// 创建索引
	_, err := repo.Index("Test get lazy files", false, context)
	if nil != err {
		t.Fatalf("create index failed: %s", err)
	}

	// 获取懒加载文件列表
	lazyFiles, err := repo.GetLazyLoadingFiles()
	if nil != err {
		t.Fatalf("get lazy loading files failed: %s", err)
	}

	expectedLazyFiles := []string{
		"/large-files/big1.dat",
		"/large-files/big2.dat",
		"/video.mp4",
		"/cache/cached_data.json",
		"/cache/subdir/cached_file.txt",
		"/backup/data.backup",
	}

	if len(lazyFiles) != len(expectedLazyFiles) {
		t.Errorf("expected %d lazy files, got %d", len(expectedLazyFiles), len(lazyFiles))
	}

	// 验证路径
	lazyFilePaths := make(map[string]bool)
	for _, file := range lazyFiles {
		lazyFilePaths[file.Path] = true
	}

	for _, expectedPath := range expectedLazyFiles {
		if !lazyFilePaths[expectedPath] {
			t.Errorf("expected lazy file [%s] not found", expectedPath)
		}
	}
}

func TestLazyLoadingWithSync(t *testing.T) {
	// 此测试验证懒加载与同步功能的兼容性
	repo, _ := setupLazyLoadingTest(t)
	defer clearLazyTestdata(t)

	context := map[string]interface{}{eventbus.CtxPushMsg: eventbus.CtxPushMsgToNone}

	// 创建索引并上传
	_, err := repo.Index("Test sync with lazy loading", false, context)
	if nil != err {
		t.Fatalf("create index failed: %s", err)
	}

	_, err = repo.SyncUpload(context)
	if nil != err {
		t.Fatalf("upload failed: %s", err)
	}

	// 添加新的懒加载文件
	newLazyFile := filepath.Join(testLazyDataPath, "large-files/big3.dat")
	newContent := strings.Repeat("C", 1500)
	err = gulu.File.WriteFileSafer(newLazyFile, []byte(newContent), 0644)
	if nil != err {
		t.Fatalf("write new lazy file failed: %s", err)
	}

	// 创建新索引
	index2, err := repo.Index("Added new lazy file", false, context)
	if nil != err {
		t.Fatalf("create second index failed: %s", err)
	}

	// 验证新的懒加载文件被正确处理
	files, err := repo.GetFiles(index2)
	if nil != err {
		t.Fatalf("get files failed: %s", err)
	}

	var newFileFound bool
	for _, file := range files {
		if strings.Contains(file.Path, "big3.dat") {
			newFileFound = true
			if repo.isLazyLoadingFile(file.Path) && len(file.Chunks) == 0 {
				t.Errorf("new lazy file [%s] should have chunks for cloud storage", file.Path)
			}
			break
		}
	}

	if !newFileFound {
		t.Error("new lazy file should be included in index")
	}

	t.Logf("Sync test completed successfully")
}

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
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/88250/gulu"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/encryption"
)

const (
	testRepoPassword     = "pass"
	testRepoPasswordSalt = "salt"
	testRepoPath         = "testdata/repo"
	testHistoryPath      = "testdata/history"
	testTempPath         = "testdata/temp"
	testDataPath         = "testdata/data"
	testDataCheckoutPath = "testdata/data-checkout"
)

var (
	deviceID      = "device-id-0"
	deviceName, _ = os.Hostname()
	deviceOS      = runtime.GOOS
)

func TestIndexEmpty(t *testing.T) {
	clearTestdata(t)

	aesKey, err := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	if nil != err {
		return
	}

	testEmptyDataPath := "testdata/empty-data"
	if err = os.MkdirAll(testEmptyDataPath, 0755); nil != err {
		t.Fatalf("mkdir failed: %s", err)
		return
	}
	repo, err := NewRepo(testEmptyDataPath, testRepoPath, testHistoryPath, testTempPath, deviceID, deviceName, deviceOS, aesKey, ignoreLines(), nil)
	if nil != err {
		t.Fatalf("new repo failed: %s", err)
		return
	}
	_, err = repo.Index("Index 1", true, map[string]interface{}{})
	if !errors.Is(err, ErrEmptyIndex) {
		t.Fatalf("should be empty index")
		return
	}
}

func TestPurge(t *testing.T) {
	clearTestdata(t)

	repo, _ := initIndex(t)
	stat, err := repo.Purge(context.Background())
	if nil != err {
		t.Fatalf("purge failed: %s", err)
		return
	}

	t.Logf("purge stat: %#v", stat)
}

func TestIndexCheckout(t *testing.T) {
	clearTestdata(t)

	repo, index := initIndex(t)
	index2, err := repo.Index("Index 2", true, map[string]interface{}{})
	if nil != err {
		t.Fatalf("index failed: %s", err)
		return
	}
	if index.ID != index2.ID {
		t.Fatalf("index id not match")
		return
	}

	aesKey := repo.store.AesKey
	repo, err = NewRepo(testDataCheckoutPath, testRepoPath, testHistoryPath, testTempPath, deviceID, deviceName, deviceOS, aesKey, ignoreLines(), nil)
	if nil != err {
		t.Fatalf("new repo failed: %s", err)
		return
	}
	_, _, err = repo.Checkout(index.ID, map[string]interface{}{})
	if nil != err {
		t.Fatalf("checkout failed: %s", err)
		return
	}

	if !gulu.File.IsExist(filepath.Join(testDataCheckoutPath, "foo")) {
		t.Fatalf("checkout failed")
		return
	}
}

func TestSearchFile(t *testing.T) {
	clearTestdata(t)

	repo, _ := initIndex(t)

	ret, _, _, _, err := repo.SearchFile("foo", 1, 32)
	if nil != err {
		t.Fatalf("search failed: %s", err)
	}

	if "/foo" != ret[0].Path {
		t.Fatalf("search failed")
	}
}

func clearTestdata(t *testing.T) {
	err := os.RemoveAll(testRepoPath)
	if nil != err {
		t.Fatalf("remove failed: %s", err)
		return
	}

	err = os.RemoveAll(testDataCheckoutPath)
	if nil != err {
		t.Fatalf("remove failed: %s", err)
		return
	}
}

func initIndex(t *testing.T) (repo *Repo, index *entity.Index) {
	aesKey, err := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	if nil != err {
		return
	}

	repo, err = NewRepo(testDataPath, testRepoPath, testHistoryPath, testTempPath, deviceID, deviceName, deviceOS, aesKey, ignoreLines(), nil)
	if nil != err {
		t.Fatalf("new repo failed: %s", err)
		return
	}
	index, err = repo.Index("Index 1", true, map[string]interface{}{})
	if nil != err {
		t.Fatalf("index failed: %s", err)
		return
	}
	return
}

func ignoreLines() []string {
	return []string{"bar"}
}

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
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/88250/go-humanize"
	"github.com/88250/gulu"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/filelock"
	"github.com/siyuan-note/logging"
	"github.com/vmihailenco/msgpack/v5"
)

var ErrNotFoundIndex = errors.New("not found index")

func (repo *Repo) Latest() (ret *entity.Index, err error) {
	latest := filepath.Join(repo.Path, "refs", "latest")
	if !filelock.IsExist(latest) {
		err = ErrNotFoundIndex
		return
	}

	data, err := os.ReadFile(latest)
	if nil != err {
		logging.LogErrorf("read latest index [%s] failed: %s", latest, err)
		return
	}
	hash := string(data)
	ret, err = repo.store.GetIndex(hash)
	if nil != err {
		logging.LogErrorf("get latest index [%s] failed: %s", hash, err)
		return
	}
	//logging.LogInfof("got local latest [%s]", ret.String())
	return
}

// FullIndex 描述了完整的索引结构。
type FullIndex struct {
	ID           string         `json:"id"`
	Files        []*entity.File `json:"files"`
	LazyManifest string         `json:"lazyManifest,omitempty"`
	Spec         int            `json:"spec"`
}

func (repo *Repo) UpdateLatest(index *entity.Index) (err error) {
	start := time.Now()

	refs := filepath.Join(repo.Path, "refs")
	err = os.MkdirAll(refs, 0755)
	if nil != err {
		return
	}
	fullLatestPath := filepath.Join(repo.Path, "full-latest.json")
	latestPath := filepath.Join(refs, "latest")
	if data, readErr := os.ReadFile(latestPath); nil == readErr && string(data) == index.ID {
		if fullLatest := repo.getFullLatest(index); nil != fullLatest && 1 <= fullLatest.Spec {
			return nil
		}
	}
	compact := 0 == len(index.LazyFiles)
	if repo.lazyLoadEnabled && nil != repo.lazyLoader && 0 < len(index.LazyFiles) && "" != index.LazyManifest {
		if manifest, manifestErr := repo.lazyLoader.getManifest(); nil == manifestErr &&
			lazyManifestFormatCurrent == lazyManifestFormat(manifest) &&
			(repo.isLazyManifestLocalClosurePrepared(index.LazyManifest) || repo.isLazyManifestPrepared(index.LazyManifest)) {
			compact = true
		}
	}
	var files []*entity.File
	if compact {
		files, err = repo.getFiles(index.Files)
	} else {
		files, err = repo.GetFiles(index)
	}
	if nil != err {
		return
	}
	fullIndex := &FullIndex{ID: index.ID, Files: files}
	if compact {
		fullIndex.LazyManifest = index.LazyManifest
		fullIndex.Spec = 1
	}
	data, err := msgpack.Marshal(fullIndex)
	if nil != err {
		return
	}
	err = gulu.File.WriteFileSafer(fullLatestPath, data, 0644)
	if nil != err {
		return
	}
	err = gulu.File.WriteFileSafer(latestPath, []byte(index.ID), 0644)
	if nil != err {
		return
	}

	logging.LogInfof("updated local latest to [%s], full latest [size=%s], cost [%s]", index.String(), humanize.Bytes(uint64(len(data))), time.Since(start))
	return
}

func (repo *Repo) getFullLatest(latest *entity.Index) (ret *FullIndex) {
	start := time.Now()

	fullLatestPath := filepath.Join(repo.Path, "full-latest.json")
	if !gulu.File.IsExist(fullLatestPath) {
		return
	}

	data, err := os.ReadFile(fullLatestPath)
	if nil != err {
		logging.LogErrorf("read full latest failed: %s", err)
		return
	}

	ret = &FullIndex{}
	if err = msgpack.Unmarshal(data, ret); nil != err {
		logging.LogErrorf("unmarshal full latest [%s] failed: %s", fullLatestPath, err)
		ret = nil
		if err = os.RemoveAll(fullLatestPath); nil != err {
			logging.LogErrorf("remove full latest [%s] failed: %s", fullLatestPath, err)
		}
		return
	}

	if ret.ID != latest.ID {
		logging.LogErrorf("full latest ID [%s] not match latest ID [%s]", ret.ID, latest.ID)
		ret = nil
		if err = os.RemoveAll(fullLatestPath); nil != err {
			logging.LogErrorf("remove full latest [%s] failed: %s", fullLatestPath, err)
		}
		return
	}
	expectedIDs := latest.Files
	if 1 > ret.Spec {
		expectedIDs = append(append(make([]string, 0, len(latest.Files)+len(latest.LazyFiles)), latest.Files...), latest.LazyFiles...)
	} else if ret.LazyManifest != latest.LazyManifest {
		logging.LogErrorf("full latest lazy manifest [%s] not match latest [%s]", ret.LazyManifest, latest.LazyManifest)
		ret = nil
		if err = os.RemoveAll(fullLatestPath); nil != err {
			logging.LogErrorf("remove full latest [%s] failed: %s", fullLatestPath, err)
		}
		return
	}
	if len(ret.Files) != len(expectedIDs) {
		logging.LogErrorf("full latest files count [%d] not match latest files count [%d]", len(ret.Files), len(expectedIDs))
		ret = nil
		if err = os.RemoveAll(fullLatestPath); nil != err {
			logging.LogErrorf("remove full latest [%s] failed: %s", fullLatestPath, err)
		}
		return
	}
	for i, file := range ret.Files {
		if nil == file || file.ID != expectedIDs[i] {
			logging.LogErrorf("full latest file at [%d] does not match latest", i)
			ret = nil
			if err = os.RemoveAll(fullLatestPath); nil != err {
				logging.LogErrorf("remove full latest [%s] failed: %s", fullLatestPath, err)
			}
			return
		}
	}

	for _, f := range ret.Files {
		repo.store.cacheFile(f)
	}

	logging.LogInfof("got local full latest [files=%d, size=%s], cost [%s]", len(ret.Files), humanize.Bytes(uint64(len(data))), time.Since(start))
	return
}

func (repo *Repo) GetTag(tag string) (id string, err error) {
	if !gulu.File.IsValidFilename(tag) {
		err = errors.New("invalid tag name")
	}
	tag = filepath.Join(repo.Path, "refs", "tags", tag)
	if !filelock.IsExist(tag) {
		err = errors.New("tag not found")
	}
	data, err := filelock.ReadFile(tag)
	if nil != err {
		return
	}
	id = string(data)
	return
}

func (repo *Repo) AddTag(id, tag string) (err error) {
	if !gulu.File.IsValidFilename(tag) {
		return errors.New("invalid tag name")
	}

	_, err = repo.store.GetIndex(id)
	if nil != err {
		return
	}

	tags := filepath.Join(repo.Path, "refs", "tags")
	if err = os.MkdirAll(tags, 0755); nil != err {
		return
	}
	tag = filepath.Join(tags, tag)
	err = gulu.File.WriteFileSafer(tag, []byte(id), 0644)
	return
}

func (repo *Repo) RemoveTag(tag string) (err error) {
	tag = filepath.Join(repo.Path, "refs", "tags", tag)
	if !gulu.File.IsExist(tag) {
		return
	}

	err = os.Remove(tag)
	return
}

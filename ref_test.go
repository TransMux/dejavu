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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/dejavu/util"
	"github.com/vmihailenco/msgpack/v5"
)

func TestUpdateLatestSameIndexKeepsFullLatest(t *testing.T) {
	clearTestdata(t)
	repo, index := initIndex(t)
	fullLatestPath := filepath.Join(repo.Path, "full-latest.json")
	wantModTime := time.Unix(1700000000, 0)
	if err := os.Chtimes(fullLatestPath, wantModTime, wantModTime); nil != err {
		t.Fatal(err)
	}
	if err := repo.UpdateLatest(index); nil != err {
		t.Fatal(err)
	}
	info, err := os.Stat(fullLatestPath)
	if nil != err {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(wantModTime) {
		t.Fatalf("full latest was rewritten for unchanged index: %s", info.ModTime())
	}
}

func TestUpdateLatestSameIndexRebuildsIncompleteFullLatest(t *testing.T) {
	clearTestdata(t)
	repo, index := initIndex(t)
	fullLatestPath := filepath.Join(repo.Path, "full-latest.json")
	data, err := msgpack.Marshal(&FullIndex{ID: index.ID, Files: []*entity.File{}, Spec: 0})
	if nil != err {
		t.Fatal(err)
	}
	if err = os.WriteFile(fullLatestPath, data, 0644); nil != err {
		t.Fatal(err)
	}
	if err = repo.UpdateLatest(index); nil != err {
		t.Fatal(err)
	}
	full := repo.getFullLatest(index)
	if nil == full || len(full.Files) != len(index.Files)+len(index.LazyFiles) {
		t.Fatalf("full latest was not rebuilt: %#v", full)
	}
}

func TestUpdateLatestDoesNotAdvanceRefWhenFullLatestCannotBeBuilt(t *testing.T) {
	clearTestdata(t)
	repo, previous := initIndex(t)
	broken := *previous
	broken.ID = util.RandHash()
	broken.LazyFiles = append(append([]string{}, previous.LazyFiles...), util.RandHash())
	if err := repo.store.PutIndex(&broken); nil != err {
		t.Fatal(err)
	}
	if err := repo.UpdateLatest(&broken); nil == err {
		t.Fatal("expected missing lazy metadata to prevent latest update")
	}
	data, err := os.ReadFile(filepath.Join(repo.Path, "refs", "latest"))
	if nil != err {
		t.Fatal(err)
	}
	if string(data) != previous.ID {
		t.Fatalf("latest advanced to %s after full latest failure, want %s", data, previous.ID)
	}
}

func TestUpdateLatestUsesCompactFullLatestForPreparedCurrentManifest(t *testing.T) {
	repo := newLazyTestRepo(t)
	assetData := []byte("asset")
	assetFile := entity.NewFile("assets/a.bin", int64(len(assetData)), 1000)
	assetFile.Chunks = []string{util.Hash(assetData)}
	if err := repo.store.PutFile(assetFile); nil != err {
		t.Fatal(err)
	}
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: map[string]*LazyAsset{
		assetFile.Path: {
			Path: assetFile.Path, FileID: assetFile.ID, Size: assetFile.Size, Modified: assetFile.Updated,
			Chunks: append([]string{}, assetFile.Chunks...),
		},
	}}
	if err := repo.lazyLoader.saveManifest(manifest); nil != err {
		t.Fatal(err)
	}
	identity, err := repo.lazyManifestIdentity()
	if nil != err {
		t.Fatal(err)
	}
	if err = repo.markLazyManifestLocalClosurePrepared(identity); nil != err {
		t.Fatal(err)
	}
	index, err := repo.Index("compact latest", false, map[string]interface{}{})
	if nil != err {
		t.Fatal(err)
	}
	full := repo.getFullLatest(index)
	if nil == full {
		t.Fatal("compact full latest missing")
	}
	if 1 != full.Spec || identity != full.LazyManifest {
		t.Fatalf("unexpected compact full latest: spec=%d manifest=%s", full.Spec, full.LazyManifest)
	}
	if len(full.Files) != len(index.Files) {
		t.Fatalf("compact full latest expanded lazy metadata: files=%d normal=%d lazy=%d", len(full.Files), len(index.Files), len(index.LazyFiles))
	}
	if 1 != len(index.LazyFiles) || assetFile.ID != index.LazyFiles[0] {
		t.Fatalf("lazy index changed: %#v", index.LazyFiles)
	}
}

func BenchmarkIndexWarm100KLazyAssets(b *testing.B) {
	repo := newLazyTestRepo(b)
	assets := make(map[string]*LazyAsset, 100000)
	chunkID := util.Hash([]byte("asset"))
	for i := 0; i < 100000; i++ {
		path := fmt.Sprintf("assets/%06d.bin", i)
		file := entity.NewFile(path, 5, int64(1000+i))
		assets[path] = &LazyAsset{Path: path, FileID: file.ID, Size: file.Size, Modified: file.Updated, Chunks: []string{chunkID}}
	}
	if err := repo.lazyLoader.saveManifest(&LazyManifest{Version: lazyManifestFormatCurrent, Assets: assets}); nil != err {
		b.Fatal(err)
	}
	identity, err := repo.lazyManifestIdentity()
	if nil != err {
		b.Fatal(err)
	}
	if err = os.MkdirAll(repo.Path, 0755); nil != err {
		b.Fatal(err)
	}
	if err = repo.markLazyManifestLocalClosurePrepared(identity); nil != err {
		b.Fatal(err)
	}
	if _, err = repo.Index("benchmark setup", false, map[string]interface{}{}); nil != err {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err = repo.Index("benchmark warm", false, map[string]interface{}{}); nil != err {
			b.Fatal(err)
		}
	}
}

func BenchmarkIndexChangedManifest100KLazyAssets(b *testing.B) {
	repo := newLazyTestRepo(b)
	assets := make(map[string]*LazyAsset, 100000)
	chunkID := util.Hash([]byte("asset"))
	for i := 0; i < 100000; i++ {
		path := fmt.Sprintf("assets/%06d.bin", i)
		file := entity.NewFile(path, 5, int64(1000+i))
		assets[path] = &LazyAsset{Path: path, FileID: file.ID, Size: file.Size, Modified: file.Updated, Chunks: []string{chunkID}}
	}
	manifest := &LazyManifest{Version: lazyManifestFormatCurrent, Assets: assets}
	if err := repo.lazyLoader.saveManifest(manifest); nil != err {
		b.Fatal(err)
	}
	identity, err := repo.lazyManifestIdentity()
	if nil != err {
		b.Fatal(err)
	}
	if err = os.MkdirAll(repo.Path, 0755); nil != err {
		b.Fatal(err)
	}
	if err = repo.markLazyManifestLocalClosurePrepared(identity); nil != err {
		b.Fatal(err)
	}
	if _, err = repo.Index("benchmark setup", false, map[string]interface{}{}); nil != err {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		asset := manifest.Assets["assets/000000.bin"]
		asset.Modified += 1000
		asset.FileID = entity.NewFile(asset.Path, asset.Size, asset.Modified).ID
		if err = repo.lazyLoader.saveManifest(manifest); nil != err {
			b.Fatal(err)
		}
		if _, err = repo.Index("benchmark changed manifest", false, map[string]interface{}{}); nil != err {
			b.Fatal(err)
		}
	}
}

func TestTag(t *testing.T) {
	clearTestdata(t)

	repo, index := initIndex(t)
	err := repo.AddTag(index.ID, "v1.0.0")
	if nil != err {
		t.Fatalf("add tag failed: %s", err)
		return
	}

	v100, err := repo.GetTag("v1.0.0")
	if v100 != index.ID {
		t.Fatalf("get tag failed: %s", err)
		return
	}

	err = repo.AddTag(index.ID, "v1.0.1")
	if nil != err {
		t.Fatalf("add tag failed: %s", err)
		return
	}

	v101, err := repo.GetTag("v1.0.1")
	if v101 != v100 {
		t.Fatalf("get tag failed: %s", err)
		return
	}
}

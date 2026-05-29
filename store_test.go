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
	"bytes"
	"os"
	"testing"

	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/dejavu/util"
	"github.com/siyuan-note/encryption"
)

func TestPutGet(t *testing.T) {
	clearTestdata(t)

	aesKey, err := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	if nil != err {
		t.Fatalf("kdf failed: %s", err)
		return
	}

	store, err := NewStore(testRepoPath, aesKey)
	if nil != err {
		t.Fatalf("new store failed: %s", err)
		return
	}

	data := []byte("Hello!")
	chunk := &entity.Chunk{ID: util.Hash(data), Data: data}
	err = store.PutChunk(chunk)
	if nil != err {
		t.Fatalf("put failed: %s", err)
		return
	}

	chunk, err = store.GetChunk(chunk.ID)
	if nil != err {
		t.Fatalf("get failed: %s", err)
		return
	}
	if 0 != bytes.Compare(chunk.Data, data) {
		t.Fatalf("data not match")
		return
	}

	err = store.Remove(chunk.ID)
	if nil != err {
		t.Fatalf("remove failed: %s", err)
		return
	}

	chunk, err = store.GetChunk(chunk.ID)
	if nil != chunk {
		t.Fatalf("get should be failed")
		return
	}
}

func TestPutChunkRejectsHashMismatch(t *testing.T) {
	clearTestdata(t)

	aesKey, err := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	if nil != err {
		t.Fatalf("kdf failed: %s", err)
	}
	store, err := NewStore(testRepoPath, aesKey)
	if nil != err {
		t.Fatalf("new store failed: %s", err)
	}

	err = store.PutChunk(&entity.Chunk{ID: util.Hash([]byte("expected")), Data: []byte("actual")})
	if nil == err {
		t.Fatal("put chunk should reject hash mismatch")
	}
}

func TestGetChunkRejectsStoredHashMismatch(t *testing.T) {
	clearTestdata(t)

	aesKey, err := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	if nil != err {
		t.Fatalf("kdf failed: %s", err)
	}
	store, err := NewStore(testRepoPath, aesKey)
	if nil != err {
		t.Fatalf("new store failed: %s", err)
	}

	chunkID := util.Hash([]byte("expected"))
	dir, file := store.AbsPath(chunkID)
	if err = os.MkdirAll(dir, 0755); nil != err {
		t.Fatalf("mkdir failed: %s", err)
	}
	data, err := store.encodeData([]byte("actual"))
	if nil != err {
		t.Fatalf("encode failed: %s", err)
	}
	if err = os.WriteFile(file, data, 0644); nil != err {
		t.Fatalf("write object failed: %s", err)
	}

	chunk, err := store.GetChunk(chunkID)
	if nil == err {
		t.Fatalf("get chunk should reject hash mismatch: %#v", chunk)
	}
}

func TestPutChunkOverwritesExistingHashMismatch(t *testing.T) {
	clearTestdata(t)

	aesKey, err := encryption.KDF(testRepoPassword, testRepoPasswordSalt)
	if nil != err {
		t.Fatalf("kdf failed: %s", err)
	}
	store, err := NewStore(testRepoPath, aesKey)
	if nil != err {
		t.Fatalf("new store failed: %s", err)
	}

	data := []byte("expected")
	chunkID := util.Hash(data)
	dir, file := store.AbsPath(chunkID)
	if err = os.MkdirAll(dir, 0755); nil != err {
		t.Fatalf("mkdir failed: %s", err)
	}
	encoded, err := store.encodeData([]byte("actual"))
	if nil != err {
		t.Fatalf("encode failed: %s", err)
	}
	if err = os.WriteFile(file, encoded, 0644); nil != err {
		t.Fatalf("write object failed: %s", err)
	}

	if err = store.PutChunk(&entity.Chunk{ID: chunkID, Data: data}); nil != err {
		t.Fatalf("put chunk should overwrite corrupt existing object: %s", err)
	}
	chunk, err := store.GetChunk(chunkID)
	if nil != err {
		t.Fatalf("get overwritten chunk failed: %s", err)
	}
	if !bytes.Equal(chunk.Data, data) {
		t.Fatalf("unexpected chunk data [%s]", chunk.Data)
	}
}

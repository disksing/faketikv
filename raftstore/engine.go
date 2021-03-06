// Copyright 2019-present PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package raftstore

import (
	"bytes"
	"math"
	"sync/atomic"
	"time"

	"github.com/cznic/mathutil"
	"github.com/golang/protobuf/proto"
	"github.com/pingcap/badger"
	"github.com/pingcap/badger/y"
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/raft_serverpb"
	"github.com/pingcap/tidb/store/mockstore/unistore/lockstore"
	"github.com/pingcap/tidb/store/mockstore/unistore/metrics"
	"github.com/pingcap/tidb/store/mockstore/unistore/tikv/dbreader"
	"github.com/pingcap/tidb/store/mockstore/unistore/tikv/mvcc"
)

type regionSnapshot struct {
	regionState *raft_serverpb.RegionLocalState
	txn         *badger.Txn
	lockSnap    *lockstore.MemStore
	term        uint64
	index       uint64
}

func (rs *regionSnapshot) redoLocks(raft *badger.DB, redoIdx uint64) error {
	regionID := rs.regionState.Region.Id
	item, err := rs.txn.Get(ApplyStateKey(regionID))
	if err != nil {
		return err
	}
	val, err := item.Value()
	if err != nil {
		return err
	}
	var applyState applyState
	applyState.Unmarshal(val)
	appliedIdx := applyState.appliedIndex
	entries, _, err := fetchEntriesTo(raft, regionID, redoIdx, appliedIdx+1, math.MaxUint64, nil)
	if err != nil {
		return err
	}
	for i := range entries {
		err = restoreAppliedEntry(&entries[i], rs.txn, rs.lockSnap)
		if err != nil {
			return err
		}
	}
	return nil
}

// Engines represents storage engines
type Engines struct {
	kv       *mvcc.DBBundle
	kvPath   string
	raft     *badger.DB
	raftPath string
}

// NewEngines creates a new Engines.
func NewEngines(kvEngine *mvcc.DBBundle, raftEngine *badger.DB, kvPath, raftPath string) *Engines {
	return &Engines{
		kv:       kvEngine,
		kvPath:   kvPath,
		raft:     raftEngine,
		raftPath: raftPath,
	}
}

func (en *Engines) newRegionSnapshot(regionID, redoIdx uint64) (snap *regionSnapshot, err error) {
	// We need to get the old region state out of the snapshot transaction to fetch data in lockStore.
	// The lockStore data must be fetch before we start the snapshot transaction to make sure there is no newer data
	// in the lockStore. The missing old data can be restored by raft log.
	oldRegionState, err := getRegionLocalState(en.kv.DB, regionID)
	if err != nil {
		return nil, err
	}
	lockSnap := lockstore.NewMemStore(8 << 20)
	iter := en.kv.LockStore.NewIterator()
	start, end := RawStartKey(oldRegionState.Region), RawEndKey(oldRegionState.Region)
	for iter.Seek(start); iter.Valid() && (len(end) == 0 || bytes.Compare(iter.Key(), end) < 0); iter.Next() {
		lockSnap.Put(iter.Key(), iter.Value())
	}

	txn := en.kv.DB.NewTransaction(false)
	defer func() {
		if err != nil {
			txn.Discard()
		}
	}()

	// Verify that the region version to make sure the start key and end key has not changed.
	regionState := new(raft_serverpb.RegionLocalState)
	val, err := getValueTxn(txn, RegionStateKey(regionID))
	if err != nil {
		return nil, err
	}
	err = regionState.Unmarshal(val)
	if err != nil {
		return nil, err
	}
	if regionState.Region.RegionEpoch.Version != oldRegionState.Region.RegionEpoch.Version {
		return nil, errors.New("region changed during newRegionSnapshot")
	}

	index, term, err := getAppliedIdxTermForSnapshot(en.raft, txn, regionID)
	if err != nil {
		return nil, err
	}
	snap = &regionSnapshot{
		regionState: regionState,
		txn:         txn,
		lockSnap:    lockSnap,
		term:        term,
		index:       index,
	}
	err = snap.redoLocks(en.raft, redoIdx)
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// WriteKV flushes the WriteBatch to the kv.
func (en *Engines) WriteKV(wb *WriteBatch) error {
	return wb.WriteToKV(en.kv)
}

// WriteRaft flushes the WriteBatch to the raft.
func (en *Engines) WriteRaft(wb *WriteBatch) error {
	return wb.WriteToRaft(en.raft)
}

// SyncKVWAL syncs the kv wal.
func (en *Engines) SyncKVWAL() error {
	// TODO: implement
	return nil
}

// SyncRaftWAL syncs the raft wal.
func (en *Engines) SyncRaftWAL() error {
	// TODO: implement
	return nil
}

// WriteBatch writes a batch of entries.
type WriteBatch struct {
	entries       []*badger.Entry
	lockEntries   []*badger.Entry
	size          int
	safePoint     int
	safePointLock int
	safePointSize int
	safePointUndo int
}

// Len returns the length of the WriteBatch.
func (wb *WriteBatch) Len() int {
	return len(wb.entries) + len(wb.lockEntries)
}

// Set adds the key-value pair to the entries.
func (wb *WriteBatch) Set(key y.Key, val []byte) {
	wb.entries = append(wb.entries, &badger.Entry{
		Key:   key,
		Value: val,
	})
	wb.size += key.Len() + len(val)
}

// SetLock adds the key-value pair to the lockEntries.
func (wb *WriteBatch) SetLock(key, val []byte) {
	wb.lockEntries = append(wb.lockEntries, &badger.Entry{
		Key:      y.KeyWithTs(key, 0),
		Value:    val,
		UserMeta: mvcc.LockUserMetaNone,
	})
}

// DeleteLock deletes the key from the lockEntries.
func (wb *WriteBatch) DeleteLock(key []byte) {
	wb.lockEntries = append(wb.lockEntries, &badger.Entry{
		Key:      y.KeyWithTs(key, 0),
		UserMeta: mvcc.LockUserMetaDelete,
	})
}

// Rollback rolls back the key.
func (wb *WriteBatch) Rollback(key y.Key) {
	rollbackKey := mvcc.EncodeExtraTxnStatusKey(key.UserKey, key.Version)
	wb.entries = append(wb.entries, &badger.Entry{
		Key:      y.KeyWithTs(rollbackKey, key.Version),
		UserMeta: mvcc.NewDBUserMeta(key.Version, 0),
	})
}

// SetWithUserMeta adds the key-value pair with the user meta.
func (wb *WriteBatch) SetWithUserMeta(key y.Key, val, userMeta []byte) {
	wb.entries = append(wb.entries, &badger.Entry{
		Key:      key,
		Value:    val,
		UserMeta: userMeta,
	})
	wb.size += key.Len() + len(val) + len(userMeta)
}

// SetOpLock adds an op lock entry to the entries.
func (wb *WriteBatch) SetOpLock(key y.Key, userMeta []byte) {
	startTS := mvcc.DBUserMeta(userMeta).StartTS()
	opLockKey := y.KeyWithTs(mvcc.EncodeExtraTxnStatusKey(key.UserKey, startTS), key.Version)
	e := &badger.Entry{
		Key:      opLockKey,
		UserMeta: userMeta,
	}
	wb.entries = append(wb.entries, e)
	wb.size += key.Len() + len(userMeta)
}

// Delete deletes the key from the entries.
func (wb *WriteBatch) Delete(key y.Key) {
	wb.entries = append(wb.entries, &badger.Entry{
		Key: key,
	})
	wb.size += key.Len()
}

// SetMsg adds the y.Key and proto.Message to the entries..
func (wb *WriteBatch) SetMsg(key y.Key, msg proto.Message) error {
	val, err := proto.Marshal(msg)
	if err != nil {
		return errors.WithStack(err)
	}
	wb.Set(key, val)
	return nil
}

// SetSafePoint sets a safe point.
func (wb *WriteBatch) SetSafePoint() {
	wb.safePoint = len(wb.entries)
	wb.safePointLock = len(wb.lockEntries)
	wb.safePointSize = wb.size
}

// RollbackToSafePoint rolls back to the safe point.
func (wb *WriteBatch) RollbackToSafePoint() {
	wb.entries = wb.entries[:wb.safePoint]
	wb.lockEntries = wb.lockEntries[:wb.safePointLock]
	wb.size = wb.safePointSize
}

// WriteToKV flushes WriteBatch to DB by two steps:
// 	1. Write entries to badger. After save ApplyState to badger, subsequent regionSnapshot will start at new raft index.
//	2. Update lockStore, the date in lockStore may be older than the DB, so we need to restore then entries from raft log.
func (wb *WriteBatch) WriteToKV(bundle *mvcc.DBBundle) error {
	if len(wb.entries) > 0 {
		start := time.Now()
		keyVersion := atomic.AddUint64(&bundle.StateTS, 1)
		err := bundle.DB.Update(func(txn *badger.Txn) error {
			for _, entry := range wb.entries {
				if len(entry.UserMeta) == 0 && len(entry.Value) == 0 {
					entry.SetDelete()
				}
				if entry.Key.Version == KvTS {
					entry.Key.Version = keyVersion
				}
				err1 := txn.SetEntry(entry)
				if err1 != nil {
					return err1
				}
			}
			return nil
		})
		metrics.KVDBUpdate.Observe(time.Since(start).Seconds())
		if err != nil {
			return errors.WithStack(err)
		}
	}
	if len(wb.lockEntries) > 0 {
		start := time.Now()
		hint := new(lockstore.Hint)
		bundle.MemStoreMu.Lock()
		for _, entry := range wb.lockEntries {
			switch entry.UserMeta[0] {
			case mvcc.LockUserMetaDeleteByte:
				bundle.LockStore.DeleteWithHint(entry.Key.UserKey, hint)
			default:
				bundle.LockStore.PutWithHint(entry.Key.UserKey, entry.Value, hint)
			}
		}
		bundle.MemStoreMu.Unlock()
		metrics.LockUpdate.Observe(time.Since(start).Seconds())
	}
	return nil
}

// WriteToRaft flushes WriteBatch to raft.
func (wb *WriteBatch) WriteToRaft(db *badger.DB) error {
	if len(wb.entries) > 0 {
		start := time.Now()
		err := db.Update(func(txn *badger.Txn) error {
			for _, entry := range wb.entries {
				if len(entry.Value) == 0 {
					entry.SetDelete()
				}
				err1 := txn.SetEntry(entry)
				if err1 != nil {
					return err1
				}
			}
			return nil
		})
		metrics.RaftDBUpdate.Observe(time.Since(start).Seconds())
		if err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

// MustWriteToKV wraps WriteToKV and will panic if error is not nil.
func (wb *WriteBatch) MustWriteToKV(db *mvcc.DBBundle) {
	err := wb.WriteToKV(db)
	if err != nil {
		panic(err)
	}
}

// MustWriteToRaft wraps WriteToRaft and will panic if error is not nil.
func (wb *WriteBatch) MustWriteToRaft(db *badger.DB) {
	err := wb.WriteToRaft(db)
	if err != nil {
		panic(err)
	}
}

// Reset resets the WriteBatch.
func (wb *WriteBatch) Reset() {
	for i := range wb.entries {
		wb.entries[i] = nil
	}
	wb.entries = wb.entries[:0]
	for i := range wb.lockEntries {
		wb.lockEntries[i] = nil
	}
	wb.lockEntries = wb.lockEntries[:0]
	wb.size = 0
	wb.safePoint = 0
	wb.safePointLock = 0
	wb.safePointSize = 0
	wb.safePointUndo = 0
}

// Todo, the following code redundant to unistore/tikv/worker.go, just as a place holder now.

const delRangeBatchSize = 4096

func deleteRange(db *mvcc.DBBundle, startKey, endKey []byte) error {
	// Delete keys first.
	keys := make([]y.Key, 0, delRangeBatchSize)
	txn := db.DB.NewTransaction(false)
	reader := dbreader.NewDBReader(startKey, endKey, txn)
	keys = collectRangeKeys(reader.GetIter(), startKey, endKey, keys)
	reader.Close()
	if err := deleteKeysInBatch(db, keys, delRangeBatchSize); err != nil {
		return err
	}

	// Delete lock
	lockIte := db.LockStore.NewIterator()
	keys = keys[:0]
	keys = collectLockRangeKeys(lockIte, startKey, endKey, keys)
	return deleteLocksInBatch(db, keys, delRangeBatchSize)
}

func collectRangeKeys(it *badger.Iterator, startKey, endKey []byte, keys []y.Key) []y.Key {
	if len(endKey) == 0 {
		panic("invalid end key")
	}
	for it.Seek(startKey); it.Valid(); it.Next() {
		item := it.Item()
		key := item.KeyCopy(nil)
		if exceedEndKey(key, endKey) {
			break
		}
		keys = append(keys, y.KeyWithTs(key, item.Version()))
	}
	return keys
}

func collectLockRangeKeys(it *lockstore.Iterator, startKey, endKey []byte, keys []y.Key) []y.Key {
	if len(endKey) == 0 {
		panic("invalid end key")
	}
	for it.Seek(startKey); it.Valid(); it.Next() {
		key := safeCopy(it.Key())
		if exceedEndKey(key, endKey) {
			break
		}
		keys = append(keys, y.KeyWithTs(key, 0))
	}
	return keys
}

func deleteKeysInBatch(db *mvcc.DBBundle, keys []y.Key, batchSize int) error {
	for len(keys) > 0 {
		batchSize := mathutil.Min(len(keys), batchSize)
		batchKeys := keys[:batchSize]
		keys = keys[batchSize:]
		dbBatch := new(WriteBatch)
		for _, key := range batchKeys {
			key.Version++
			dbBatch.Delete(key)
		}
		if err := dbBatch.WriteToKV(db); err != nil {
			return err
		}
	}
	return nil
}

func deleteLocksInBatch(db *mvcc.DBBundle, keys []y.Key, batchSize int) error {
	for len(keys) > 0 {
		batchSize := mathutil.Min(len(keys), batchSize)
		batchKeys := keys[:batchSize]
		keys = keys[batchSize:]
		dbBatch := new(WriteBatch)
		for _, key := range batchKeys {
			dbBatch.DeleteLock(key.UserKey)
		}
		if err := dbBatch.WriteToKV(db); err != nil {
			return err
		}
	}
	return nil
}

type raftLogFilter struct {
}

func (r *raftLogFilter) Filter(key, val, userMeta []byte) badger.Decision {
	return badger.DecisionKeep
}

var raftLogGuard = badger.Guard{
	Prefix:   []byte{LocalPrefix, RegionRaftPrefix},
	MatchLen: 10,
	MinSize:  1024 * 1024,
}

func (r *raftLogFilter) Guards() []badger.Guard {
	return []badger.Guard{
		raftLogGuard,
	}
}

// CreateRaftLogCompactionFilter creates a new badger.CompactionFilter.
func CreateRaftLogCompactionFilter(targetLevel int, startKey, endKey []byte) badger.CompactionFilter {
	return &raftLogFilter{}
}

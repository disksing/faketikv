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

package rocksdb

import (
	"os"

	"github.com/pingcap/errors"
)

// Error
var (
	ErrChecksumMismatch    = errors.New("Checksum mismatch")
	ErrMagicNumberMismatch = errors.New("Magic number mismatch")
	errEnd                 = errors.New("reach end of block")
)

// SstFileIterator is an iterator for an SST file.
type SstFileIterator struct {
	f              *os.File
	indexBlockIter *blockIterator
	dataBlockIter  *blockIterator
	readBuf        []byte
	dataBuf        []byte
	invalid        bool
	err            error
	checksumType   ChecksumType
}

// NewSstFileIterator returns a new SstFileIterator.
func NewSstFileIterator(f *os.File) (*SstFileIterator, error) {
	it := &SstFileIterator{
		f:             f,
		dataBlockIter: new(blockIterator),
	}

	if err := it.loadIndexBlock(); err != nil {
		return nil, err
	}

	return it, nil
}

// SeekToFirst moves the iterator to the first key.
func (it *SstFileIterator) SeekToFirst() {
	it.indexBlockIter.Rewind()
	it.invalid = false
	if err := it.loadNextDataBlk(); err != nil {
		it.setErr(err)
		return
	}
	it.Next()
}

// Next moves the SstFileIterator to the next key.
func (it *SstFileIterator) Next() {
	if it.dataBlockIter.end() {
		if err := it.loadNextDataBlk(); err != nil {
			it.setErr(err)
			return
		}
	}

	it.dataBlockIter.Next()
}

// Key returns the key associated with the current SstFileIterator
func (it *SstFileIterator) Key() InternalKey {
	var ikey InternalKey
	ikey.Decode(it.dataBlockIter.Key())
	return ikey
}

// Value returns the value associated with the current SstFileIterator
func (it *SstFileIterator) Value() []byte {
	return it.dataBlockIter.Value()
}

// Valid returns whether the SstFileIterator is exhausted.
func (it *SstFileIterator) Valid() bool {
	return !it.invalid
}

// Err returns the SstFileIterator err
func (it *SstFileIterator) Err() error {
	return it.err
}

func (it *SstFileIterator) loadNextDataBlk() error {
	var err error

	if it.indexBlockIter.end() {
		return errEnd
	}

	it.indexBlockIter.Next()
	var handle blockHandle
	handle.Decode(it.indexBlockIter.Value())

	it.checkReadBufSize(handle.Size + blockTrailerSize)
	if _, err = it.f.ReadAt(it.readBuf, int64(handle.Offset)); err != nil {
		return err
	}
	if it.dataBuf, err = it.decompressBlock(it.dataBuf, it.readBuf); err != nil {
		return err
	}
	it.dataBlockIter.Reset(it.dataBuf)

	return nil
}

func (it *SstFileIterator) checkReadBufSize(sz uint64) {
	if uint64(cap(it.readBuf)) < sz {
		it.readBuf = make([]byte, sz)
		return
	}
	it.readBuf = it.readBuf[:sz]
}

func (it *SstFileIterator) decompressBlock(dst, raw []byte) ([]byte, error) {
	trailerPos := len(raw) - blockTrailerSize

	blkData := raw[:trailerPos]
	compressTp := CompressionType(raw[trailerPos])

	switch it.checksumType {
	case ChecksumCRC32:
		crc := newCrc32()
		crc.Write(raw[:trailerPos+1])
		sum := crc.Sum32()
		expected := unmaskCrc32(rocksEndian.Uint32(raw[trailerPos+1:]))
		if expected != sum {
			return nil, ErrChecksumMismatch
		}
	case ChecksumXXHash:
		panic("unsupported")
	}

	return DecompressBlock(compressTp, blkData, dst)
}

func (it *SstFileIterator) getIndexBlockHandle() (blockHandle, error) {
	var handle blockHandle

	footer, err := it.loadFooter()
	if err != nil {
		return handle, err
	}

	// Skip meta index handle
	n := handle.Decode(footer[1:])
	handle.Decode(footer[1+n:])
	return handle, nil
}

func (it *SstFileIterator) loadFooter() ([]byte, error) {
	fi, err := it.f.Stat()
	if err != nil {
		return nil, err
	}

	off := fi.Size() - footerEncodedLength
	var footerBuf [footerEncodedLength]byte
	if _, err = it.f.ReadAt(footerBuf[:], off); err != nil {
		return nil, err
	}

	if !it.checkMagicNumber(footerBuf[:]) {
		return nil, ErrMagicNumberMismatch
	}
	it.checksumType = ChecksumType(footerBuf[0])

	return footerBuf[:], nil
}

func (it *SstFileIterator) checkMagicNumber(footer []byte) bool {
	pos := footerEncodedLength - 8
	if rocksEndian.Uint32(footer[pos:]) != blockBasedTableMagicNumber&0xffffffff {
		return false
	}
	pos += 4
	return rocksEndian.Uint32(footer[pos:]) == blockBasedTableMagicNumber>>32
}

func (it *SstFileIterator) loadIndexBlock() error {
	handle, err := it.getIndexBlockHandle()
	if err != nil {
		return err
	}

	indexBlkData := make([]byte, handle.Size+blockTrailerSize)
	if _, err = it.f.ReadAt(indexBlkData, int64(handle.Offset)); err != nil {
		return err
	}
	if indexBlkData, err = it.decompressBlock(nil, indexBlkData); err != nil {
		return err
	}
	it.indexBlockIter = newBlockIterator(indexBlkData)

	return nil
}

func (it *SstFileIterator) setErr(err error) {
	if err != errEnd {
		it.err = err
	}
	it.invalid = true
}

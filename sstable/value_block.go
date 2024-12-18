// Copyright 2022 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"context"
	"math/rand/v2"
	"unsafe"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/invariants"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider/objiotracing"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/cockroachdb/pebble/sstable/valblk"
)

const valueBlocksIndexHandleMaxLen = blockHandleMaxLenWithoutProperties + 3

// Assert blockHandleLikelyMaxLen >= valueBlocksIndexHandleMaxLen.
const _ = uint(blockHandleLikelyMaxLen - valueBlocksIndexHandleMaxLen)

// Assert blockHandleLikelyMaxLen >= valblk.HandleMaxLen.
const _ = uint(blockHandleLikelyMaxLen - valblk.HandleMaxLen)

// UserKeyPrefixBound represents a [Lower,Upper) bound of user key prefixes.
// If both are nil, there is no bound specified. Else, Compare(Lower,Upper)
// must be < 0.
type UserKeyPrefixBound struct {
	// Lower is a lower bound user key prefix.
	Lower []byte
	// Upper is an upper bound user key prefix.
	Upper []byte
}

// IsEmpty returns true iff the bound is empty.
func (ukb *UserKeyPrefixBound) IsEmpty() bool {
	return len(ukb.Lower) == 0 && len(ukb.Upper) == 0
}

type blockProviderWhenOpen interface {
	readBlockForVBR(
		h block.Handle, stats *base.InternalIteratorStats,
	) (block.BufferHandle, error)
}

type blockProviderWhenClosed struct {
	rp ReaderProvider
	r  *Reader
}

func (bpwc *blockProviderWhenClosed) open(ctx context.Context) error {
	var err error
	bpwc.r, err = bpwc.rp.GetReader(ctx)
	return err
}

func (bpwc *blockProviderWhenClosed) close() {
	bpwc.rp.Close()
	bpwc.r = nil
}

func (bpwc blockProviderWhenClosed) readBlockForVBR(
	h block.Handle, stats *base.InternalIteratorStats,
) (block.BufferHandle, error) {
	// This is rare, since most block reads happen when the corresponding
	// sstable iterator is open. So we are willing to sacrifice a proper context
	// for tracing.
	//
	// TODO(sumeer): consider fixing this. See
	// https://github.com/cockroachdb/pebble/pull/3065#issue-1991175365 for an
	// alternative.
	ctx := objiotracing.WithBlockType(context.Background(), objiotracing.ValueBlock)
	// TODO(jackson,sumeer): Consider whether to use a buffer pool in this case.
	// The bpwc is not allowed to outlive the iterator tree, so it cannot
	// outlive the buffer pool.
	return bpwc.r.readValueBlock(ctx, noEnv, noReadHandle, h)
}

// ReaderProvider supports the implementation of blockProviderWhenClosed.
// GetReader and Close can be called multiple times in pairs.
type ReaderProvider interface {
	GetReader(ctx context.Context) (r *Reader, err error)
	Close()
}

// MakeTrivialReaderProvider creates a ReaderProvider which always returns the
// given reader. It should be used when the Reader will outlive the iterator
// tree.
func MakeTrivialReaderProvider(r *Reader) ReaderProvider {
	return (*trivialReaderProvider)(r)
}

// trivialReaderProvider implements ReaderProvider for a Reader that will
// outlive the top-level iterator in the iterator tree.
//
// Defining the type in this manner (as opposed to a struct) avoids allocation.
type trivialReaderProvider Reader

var _ ReaderProvider = (*trivialReaderProvider)(nil)

// GetReader implements ReaderProvider.
func (trp *trivialReaderProvider) GetReader(ctx context.Context) (*Reader, error) {
	return (*Reader)(trp), nil
}

// Close implements ReaderProvider.
func (trp *trivialReaderProvider) Close() {}

// valueBlockReader implements GetLazyValueForPrefixAndValueHandler; it is used
// to create LazyValues (each of which can can be used to retrieve a value in a
// value block). It is used when the sstable was written with
// Properties.ValueBlocksAreEnabled. The lifetime of this object is tied to the
// lifetime of the sstable iterator.
type valueBlockReader struct {
	bpOpen blockProviderWhenOpen
	rp     ReaderProvider
	vbih   valblk.IndexHandle
	stats  *base.InternalIteratorStats

	// fetcher is allocated lazily the first time we create a LazyValue, in order
	// to avoid the allocation if we never read a lazy value (which should be the
	// case when we're reading the latest value of a key).
	fetcher *valueBlockFetcher
}

var _ block.GetLazyValueForPrefixAndValueHandler = (*valueBlockReader)(nil)

func (r *valueBlockReader) GetLazyValueForPrefixAndValueHandle(handle []byte) base.LazyValue {
	if r.fetcher == nil {
		// NB: we cannot avoid this allocation, since the valueBlockFetcher
		// can outlive the singleLevelIterator due to be being embedded in a
		// LazyValue.
		//
		// TODO(radu): since it is a relatively small object, we could allocate
		// multiple instances together, using a sync.Pool (each pool object would
		// contain an array of instances, a subset of which have been given out).
		r.fetcher = newValueBlockFetcher(r.bpOpen, r.rp, r.vbih, r.stats)
	}
	lazyFetcher := &r.fetcher.lazyFetcher
	valLen, h := valblk.DecodeLenFromHandle(handle[1:])
	*lazyFetcher = base.LazyFetcher{
		Fetcher: r.fetcher,
		Attribute: base.AttributeAndLen{
			ValueLen:       int32(valLen),
			ShortAttribute: block.ValuePrefix(handle[0]).ShortAttribute(),
		},
	}
	if r.stats != nil {
		r.stats.SeparatedPointValue.Count++
		r.stats.SeparatedPointValue.ValueBytes += uint64(valLen)
	}
	return base.LazyValue{
		ValueOrHandle: h,
		Fetcher:       lazyFetcher,
	}
}

func (r *valueBlockReader) close() {
	r.bpOpen = nil
	if r.fetcher != nil {
		r.fetcher.close()
		r.fetcher = nil
	}
}

// valueBlockFetcher implements base.ValueFetcher and is used through LazyValue
// to fetch a value from a value block. The lifetime of this object is not tied
// to the lifetime of the iterator - a LazyValue can be accessed later.
type valueBlockFetcher struct {
	bpOpen blockProviderWhenOpen
	rp     ReaderProvider
	vbih   valblk.IndexHandle
	stats  *base.InternalIteratorStats
	// The value blocks index is lazily retrieved the first time the reader
	// needs to read a value that resides in a value block.
	vbiBlock []byte
	vbiCache block.BufferHandle
	// When sequentially iterating through all key-value pairs, the cost of
	// repeatedly getting a block that is already in the cache and releasing the
	// bufferHandle can be ~40% of the cpu overhead. So the reader remembers the
	// last value block it retrieved, in case there is locality of access, and
	// this value block can be used for the next value retrieval.
	valueBlockNum uint32
	valueBlock    []byte
	valueBlockPtr unsafe.Pointer
	valueCache    block.BufferHandle
	closed        bool
	bufToMangle   []byte

	// lazyFetcher is the LazyFetcher value embedded in any LazyValue that we
	// return. It is used to avoid having a separate allocation for that.
	lazyFetcher base.LazyFetcher
}

var _ base.ValueFetcher = (*valueBlockFetcher)(nil)

func newValueBlockFetcher(
	bpOpen blockProviderWhenOpen,
	rp ReaderProvider,
	vbih valblk.IndexHandle,
	stats *base.InternalIteratorStats,
) *valueBlockFetcher {
	return &valueBlockFetcher{
		bpOpen: bpOpen,
		rp:     rp,
		vbih:   vbih,
		stats:  stats,
	}
}

// Fetch implements base.ValueFetcher.
func (f *valueBlockFetcher) Fetch(
	ctx context.Context, handle []byte, valLen int32, buf []byte,
) (val []byte, callerOwned bool, err error) {
	if !f.closed {
		val, err := f.getValueInternal(handle, valLen)
		if invariants.Enabled {
			val = f.doValueMangling(val)
		}
		return val, false, err
	}

	bp := blockProviderWhenClosed{rp: f.rp}
	err = bp.open(ctx)
	if err != nil {
		return nil, false, err
	}
	defer bp.close()
	defer f.close()
	f.bpOpen = bp
	var v []byte
	v, err = f.getValueInternal(handle, valLen)
	if err != nil {
		return nil, false, err
	}
	buf = append(buf[:0], v...)
	return buf, true, nil
}

func (f *valueBlockFetcher) close() {
	f.vbiBlock = nil
	f.vbiCache.Release()
	// Set the handle to empty since Release does not nil the Handle.value. If
	// we were to reopen this valueBlockFetcher and retrieve the same
	// Handle.value from the cache, we don't want to accidentally unref it when
	// attempting to unref the old handle.
	f.vbiCache = block.BufferHandle{}
	f.valueBlock = nil
	f.valueBlockPtr = nil
	f.valueCache.Release()
	// See comment above.
	f.valueCache = block.BufferHandle{}
	f.closed = true
	// rp, vbih, stats remain valid, so that LazyFetcher.ValueFetcher can be
	// implemented.
}

// doValueMangling attempts to uncover violations of the contract listed in
// the declaration comment of LazyValue. It is expensive, hence only called
// when invariants.Enabled.
func (f *valueBlockFetcher) doValueMangling(v []byte) []byte {
	// Randomly set the bytes in the previous retrieved value to 0, since
	// property P1 only requires the valueBlockReader to maintain the memory of
	// one fetched value.
	if rand.IntN(2) == 0 {
		clear(f.bufToMangle)
	}
	// Store the current value in a new buffer for future mangling.
	f.bufToMangle = append([]byte(nil), v...)
	return f.bufToMangle
}

func (f *valueBlockFetcher) getValueInternal(handle []byte, valLen int32) (val []byte, err error) {
	vh := valblk.DecodeRemainingHandle(handle)
	vh.ValueLen = uint32(valLen)
	if f.vbiBlock == nil {
		ch, err := f.bpOpen.readBlockForVBR(f.vbih.Handle, f.stats)
		if err != nil {
			return nil, err
		}
		f.vbiCache = ch
		f.vbiBlock = ch.BlockData()
	}
	if f.valueBlock == nil || f.valueBlockNum != vh.BlockNum {
		vbh, err := f.getBlockHandle(vh.BlockNum)
		if err != nil {
			return nil, err
		}
		vbCacheHandle, err := f.bpOpen.readBlockForVBR(vbh, f.stats)
		if err != nil {
			return nil, err
		}
		f.valueBlockNum = vh.BlockNum
		f.valueCache.Release()
		f.valueCache = vbCacheHandle
		f.valueBlock = vbCacheHandle.BlockData()
		f.valueBlockPtr = unsafe.Pointer(&f.valueBlock[0])
	}
	if f.stats != nil {
		f.stats.SeparatedPointValue.ValueBytesFetched += uint64(valLen)
	}
	return f.valueBlock[vh.OffsetInBlock : vh.OffsetInBlock+vh.ValueLen], nil
}

func (f *valueBlockFetcher) getBlockHandle(blockNum uint32) (block.Handle, error) {
	indexEntryLen :=
		int(f.vbih.BlockNumByteLength + f.vbih.BlockOffsetByteLength + f.vbih.BlockLengthByteLength)
	offsetInIndex := indexEntryLen * int(blockNum)
	if len(f.vbiBlock) < offsetInIndex+indexEntryLen {
		return block.Handle{}, base.AssertionFailedf(
			"index entry out of bounds: offset %d length %d block length %d",
			offsetInIndex, indexEntryLen, len(f.vbiBlock))
	}
	b := f.vbiBlock[offsetInIndex : offsetInIndex+indexEntryLen]
	n := int(f.vbih.BlockNumByteLength)
	bn := littleEndianGet(b, n)
	if uint32(bn) != blockNum {
		return block.Handle{},
			errors.Errorf("expected block num %d but found %d", blockNum, bn)
	}
	b = b[n:]
	n = int(f.vbih.BlockOffsetByteLength)
	blockOffset := littleEndianGet(b, n)
	b = b[n:]
	n = int(f.vbih.BlockLengthByteLength)
	blockLen := littleEndianGet(b, n)
	return block.Handle{Offset: blockOffset, Length: blockLen}, nil
}

func littleEndianGet(b []byte, n int) uint64 {
	_ = b[n-1] // bounds check
	v := uint64(b[0])
	for i := 1; i < n; i++ {
		v |= uint64(b[i]) << (8 * i)
	}
	return v
}

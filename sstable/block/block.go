// Copyright 2024 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package block

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"runtime"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/cockroachdb/crlib/crtime"
	"github.com/cockroachdb/crlib/fifo"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/cache"
	"github.com/cockroachdb/pebble/internal/crc"
	"github.com/cockroachdb/pebble/internal/invariants"
	"github.com/cockroachdb/pebble/internal/sstableinternal"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
)

// Handle is the file offset and length of a block.
type Handle struct {
	Offset, Length uint64
}

// EncodeVarints encodes the block handle into dst using a variable-width
// encoding and returns the number of bytes written.
func (h Handle) EncodeVarints(dst []byte) int {
	n := binary.PutUvarint(dst, h.Offset)
	m := binary.PutUvarint(dst[n:], h.Length)
	return n + m
}

// HandleWithProperties is used for data blocks and first/lower level index
// blocks, since they can be annotated using BlockPropertyCollectors.
type HandleWithProperties struct {
	Handle
	Props []byte
}

// EncodeVarints encodes the block handle and properties into dst using a
// variable-width encoding and returns the number of bytes written.
func (h HandleWithProperties) EncodeVarints(dst []byte) []byte {
	n := h.Handle.EncodeVarints(dst)
	dst = append(dst[:n], h.Props...)
	return dst
}

// DecodeHandle returns the block handle encoded in a variable-width encoding at
// the start of src, as well as the number of bytes it occupies. It returns zero
// if given invalid input. A block handle for a data block or a first/lower
// level index block should not be decoded using DecodeHandle since the caller
// may validate that the number of bytes decoded is equal to the length of src,
// which will be false if the properties are not decoded. In those cases the
// caller should use DecodeHandleWithProperties.
func DecodeHandle(src []byte) (Handle, int) {
	offset, n := binary.Uvarint(src)
	length, m := binary.Uvarint(src[n:])
	if n == 0 || m == 0 {
		return Handle{}, 0
	}
	return Handle{Offset: offset, Length: length}, n + m
}

// DecodeHandleWithProperties returns the block handle and properties encoded in
// a variable-width encoding at the start of src. src needs to be exactly the
// length that was encoded. This method must be used for data block and
// first/lower level index blocks. The properties in the block handle point to
// the bytes in src.
func DecodeHandleWithProperties(src []byte) (HandleWithProperties, error) {
	bh, n := DecodeHandle(src)
	if n == 0 {
		return HandleWithProperties{}, errors.Errorf("invalid block.Handle")
	}
	return HandleWithProperties{
		Handle: bh,
		Props:  src[n:],
	}, nil
}

// TrailerLen is the length of the trailer at the end of a block.
const TrailerLen = 5

// Trailer is the trailer at the end of a block, encoding the block type
// (compression) and a checksum.
type Trailer = [TrailerLen]byte

// MakeTrailer constructs a trailer from a block type and a checksum.
func MakeTrailer(blockType byte, checksum uint32) (t Trailer) {
	t[0] = blockType
	binary.LittleEndian.PutUint32(t[1:5], checksum)
	return t
}

// ChecksumType specifies the checksum used for blocks.
type ChecksumType byte

// The available checksum types. These values are part of the durable format and
// should not be changed.
const (
	ChecksumTypeNone     ChecksumType = 0
	ChecksumTypeCRC32c   ChecksumType = 1
	ChecksumTypeXXHash   ChecksumType = 2
	ChecksumTypeXXHash64 ChecksumType = 3
)

// String implements fmt.Stringer.
func (t ChecksumType) String() string {
	switch t {
	case ChecksumTypeCRC32c:
		return "crc32c"
	case ChecksumTypeNone:
		return "none"
	case ChecksumTypeXXHash:
		return "xxhash"
	case ChecksumTypeXXHash64:
		return "xxhash64"
	default:
		panic(errors.Newf("sstable: unknown checksum type: %d", t))
	}
}

// A Checksummer calculates checksums for blocks.
type Checksummer struct {
	Type         ChecksumType
	xxHasher     *xxhash.Digest
	blockTypeBuf [1]byte
}

// Checksum computes a checksum over the provided block and block type.
func (c *Checksummer) Checksum(block []byte, blockType byte) (checksum uint32) {
	// Calculate the checksum.
	c.blockTypeBuf[0] = blockType
	switch c.Type {
	case ChecksumTypeCRC32c:
		checksum = crc.New(block).Update(c.blockTypeBuf[:]).Value()
	case ChecksumTypeXXHash64:
		if c.xxHasher == nil {
			c.xxHasher = xxhash.New()
		} else {
			c.xxHasher.Reset()
		}
		c.xxHasher.Write(block)
		c.xxHasher.Write(c.blockTypeBuf[:])
		checksum = uint32(c.xxHasher.Sum64())
	default:
		panic(errors.Newf("unsupported checksum type: %d", c.Type))
	}
	return checksum
}

// ValidateChecksum validates the checksum of a block.
func ValidateChecksum(checksumType ChecksumType, b []byte, bh Handle) error {
	expectedChecksum := binary.LittleEndian.Uint32(b[bh.Length+1:])
	var computedChecksum uint32
	switch checksumType {
	case ChecksumTypeCRC32c:
		computedChecksum = crc.New(b[:bh.Length+1]).Value()
	case ChecksumTypeXXHash64:
		computedChecksum = uint32(xxhash.Sum64(b[:bh.Length+1]))
	default:
		return errors.Errorf("unsupported checksum type: %d", checksumType)
	}
	if expectedChecksum != computedChecksum {
		return base.CorruptionErrorf("block %d/%d: %s checksum mismatch %x != %x",
			errors.Safe(bh.Offset), errors.Safe(bh.Length), checksumType,
			expectedChecksum, computedChecksum)
	}
	return nil
}

// Metadata is an in-memory buffer that stores metadata for a block. It is
// allocated together with the buffer storing the block and is initialized once
// when the block is read from disk.
//
// Portions of this buffer can be cast to the structures we need (through
// unsafe.Pointer), but note that any pointers in these structures will be
// invisible to the GC. Pointers to the block's data buffer are ok, since the
// metadata and the data have the same lifetime (sharing the underlying
// allocation).
type Metadata [MetadataSize]byte

// MetadataSize is the size of the metadata. The value is chosen to fit a
// colblk.DataBlockDecoder and a CockroachDB colblk.KeySeeker.
const MetadataSize = 336

// Assert that MetadataSize is a multiple of 8. This is necessary to keep the
// block data buffer aligned.
const _ uint = -(MetadataSize % 8)

// DataBlockIterator is a type constraint for implementations of block iterators
// over data blocks. It's implemented by *rowblk.Iter and *colblk.DataBlockIter.
type DataBlockIterator interface {
	base.InternalIterator

	// Handle returns the handle to the block.
	Handle() BufferHandle
	// InitHandle initializes the block from the provided buffer handle.
	//
	// The iterator takes ownership of the BufferHandle and releases it when it is
	// closed (or re-initialized with another handle). This happens even in error
	// cases.
	InitHandle(*base.Comparer, BufferHandle, IterTransforms) error
	// Valid returns true if the iterator is currently positioned at a valid KV.
	Valid() bool
	// KV returns the key-value pair at the current iterator position. The
	// iterator must be Valid().
	KV() *base.InternalKV
	// IsLowerBound returns true if all keys produced by this iterator are >= the
	// given key. The function is best effort; false negatives are allowed.
	//
	// If IsLowerBound is true then Compare(First().UserKey, k) >= 0.
	//
	// If the iterator produces no keys (i.e. First() is nil), IsLowerBound can
	// return true for any key.
	IsLowerBound(k []byte) bool
	// Invalidate invalidates the block iterator, removing references to the
	// block it was initialized with. The iterator may continue to be used after
	// a call to Invalidate, but all positioning methods should return false.
	// Valid() must also return false.
	Invalidate()
	// IsDataInvalidated returns true when the iterator has been invalidated
	// using an Invalidate call.
	//
	// NB: this is different from Valid which indicates whether the current *KV*
	// is valid.
	IsDataInvalidated() bool
}

// IndexBlockIterator is an interface for implementations of block iterators
// over index blocks. It's implemented by *rowblk.IndexIter and
// *colblk.IndexBlockIter.
type IndexBlockIterator interface {
	// Init initializes the block iterator from the provided block.
	Init(*base.Comparer, []byte, IterTransforms) error
	// InitHandle initializes an iterator from the provided block handle.
	//
	// The iterator takes ownership of the BufferHandle and releases it when it is
	// closed (or re-initialized with another handle). This happens even in error
	// cases.
	InitHandle(*base.Comparer, BufferHandle, IterTransforms) error
	// Valid returns true if the iterator is currently positioned at a valid
	// block handle.
	Valid() bool
	// IsDataInvalidated returns true when the iterator has been invalidated
	// using an Invalidate call.
	//
	// NB: this is different from Valid which indicates whether the iterator is
	// currently positioned over a valid block entry.
	IsDataInvalidated() bool
	// Invalidate invalidates the block iterator, removing references to the
	// block it was initialized with. The iterator may continue to be used after
	// a call to Invalidate, but all positioning methods should return false.
	// Valid() must also return false.
	Invalidate()
	// Handle returns the underlying block buffer handle, if the iterator was
	// initialized with one.
	Handle() BufferHandle
	// Separator returns the separator at the iterator's current position. The
	// iterator must be positioned at a valid row. A Separator is a user key
	// guaranteed to be greater than or equal to every key contained within the
	// referenced block(s).
	Separator() []byte
	// SeparatorLT returns true if the separator at the iterator's current
	// position is strictly less than the provided key. For some
	// implementations, it may be more performant to call SeparatorLT rather
	// than explicitly performing Compare(Separator(), key) < 0.
	SeparatorLT(key []byte) bool
	// SeparatorGT returns true if the separator at the iterator's current
	// position is strictly greater than (or equal, if orEqual=true) the
	// provided key. For some implementations, it may be more performant to call
	// SeparatorGT rather than explicitly performing a comparison using the key
	// returned by Separator.
	SeparatorGT(key []byte, orEqual bool) bool
	// BlockHandleWithProperties decodes the block handle with any encoded
	// properties at the iterator's current position.
	BlockHandleWithProperties() (HandleWithProperties, error)
	// SeekGE seeks the index iterator to the first block entry with a separator
	// key greater or equal to the given key. If it returns true, the iterator
	// is positioned over the first block that might contain the key [key], and
	// following blocks have keys ≥ Separator(). It returns false if the seek
	// key is greater than all index block separators.
	SeekGE(key []byte) bool
	// First seeks index iterator to the first block entry. It returns false if
	// the index block is empty.
	First() bool
	// Last seeks index iterator to the last block entry. It returns false if
	// the index block is empty.
	Last() bool
	// Next steps the index iterator to the next block entry. It returns false
	// if the index block is exhausted in the forward direction. A call to Next
	// while already exhausted in the forward direction is a no-op.
	Next() bool
	// Prev steps the index iterator to the previous block entry. It returns
	// false if the index block is exhausted in the reverse direction. A call to
	// Prev while already exhausted in the reverse direction is a no-op.
	Prev() bool
	// Close closes the iterator, releasing any resources it holds. After Close,
	// the iterator must be reset such that it could be reused after a call to
	// Init or InitHandle.
	Close() error
}

// NoReadEnv is the empty ReadEnv which reports no stats and does not use a
// buffer pool.
var NoReadEnv = ReadEnv{}

// ReadEnv contains arguments used when reading a block which apply to all
// the block reads performed by a higher-level operation.
type ReadEnv struct {
	// stats and iterStats are slightly different. stats is a shared struct
	// supplied from the outside, and represents stats for the whole iterator
	// tree and can be reset from the outside (e.g. when the pebble.Iterator is
	// being reused). It is currently only provided when the iterator tree is
	// rooted at pebble.Iterator. iterStats contains an sstable iterator's
	// private stats that are reported to a CategoryStatsCollector when this
	// iterator is closed. In the important code paths, the CategoryStatsCollector
	// is managed by the fileCacheContainer.
	Stats     *base.InternalIteratorStats
	IterStats *ChildIterStatsAccumulator

	// BufferPool is not-nil if we read blocks into a buffer pool and not into the
	// cache. This is used during compactions.
	BufferPool *BufferPool
}

// BlockServedFromCache updates the stats when a block was found in the cache.
func (env *ReadEnv) BlockServedFromCache(blockLength uint64) {
	if env.Stats != nil {
		env.Stats.BlockBytes += blockLength
		env.Stats.BlockBytesInCache += blockLength
	}
	if env.IterStats != nil {
		env.IterStats.Accumulate(blockLength, blockLength, 0)
	}
}

// BlockRead updates the stats when a block had to be read.
func (env *ReadEnv) BlockRead(blockLength uint64, readDuration time.Duration) {
	if env.Stats != nil {
		env.Stats.BlockBytes += blockLength
		env.Stats.BlockReadDuration += readDuration
	}
	if env.IterStats != nil {
		env.IterStats.Accumulate(blockLength, 0, readDuration)
	}
}

// A Reader reads blocks from a single file, handling caching, checksum
// validation and decompression.
type Reader struct {
	readable      objstorage.Readable
	cacheOpts     sstableinternal.CacheOptions
	loadBlockSema *fifo.Semaphore
	logger        base.LoggerAndTracer
	checksumType  ChecksumType
}

// Init initializes the Reader to read blocks from the provided Readable.
func (r *Reader) Init(
	readable objstorage.Readable,
	cacheOpts sstableinternal.CacheOptions,
	sema *fifo.Semaphore,
	logger base.LoggerAndTracer,
	checksumType ChecksumType,
) {
	r.readable = readable
	r.cacheOpts = cacheOpts
	r.loadBlockSema = sema
	r.logger = logger
	r.checksumType = checksumType
	if r.cacheOpts.Cache == nil {
		r.cacheOpts.Cache = cache.New(0)
	} else {
		r.cacheOpts.Cache.Ref()
	}
	if r.cacheOpts.CacheID == 0 {
		r.cacheOpts.CacheID = r.cacheOpts.Cache.NewID()
	}
}

// FileNum returns the file number of the file being read.
func (r *Reader) FileNum() base.DiskFileNum {
	return r.cacheOpts.FileNum
}

// ChecksumType returns the checksum type used by the reader.
func (r *Reader) ChecksumType() ChecksumType {
	return r.checksumType
}

// Read reads the block referenced by the provided handle. The readHandle is
// optional.
func (r *Reader) Read(
	ctx context.Context,
	env ReadEnv,
	readHandle objstorage.ReadHandle,
	bh Handle,
	initBlockMetadataFn func(*Metadata, []byte) error,
) (handle BufferHandle, _ error) {
	var cv *cache.Value
	var crh cache.ReadHandle
	hit := true
	if env.BufferPool == nil {
		var errorDuration time.Duration
		var err error
		cv, crh, errorDuration, hit, err = r.cacheOpts.Cache.GetWithReadHandle(
			ctx, r.cacheOpts.CacheID, r.cacheOpts.FileNum, bh.Offset)
		if errorDuration > 5*time.Millisecond && r.logger.IsTracingEnabled(ctx) {
			r.logger.Eventf(
				ctx, "waited for turn when %s time wasted by failed reads", errorDuration.String())
		}
		// TODO(sumeer): consider tracing when waited longer than some duration
		// for turn to do the read.
		if err != nil {
			return BufferHandle{}, err
		}
	} else {
		// The compaction path uses env.BufferPool, and does not coordinate read
		// using a cache.ReadHandle. This is ok since only a single compaction is
		// reading a block.
		cv = r.cacheOpts.Cache.Get(r.cacheOpts.CacheID, r.cacheOpts.FileNum, bh.Offset)
		if cv != nil {
			hit = true
		}
	}
	// INVARIANT: hit => cv != nil
	if cv != nil {
		if hit {
			// Cache hit.
			if readHandle != nil {
				readHandle.RecordCacheHit(ctx, int64(bh.Offset), int64(bh.Length+TrailerLen))
			}
			env.BlockServedFromCache(bh.Length)
		}
		if invariants.Enabled && crh.Valid() {
			panic("cache.ReadHandle must not be valid")
		}
		return CacheBufferHandle(cv), nil
	}

	// Need to read. First acquire loadBlockSema, if needed.
	if sema := r.loadBlockSema; sema != nil {
		if err := sema.Acquire(ctx, 1); err != nil {
			// An error here can only come from the context.
			return BufferHandle{}, err
		}
		defer sema.Release(1)
	}
	value, err := r.doRead(ctx, env, readHandle, bh, initBlockMetadataFn)
	if err != nil {
		if crh.Valid() {
			crh.SetReadError(err)
		}
		return BufferHandle{}, err
	}
	h := value.MakeHandle(crh, r.cacheOpts.CacheID, r.cacheOpts.FileNum, bh.Offset)
	return h, nil
}

// TODO(sumeer): should the threshold be configurable.
const slowReadTracingThreshold = 5 * time.Millisecond

// doRead is a helper for Read that does the read, checksum check,
// decompression, and returns either a Value or an error.
func (r *Reader) doRead(
	ctx context.Context,
	env ReadEnv,
	readHandle objstorage.ReadHandle,
	bh Handle,
	initBlockMetadataFn func(*Metadata, []byte) error,
) (Value, error) {
	compressed := Alloc(int(bh.Length+TrailerLen), env.BufferPool)
	readStopwatch := makeStopwatch()
	var err error
	if readHandle != nil {
		err = readHandle.ReadAt(ctx, compressed.BlockData(), int64(bh.Offset))
	} else {
		err = r.readable.ReadAt(ctx, compressed.BlockData(), int64(bh.Offset))
	}
	readDuration := readStopwatch.stop()
	// Call IsTracingEnabled to avoid the allocations of boxing integers into an
	// interface{}, unless necessary.
	if readDuration >= slowReadTracingThreshold && r.logger.IsTracingEnabled(ctx) {
		_, file1, line1, _ := runtime.Caller(1)
		_, file2, line2, _ := runtime.Caller(2)
		r.logger.Eventf(ctx, "reading block of %d bytes took %s (fileNum=%s; %s/%s:%d -> %s/%s:%d)",
			int(bh.Length+TrailerLen), readDuration.String(),
			r.cacheOpts.FileNum,
			filepath.Base(filepath.Dir(file2)), filepath.Base(file2), line2,
			filepath.Base(filepath.Dir(file1)), filepath.Base(file1), line1)
	}
	if err != nil {
		compressed.Release()
		return Value{}, err
	}
	env.BlockRead(bh.Length, readDuration)
	if err = ValidateChecksum(r.checksumType, compressed.BlockData(), bh); err != nil {
		compressed.Release()
		err = errors.Wrapf(err, "pebble/table: table %s", r.cacheOpts.FileNum)
		return Value{}, err
	}
	typ := CompressionIndicator(compressed.BlockData()[bh.Length])
	compressed.Truncate(int(bh.Length))
	var decompressed Value
	if typ == NoCompressionIndicator {
		decompressed = compressed
	} else {
		// Decode the length of the decompressed value.
		decodedLen, prefixLen, err := DecompressedLen(typ, compressed.BlockData())
		if err != nil {
			compressed.Release()
			return Value{}, err
		}
		decompressed = Alloc(decodedLen, env.BufferPool)
		err = DecompressInto(typ, compressed.BlockData()[prefixLen:], decompressed.BlockData())
		compressed.Release()
		if err != nil {
			decompressed.Release()
			return Value{}, err
		}
	}
	if err = initBlockMetadataFn(decompressed.BlockMetadata(), decompressed.BlockData()); err != nil {
		decompressed.Release()
		return Value{}, err
	}
	return decompressed, nil
}

// Readable returns the underlying objstorage.Readable.
//
// Users should avoid accessing the underlying Readable if it can be avoided.
func (r *Reader) Readable() objstorage.Readable {
	return r.readable
}

// GetFromCache retrieves the block from the cache, if it is present.
//
// Users should prefer using Read, which handles reading from object storage on
// a cache miss.
func (r *Reader) GetFromCache(bh Handle) *cache.Value {
	return r.cacheOpts.Cache.Get(r.cacheOpts.CacheID, r.cacheOpts.FileNum, bh.Offset)
}

// UsePreallocatedReadHandle returns a ReadHandle that reads from the reader and
// uses the provided preallocated read handle to back the read handle, avoiding
// an unnecessary allocation.
func (r *Reader) UsePreallocatedReadHandle(
	readBeforeSize objstorage.ReadBeforeSize, rh *objstorageprovider.PreallocatedReadHandle,
) objstorage.ReadHandle {
	return objstorageprovider.UsePreallocatedReadHandle(r.readable, readBeforeSize, rh)
}

// Close releases resources associated with the Reader.
func (r *Reader) Close() error {
	r.cacheOpts.Cache.Unref()
	var err error
	if r.readable != nil {
		err = r.readable.Close()
		r.readable = nil
	}
	return err
}

// ReadRaw reads len(buf) bytes from the provided Readable at the given offset
// into buf. It's used to read the footer of a table.
func ReadRaw(
	ctx context.Context,
	f objstorage.Readable,
	readHandle objstorage.ReadHandle,
	logger base.LoggerAndTracer,
	fileNum base.DiskFileNum,
	buf []byte,
	off int64,
) ([]byte, error) {
	size := f.Size()
	if size < int64(len(buf)) {
		return nil, base.CorruptionErrorf("pebble/table: invalid table %s (file size is too small)", errors.Safe(fileNum))
	}

	readStopwatch := makeStopwatch()
	var err error
	if readHandle != nil {
		err = readHandle.ReadAt(ctx, buf, off)
	} else {
		err = f.ReadAt(ctx, buf, off)
	}
	readDuration := readStopwatch.stop()
	// Call IsTracingEnabled to avoid the allocations of boxing integers into an
	// interface{}, unless necessary.
	if readDuration >= slowReadTracingThreshold && logger.IsTracingEnabled(ctx) {
		logger.Eventf(ctx, "reading footer of %d bytes took %s",
			len(buf), readDuration.String())
	}
	if err != nil {
		return nil, errors.Wrap(err, "pebble/table: invalid table (could not read footer)")
	}
	return buf, nil
}

// DeterministicReadBlockDurationForTesting is for tests that want a
// deterministic value of the time to read a block (that is not in the cache).
// The return value is a function that must be called before the test exits.
func DeterministicReadBlockDurationForTesting() func() {
	drbdForTesting := deterministicReadBlockDurationForTesting
	deterministicReadBlockDurationForTesting = true
	return func() {
		deterministicReadBlockDurationForTesting = drbdForTesting
	}
}

var deterministicReadBlockDurationForTesting = false

type deterministicStopwatchForTesting struct {
	startTime crtime.Mono
}

func makeStopwatch() deterministicStopwatchForTesting {
	return deterministicStopwatchForTesting{startTime: crtime.NowMono()}
}

func (w deterministicStopwatchForTesting) stop() time.Duration {
	dur := w.startTime.Elapsed()
	if deterministicReadBlockDurationForTesting {
		dur = slowReadTracingThreshold
	}
	return dur
}

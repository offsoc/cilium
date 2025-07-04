// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium
// Copyright The Kubernetes Authors.

package allocator

import (
	"errors"
	"math/big"
	"math/rand/v2"

	"github.com/cilium/cilium/pkg/lock"
)

// AllocationBitmap is a contiguous block of resources that can be allocated atomically.
//
// Each resource has an offset.  The internal structure is a bitmap, with a bit for each offset.
//
// If a resource is taken, the bit at that offset is set to one.
// r.count is always equal to the number of set bits and can be recalculated at any time
// by counting the set bits in r.allocated.
//
// TODO: use RLE and compact the allocator to minimize space.
type AllocationBitmap struct {
	// strategy carries the details of how to choose the next available item out of the range
	strategy bitAllocator
	// max is the maximum size of the usable items in the range
	max int
	// rangeSpec is the range specifier, matching RangeAllocation.Range
	rangeSpec string

	// lock guards the following members
	lock lock.Mutex
	// count is the number of currently allocated elements in the range
	count int
	// allocated is a bit array of the allocated items in the range
	allocated *big.Int
}

// AllocationBitmap implements Interface and Snapshottable
var _ Interface = &AllocationBitmap{}
var _ Snapshottable = &AllocationBitmap{}

// bitAllocator represents a search strategy in the allocation map for a valid item.
type bitAllocator interface {
	AllocateBit(allocated *big.Int, max, count int) (int, bool)
}

// NewAllocationMap creates an allocation bitmap using the random scan strategy.
func NewAllocationMap(max int, rangeSpec string) *AllocationBitmap {
	a := AllocationBitmap{
		strategy:  randomScanStrategy{},
		allocated: big.NewInt(0),
		count:     0,
		max:       max,
		rangeSpec: rangeSpec,
	}
	return &a
}

// NewContiguousAllocationMap creates an allocation bitmap using the contiguous scan strategy.
func NewContiguousAllocationMap(max int, rangeSpec string) *AllocationBitmap {
	a := AllocationBitmap{
		strategy:  contiguousScanStrategy{},
		allocated: big.NewInt(0),
		count:     0,
		max:       max,
		rangeSpec: rangeSpec,
	}
	return &a
}

// Allocate attempts to reserve the provided item.
// Returns true if it was allocated, false if it was already in use
func (r *AllocationBitmap) Allocate(offset int) bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.allocated.Bit(offset) == 1 {
		return false
	}
	r.allocated = r.allocated.SetBit(r.allocated, offset, 1)
	r.count++
	return true
}

// AllocateNext reserves one of the items from the pool.
// (0, false, nil) may be returned if there are no items left.
func (r *AllocationBitmap) AllocateNext() (int, bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	next, ok := r.strategy.AllocateBit(r.allocated, r.max, r.count)
	if !ok {
		return 0, false
	}
	r.count++
	r.allocated = r.allocated.SetBit(r.allocated, next, 1)
	return next, true
}

// Release releases the item back to the pool. Releasing an
// unallocated item or an item out of the range is a no-op and
// returns no error.
func (r *AllocationBitmap) Release(offset int) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.allocated.Bit(offset) == 0 {
		return
	}

	r.allocated = r.allocated.SetBit(r.allocated, offset, 0)
	r.count--
}

const (
	// Find the size of a big.Word in bytes.
	notZero   = uint64(^big.Word(0))
	wordPower = (notZero>>8)&1 + (notZero>>16)&1 + (notZero>>32)&1
	wordSize  = 1 << wordPower
)

// ForEach calls the provided function for each allocated bit.  The
// AllocationBitmap may not be modified while this loop is running.
func (r *AllocationBitmap) ForEach(fn func(int)) {
	r.lock.Lock()
	defer r.lock.Unlock()

	words := r.allocated.Bits()
	for wordIdx, word := range words {
		bit := 0
		for word > 0 {
			if (word & 1) != 0 {
				fn((wordIdx * wordSize * 8) + bit)
				word = word &^ 1
			}
			bit++
			word = word >> 1
		}
	}
}

// Has returns true if the provided item is already allocated and a call
// to Allocate(offset) would fail.
func (r *AllocationBitmap) Has(offset int) bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.allocated.Bit(offset) == 1
}

// Free returns the count of items left in the range.
func (r *AllocationBitmap) Free() int {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.max - r.count
}

// Snapshot saves the current state of the pool.
func (r *AllocationBitmap) Snapshot() (string, []byte) {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.rangeSpec, r.allocated.Bytes()
}

// Restore restores the pool to the previously captured state.
func (r *AllocationBitmap) Restore(rangeSpec string, data []byte) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.rangeSpec != rangeSpec {
		return errors.New("the provided range does not match the current range")
	}

	r.allocated = big.NewInt(0).SetBytes(data)
	r.count = countBits(r.allocated)

	return nil
}

// randomScanStrategy chooses a random address from the provided big.Int, and then
// scans forward looking for the next available address (it will wrap the range if
// necessary).
type randomScanStrategy struct{}

func (rss randomScanStrategy) AllocateBit(allocated *big.Int, max, count int) (int, bool) {
	if count >= max {
		return 0, false
	}
	offset := rand.IntN(max)
	for i := range max {
		at := (offset + i) % max
		if allocated.Bit(at) == 0 {
			return at, true
		}
	}
	return 0, false
}

var _ bitAllocator = randomScanStrategy{}

// contiguousScanStrategy tries to allocate starting at 0 and filling in any gaps
type contiguousScanStrategy struct{}

func (contiguousScanStrategy) AllocateBit(allocated *big.Int, max, count int) (int, bool) {
	if count >= max {
		return 0, false
	}
	for i := range max {
		if allocated.Bit(i) == 0 {
			return i, true
		}
	}
	return 0, false
}

var _ bitAllocator = contiguousScanStrategy{}

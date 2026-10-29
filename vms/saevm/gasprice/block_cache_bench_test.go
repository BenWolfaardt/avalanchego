// Copyright (C) 2019, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"container/heap"
	"fmt"
	"math/big"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/avalanchego/cache/lru"
	"github.com/ava-labs/avalanchego/vms/saevm/saetest"

	saeparams "github.com/ava-labs/avalanchego/vms/saevm/params"
)

// maxTransfersPerBlock is the number of signed EIP-1559 transfers that fit in
// [saeparams.TargetBlockBytes], which binds before gas once the target grows.
func maxTransfersPerBlock(tb testing.TB) int {
	tb.Helper()
	config := saetest.ChainConfig()
	kc := saetest.NewUNSAFEKeyChain(tb, 1)
	to := common.HexToAddress("0x8db97C7cEcE249c2b98bDC0226Cc4C2A57BF52FC")
	tx := kc.SignTx(tb, types.LatestSigner(config), 0, &types.DynamicFeeTx{
		ChainID:   config.ChainID,
		Nonce:     1_000,
		To:        &to,
		Value:     big.NewInt(avax),
		Gas:       params.TxGas,
		GasTipCap: big.NewInt(nAVAX),
		GasFeeCap: big.NewInt(50 * nAVAX),
	})
	return int(saeparams.TargetBlockBytes / tx.Size())
}

func benchTxCounts(tb testing.TB) []int {
	tb.Helper()
	ceiling := maxTransfersPerBlock(tb)
	counts := []int{100, 1_000}
	if ceiling > 10_000 {
		counts = append(counts, 10_000)
	}
	return append(counts, ceiling)
}

func randomTip(rng *rand.Rand) uint64 {
	return rng.Uint64N(100*nAVAX) + 1
}

func newRandomBlock(rng *rand.Rand, numTxs int) *block {
	b := &block{
		baseFee: new(big.Int),
		txs:     make([]transaction, numTxs),
	}
	for i := range b.txs {
		b.txs[i] = transaction{
			gas: params.TxGas,
			tip: new(big.Int).SetUint64(randomTip(rng)),
		}
		b.gasUsed += params.TxGas
	}
	b.gasLimit = b.gasUsed
	slices.SortFunc(b.txs, transaction.Compare)
	return b
}

func newRandomTxs(rng *rand.Rand, numTxs int) []*types.Transaction {
	txs := make([]*types.Transaction, numTxs)
	for i := range txs {
		txs[i] = newDynamicFeeTx(params.TxGas, randomTip(rng))
	}
	return txs
}

func newRandomEthBlock(rng *rand.Rand, numTxs int) *types.Block {
	header := &types.Header{
		Number:   big.NewInt(1),
		BaseFee:  big.NewInt(1),
		GasUsed:  uint64(numTxs) * params.TxGas,
		GasLimit: uint64(numTxs) * params.TxGas,
	}
	return types.NewBlockWithHeader(header).WithBody(types.Body{
		Transactions: newRandomTxs(rng, numTxs),
	})
}

func evenPercentiles(n int) []float64 {
	percentiles := make([]float64, n)
	for i := range percentiles {
		percentiles[i] = 100 * float64(i+1) / float64(n+1)
	}
	return percentiles
}

func randomPercentiles(rng *rand.Rand) []float64 {
	percentiles := make([]float64, rng.IntN(10))
	for i := range percentiles {
		percentiles[i] = float64(rng.IntN(1_001)) / 10
	}
	slices.Sort(percentiles)
	return slices.Compact(percentiles)
}

// prefixBlock is the binary-search alternative to [block.tipPercentiles].
type prefixBlock struct {
	*block
	cumGas []uint64 // cumGas[i] is the summed gas of txs[0..i]
}

func withPrefixSums(b *block) *prefixBlock {
	cumGas := make([]uint64, len(b.txs))
	var sum uint64
	for i, tx := range b.txs {
		sum += tx.gas
		cumGas[i] = sum
	}
	return &prefixBlock{
		block:  b,
		cumGas: cumGas,
	}
}

func newPrefixBlock(blk *types.Block) *prefixBlock {
	return withPrefixSums(newBlock(blk))
}

func (b *prefixBlock) tipPercentilesBinary(percentiles []float64) []*big.Int {
	out := make([]*big.Int, len(percentiles))
	if len(b.txs) == 0 {
		for i := range out {
			out[i] = new(big.Int)
		}
		return out
	}

	for i, p := range percentiles {
		threshold := uint64(float64(b.gasUsed) * p / 100)
		txIndex, _ := slices.BinarySearch(b.cumGas, threshold)
		out[i] = b.txs[min(txIndex, len(b.txs)-1)].tip
	}
	return out
}

func TestTipPercentilesBinaryMatches(t *testing.T) {
	rng := rand.New(rand.NewPCG(0, 0)) //#nosec G404 -- Deterministic test input
	for range 10_000 {
		b := &block{
			baseFee: new(big.Int),
			txs:     make([]transaction, rng.IntN(50)),
		}
		var sumGas uint64
		for i := range b.txs {
			// Small gas values put thresholds exactly on cumulative-gas
			// boundaries often, and zeros produce runs of equal sums.
			b.txs[i] = transaction{
				gas: rng.Uint64N(10),
				tip: big.NewInt(rng.Int64N(100)),
			}
			sumGas += b.txs[i].gas
		}
		slices.SortFunc(b.txs, transaction.Compare)
		b.gasUsed = sumGas
		if rng.IntN(2) == 0 {
			// A gasUsed above the summed gas exercises the clamp to the last tx.
			b.gasUsed = rng.Uint64N(2*sumGas + 1)
		}

		percentiles := randomPercentiles(rng)
		want := b.tipPercentiles(percentiles)
		got := withPrefixSums(b).tipPercentilesBinary(percentiles)
		require.Equalf(t, want, got, "tipPercentilesBinary(%v) on %d txs", percentiles, len(b.txs))
	}
}

// blockStore is the subset of the block cache that the benchmarks compare.
type blockStore interface {
	get(n uint64) (*block, bool)
	put(n uint64, b *block)
}

type lruStore struct {
	cache *lru.Cache[uint64, *block]
}

func (s lruStore) get(n uint64) (*block, bool) { return s.cache.Get(n) }
func (s lruStore) put(n uint64, b *block)      { s.cache.Put(n, b) }

type ringEntry struct {
	height uint64
	blk    *block
}

// ringRW is a ring buffer keyed by height behind a single RWMutex.
type ringRW struct {
	lock    sync.RWMutex
	entries []ringEntry
}

func (r *ringRW) get(n uint64) (*block, bool) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	e := r.entries[n%uint64(len(r.entries))]
	if e.blk == nil || e.height != n {
		return nil, false
	}
	return e.blk, true
}

func (r *ringRW) put(n uint64, b *block) {
	r.lock.Lock()
	defer r.lock.Unlock()

	e := &r.entries[n%uint64(len(r.entries))]
	// An old height fetched on a miss must never evict a newer block.
	if e.blk != nil && n < e.height {
		return
	}
	*e = ringEntry{height: n, blk: b}
}

// ringAtomic is a ring buffer keyed by height with lock-free reads.
type ringAtomic struct {
	entries []atomic.Pointer[ringEntry]
}

func (r *ringAtomic) get(n uint64) (*block, bool) {
	e := r.entries[n%uint64(len(r.entries))].Load()
	if e == nil || e.height != n {
		return nil, false
	}
	return e.blk, true
}

func (r *ringAtomic) put(n uint64, b *block) {
	slot := &r.entries[n%uint64(len(r.entries))]
	e := &ringEntry{height: n, blk: b}
	for {
		old := slot.Load()
		if old != nil && n < old.height {
			return
		}
		if slot.CompareAndSwap(old, e) {
			return
		}
	}
}

var storeImpls = []struct {
	name string
	new  func(size int) blockStore
}{
	{
		name: "lru",
		new: func(size int) blockStore {
			return lruStore{lru.NewCache[uint64, *block](size)}
		},
	},
	{
		name: "ringRW",
		new: func(size int) blockStore {
			return &ringRW{entries: make([]ringEntry, size)}
		},
	},
	{
		name: "ringAtomic",
		new: func(size int) blockStore {
			return &ringAtomic{entries: make([]atomic.Pointer[ringEntry], size)}
		},
	},
}

func TestRingStoresKeepNewest(t *testing.T) {
	const size = 8
	for _, impl := range storeImpls[1:] {
		t.Run(impl.name, func(t *testing.T) {
			s := impl.new(size)
			newer, older, newest := &block{}, &block{}, &block{}

			s.put(size+1, newer)
			s.put(1, older)
			got, ok := s.get(size + 1)
			require.True(t, ok, "get(newer)")
			require.Same(t, newer, got, "get(newer)")
			_, ok = s.get(1)
			require.False(t, ok, "get(older) after it lost the slot")

			s.put(2*size+1, newest)
			_, ok = s.get(size + 1)
			require.False(t, ok, "get(newer) after eviction")
			got, ok = s.get(2*size + 1)
			require.True(t, ok, "get(newest)")
			require.Same(t, newest, got, "get(newest)")
		})
	}
}

// selectTipSort is the selection in [Estimator.SuggestGasTipCap].
func selectTipSort(blks []*block, percentile uint64) *big.Int {
	var tips []transaction
	for _, b := range blks {
		tips = append(tips, b.txs...)
	}
	n := len(tips)
	if n == 0 {
		return nil
	}
	slices.SortFunc(tips, transaction.Compare)
	return tips[(n-1)*int(percentile)/100].tip
}

type txRun struct {
	txs  []transaction
	next int
}

type txRunHeap []txRun

func (h txRunHeap) Len() int { return len(h) }
func (h txRunHeap) Less(i, j int) bool {
	return h[i].txs[h[i].next].Compare(h[j].txs[h[j].next]) < 0
}
func (h txRunHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *txRunHeap) Push(x any)   { *h = append(*h, x.(txRun)) }
func (h *txRunHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// selectTipMerge selects the same tip as [selectTipSort] by merging the
// already-sorted txs of each block, popping only up to the target index.
func selectTipMerge(blks []*block, percentile uint64) *big.Int {
	h := make(txRunHeap, 0, len(blks))
	var n int
	for _, b := range blks {
		if len(b.txs) == 0 {
			continue
		}
		h = append(h, txRun{txs: b.txs})
		n += len(b.txs)
	}
	if n == 0 {
		return nil
	}
	heap.Init(&h)
	for range (n - 1) * int(percentile) / 100 {
		h[0].next++
		if h[0].next == len(h[0].txs) {
			heap.Pop(&h)
		} else {
			heap.Fix(&h, 0)
		}
	}
	return h[0].txs[h[0].next].tip
}

func TestSelectTipMergeMatches(t *testing.T) {
	rng := rand.New(rand.NewPCG(0, 0)) //#nosec G404 -- Deterministic test input
	for range 10_000 {
		blks := make([]*block, rng.IntN(21))
		for i := range blks {
			blks[i] = &block{txs: make([]transaction, rng.IntN(50))}
			for j := range blks[i].txs {
				blks[i].txs[j] = transaction{tip: big.NewInt(rng.Int64N(100))}
			}
			slices.SortFunc(blks[i].txs, transaction.Compare)
		}
		percentile := rng.Uint64N(100) + 1

		want := selectTipSort(blks, percentile)
		got := selectTipMerge(blks, percentile)
		if want == nil {
			require.Nil(t, got, "selectTipMerge() with no txs")
			continue
		}
		require.Zerof(t, want.Cmp(got), "selectTipMerge(%d) = %v, want %v", percentile, got, want)
	}
}

func BenchmarkTipPercentiles(b *testing.B) {
	rng := rand.New(rand.NewPCG(0, 0)) //#nosec G404 -- Deterministic benchmark input
	for _, numTxs := range benchTxCounts(b) {
		blk := withPrefixSums(newRandomBlock(rng, numTxs))
		for _, numPercentiles := range []int{1, 5, maxPercentiles} {
			percentiles := evenPercentiles(numPercentiles)
			impls := []struct {
				name string
				fn   func([]float64) []*big.Int
			}{
				{"linear", blk.tipPercentiles},
				{"binary", blk.tipPercentilesBinary},
			}
			for _, impl := range impls {
				b.Run(fmt.Sprintf("impl=%s/txs=%d/percentiles=%d", impl.name, numTxs, numPercentiles), func(b *testing.B) {
					for b.Loop() {
						impl.fn(percentiles)
					}
				})
			}
		}
	}
}

func BenchmarkNewBlock(b *testing.B) {
	rng := rand.New(rand.NewPCG(0, 0)) //#nosec G404 -- Deterministic benchmark input
	for _, numTxs := range benchTxCounts(b) {
		blk := newRandomEthBlock(rng, numTxs)
		b.Run(fmt.Sprintf("impl=newBlock/txs=%d", numTxs), func(b *testing.B) {
			for b.Loop() {
				newBlock(blk)
			}
		})
		b.Run(fmt.Sprintf("impl=newPrefixBlock/txs=%d", numTxs), func(b *testing.B) {
			for b.Loop() {
				newPrefixBlock(blk)
			}
		})
	}
}

const (
	// benchCacheSize is the size [NewEstimator] gives the cache with
	// [DefaultConfig].
	benchCacheSize = 27_053
	// benchWindow is [DefaultConfig]'s HistoryMaxBlocks, the most blocks a
	// single [Estimator.FeeHistory] call reads.
	benchWindow      = 2048
	benchWindowStart = benchCacheSize - benchWindow
)

func newWarmStore(newStore func(int) blockStore) blockStore {
	s := newStore(benchCacheSize)
	blk := &block{}
	for n := range uint64(benchCacheSize) {
		s.put(n, blk)
	}
	return s
}

func BenchmarkCacheGet(b *testing.B) {
	for _, impl := range storeImpls {
		b.Run("impl="+impl.name, func(b *testing.B) {
			s := newWarmStore(impl.new)
			var i uint64
			for b.Loop() {
				if _, ok := s.get(benchWindowStart + i%benchWindow); !ok {
					b.Fatal("cache miss")
				}
				i++
			}
		})
	}
}

// startWriter re-puts the read window in a tight loop until the returned
// function is called, which waits for the writer to exit. This is far more
// writes than the one per accepted block seen in production.
func startWriter(s blockStore) (stop func()) {
	var (
		blk  = &block{}
		done = make(chan struct{})
		wg   sync.WaitGroup
	)
	wg.Go(func() {
		for i := uint64(0); ; i++ {
			select {
			case <-done:
				return
			default:
			}
			s.put(benchWindowStart+i%benchWindow, blk)
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

func BenchmarkCacheGetParallel(b *testing.B) {
	for _, impl := range storeImpls {
		for _, writer := range []bool{false, true} {
			b.Run(fmt.Sprintf("impl=%s/writer=%t", impl.name, writer), func(b *testing.B) {
				s := newWarmStore(impl.new)
				if writer {
					stop := startWriter(s)
					defer stop()
				}
				var offset atomic.Uint64
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					i := offset.Add(benchWindow / 16)
					for pb.Next() {
						if _, ok := s.get(benchWindowStart + i%benchWindow); !ok {
							b.Error("cache miss")
							return
						}
						i++
					}
				})
			})
		}
	}
}

func BenchmarkFeeHistory(b *testing.B) {
	rng := rand.New(rand.NewPCG(0, 0)) //#nosec G404 -- Deterministic benchmark input
	percentiles := []float64{10, 50, 90}
	for _, numTxs := range []int{100, 1_000} {
		b.Run(fmt.Sprintf("txs=%d", numTxs), func(b *testing.B) {
			cfg := DefaultConfig()
			sut := newSUT(b, cfg)
			txs := newRandomTxs(rng, numTxs)
			for range cfg.HistoryMaxBlocks {
				sut.addBlock(b, 0, nil, txs...)
			}

			ctx := b.Context()
			// Populates any block the background goroutine has not cached yet.
			_, _, _, _, err := sut.FeeHistory(ctx, cfg.HistoryMaxBlocks, rpc.LatestBlockNumber, percentiles)
			require.NoError(b, err, "FeeHistory()")
			for b.Loop() {
				if _, _, _, _, err := sut.FeeHistory(ctx, cfg.HistoryMaxBlocks, rpc.LatestBlockNumber, percentiles); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSuggestGasTipCap(b *testing.B) {
	rng := rand.New(rand.NewPCG(0, 0)) //#nosec G404 -- Deterministic benchmark input
	for _, numTxs := range []int{100, 1_000} {
		b.Run(fmt.Sprintf("txs=%d", numTxs), func(b *testing.B) {
			cfg := DefaultConfig()
			now := time.Unix(1_000_000, 0)
			cfg.Now = func() time.Time { return now }
			sut := newSUT(b, cfg)
			txs := newRandomTxs(rng, numTxs)

			// SuggestGasTipCap only recomputes on a new head, so every
			// iteration adds one and waits for the background goroutine to
			// cache it, keeping the fetch out of the measurement.
			addCachedBlock := func() {
				sut.addBlock(b, uint64(now.Unix()), nil, txs...)
				n := sut.chain.Last().NumberU64()
				for {
					if _, ok := sut.blockCache.cache.Get(n); ok {
						return
					}
					runtime.Gosched()
				}
			}
			for range cfg.SuggestedTipMaxBlocks {
				addCachedBlock()
			}

			ctx := b.Context()
			for b.Loop() {
				b.StopTimer()
				addCachedBlock()
				b.StartTimer()
				if _, err := sut.SuggestGasTipCap(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSuggestTipSelect(b *testing.B) {
	rng := rand.New(rand.NewPCG(0, 0)) //#nosec G404 -- Deterministic benchmark input
	cfg := DefaultConfig()
	impls := []struct {
		name string
		fn   func([]*block, uint64) *big.Int
	}{
		{"sort", selectTipSort},
		{"merge", selectTipMerge},
	}
	for _, numTxs := range benchTxCounts(b) {
		blks := make([]*block, cfg.SuggestedTipMaxBlocks)
		for i := range blks {
			blks[i] = newRandomBlock(rng, numTxs)
		}
		for _, impl := range impls {
			b.Run(fmt.Sprintf("impl=%s/txs=%d", impl.name, numTxs), func(b *testing.B) {
				for b.Loop() {
					impl.fn(blks, cfg.SuggestedTipPercentile)
				}
			})
		}
	}
}

// Copyright 2020 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package manifest

import (
	stdcmp "cmp"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/buildtags"
	"github.com/stretchr/testify/require"
)

func newItem(k InternalKey) *TableMetadata {
	m := (&TableMetadata{}).ExtendPointKeyBounds(
		base.DefaultComparer.Compare, k, k,
	)
	m.InitPhysicalBacking()
	return m
}

func cmp(a, b *TableMetadata) int {
	return cmpKey(a.Smallest(), b.Smallest())
}

func cmpKey(a, b InternalKey) int {
	return base.InternalCompare(base.DefaultComparer.Compare, a, b)
}

//////////////////////////////////////////
//        Invariant verification        //
//////////////////////////////////////////

// Verify asserts that the tree's structural invariants all hold.
func (t *btree[M]) Verify(tt *testing.T) {
	if t.Count() == 0 {
		require.Nil(tt, t.root)
		return
	}
	t.verifyLeafSameDepth(tt)
	t.verifyCountAllowed(tt)
	t.isSorted(tt)
	t.root.verifyInvariants()
}

func (t *btree[M]) verifyLeafSameDepth(tt *testing.T) {
	h := t.height()
	t.root.verifyDepthEqualToHeight(tt, 1, h)
}

func (n *node[M]) verifyDepthEqualToHeight(t *testing.T, depth, height int) {
	if n.leaf {
		require.Equal(t, height, depth, "all leaves should have the same depth as the tree height")
	}
	n.recurse(func(child *node[M], _ int16) {
		child.verifyDepthEqualToHeight(t, depth+1, height)
	})
}

func (t *btree[M]) verifyCountAllowed(tt *testing.T) {
	t.root.verifyCountAllowed(tt, true)
}

// height returns the height of the tree.
func (t *btree[M]) height() int {
	if t.root == nil {
		return 0
	}
	h := 1
	n := t.root
	for !n.leaf {
		n = n.children[0]
		h++
	}
	return h
}

func (n *node[M]) verifyCountAllowed(t *testing.T, root bool) {
	if !root {
		require.GreaterOrEqual(t, n.count, int16(minItems), "item count %d must be in range [%d,%d]", n.count, minItems, maxItems)
		require.LessOrEqual(t, n.count, int16(maxItems), "item count %d must be in range [%d,%d]", n.count, minItems, maxItems)
	}
	for i, item := range n.items {
		if i < int(n.count) {
			require.NotNil(t, item, "item below count")
		} else {
			require.Nil(t, item, "item above count")
		}
	}
	if !n.leaf {
		for i, child := range n.children {
			if i <= int(n.count) {
				require.NotNil(t, child, "node below count")
			} else {
				require.Nil(t, child, "node above count")
			}
		}
	}
	n.recurse(func(child *node[M], _ int16) {
		child.verifyCountAllowed(t, false)
	})
}

func (t *btree[M]) isSorted(tt *testing.T) {
	t.root.isSorted(tt, t.bcmp)
}

func (n *node[M]) isSorted(t *testing.T, cmp func(M, M) int) {
	for i := int16(1); i < n.count; i++ {
		require.LessOrEqual(t, cmp(n.items[i-1], n.items[i]), 0)
	}
	if !n.leaf {
		for i := int16(0); i < n.count; i++ {
			prev := n.children[i]
			next := n.children[i+1]

			require.LessOrEqual(t, cmp(prev.items[prev.count-1], n.items[i]), 0)
			require.LessOrEqual(t, cmp(n.items[i], next.items[0]), 0)
		}
	}
	n.recurse(func(child *node[M], _ int16) {
		child.isSorted(t, cmp)
	})
}

func (n *node[M]) recurse(f func(child *node[M], pos int16)) {
	if !n.leaf {
		for i := int16(0); i <= n.count; i++ {
			f(n.children[i], i)
		}
	}
}

//////////////////////////////////////////
//              Unit Tests              //
//////////////////////////////////////////

func key(i int) InternalKey {
	if i < 0 || i > 9999999 {
		panic("key out of bounds")
	}
	return base.MakeInternalKey([]byte(fmt.Sprintf("%07d", i)), 0, base.InternalKeyKindSet)
}

func keyWithMemo(i int, memo map[int]InternalKey) InternalKey {
	if s, ok := memo[i]; ok {
		return s
	}
	s := key(i)
	memo[i] = s
	return s
}

func checkIterRelative(
	t *testing.T, it *iterator[*TableMetadata], start, end int, keyMemo map[int]InternalKey,
) {
	t.Helper()
	i := start
	for ; it.valid(); it.next() {
		item := it.cur()
		expected := keyWithMemo(i, keyMemo)
		if cmpKey(expected, item.Smallest()) != 0 {
			t.Fatalf("expected %s, but found %s", expected, item.Smallest())
		}
		i++
	}
	if i != end {
		t.Fatalf("expected %d, but at %d", end, i)
	}
}

func checkIter(
	t *testing.T, it iterator[*TableMetadata], start, end int, keyMemo map[int]InternalKey,
) {
	t.Helper()
	i := start
	for it.first(); it.valid(); it.next() {
		item := it.cur()
		expected := keyWithMemo(i, keyMemo)
		if cmpKey(expected, item.Smallest()) != 0 {
			t.Fatalf("expected %s, but found %s", expected, item.Smallest())
		}
		require.Equal(t, i-start, it.countLeft())
		i++
	}
	if i != end {
		t.Fatalf("expected %d, but at %d", end, i)
	}

	for it.last(); it.valid(); it.prev() {
		i--
		item := it.cur()
		expected := keyWithMemo(i, keyMemo)
		if cmpKey(expected, item.Smallest()) != 0 {
			t.Fatalf("expected %s, but found %s", expected, item.Smallest())
		}
		require.Equal(t, i-start, it.countLeft())
	}
	if i != start {
		t.Fatalf("expected %d, but at %d: %+v", start, i, it)
	}
}

// TestBTree tests basic btree operations.
func TestBTree(t *testing.T) {
	var tr btree[*TableMetadata]
	tr.bcmp = cmp
	keyMemo := make(map[int]InternalKey)

	// With degree == 16 (max-items/node == 31) we need 513 items in order for
	// there to be 3 levels in the tree. The count here is comfortably above
	// that.
	const count = 768
	items := rang(0, count-1)

	// Add keys in sorted order.
	for i := 0; i < count; i++ {
		require.NoError(t, tr.Insert(items[i]))
		tr.Verify(t)
		if e := i + 1; e != tr.Count() {
			t.Fatalf("expected length %d, but found %d", e, tr.Count())
		}
		checkIter(t, tableMetadataIter(&tr), 0, i+1, keyMemo)
	}

	// delete keys in sorted order.
	for i := 0; i < count; i++ {
		var obsolete ObsoleteFiles
		tr.Delete(items[i], &obsolete)
		if len(obsolete.TableBackings) == 0 {
			t.Fatalf("expected item %d to be obsolete", i)
		}
		tr.Verify(t)
		if e := count - (i + 1); e != tr.Count() {
			t.Fatalf("expected length %d, but found %d", e, tr.Count())
		}
		checkIter(t, tableMetadataIter(&tr), i+1, count, keyMemo)
	}

	// Add keys in reverse sorted order.
	for i := 1; i <= count; i++ {
		require.NoError(t, tr.Insert(items[count-i]))
		tr.Verify(t)
		if i != tr.Count() {
			t.Fatalf("expected length %d, but found %d", i, tr.Count())
		}
		checkIter(t, tableMetadataIter(&tr), count-i, count, keyMemo)
	}

	// delete keys in reverse sorted order.
	for i := 1; i <= count; i++ {
		var obsolete ObsoleteFiles
		tr.Delete(items[count-i], &obsolete)
		if len(obsolete.TableBackings) == 0 {
			t.Fatalf("expected item %d to be obsolete", i)
		}
		tr.Verify(t)
		if e := count - i; e != tr.Count() {
			t.Fatalf("expected length %d, but found %d", e, tr.Count())
		}
		checkIter(t, tableMetadataIter(&tr), 0, count-i, keyMemo)
	}
}

func TestIterClone(t *testing.T) {
	const count = 65536

	var tr btree[*TableMetadata]
	tr.bcmp = cmp
	keyMemo := make(map[int]InternalKey)

	for i := 0; i < count; i++ {
		require.NoError(t, tr.Insert(newItem(key(i))))
	}

	it := tableMetadataIter(&tr)
	i := 0
	for it.first(); it.valid(); it.next() {
		if i%500 == 0 {
			c := it.clone()

			require.Equal(t, 0, cmpIter(it, c))
			checkIterRelative(t, &c, i, count, keyMemo)
			if i < count {
				require.Equal(t, -1, cmpIter(it, c))
				require.Equal(t, +1, cmpIter(c, it))
			}
		}
		i++
	}
}

func TestIterCmpEdgeCases(t *testing.T) {
	var tr btree[*TableMetadata]
	tr.bcmp = cmp
	t.Run("empty", func(t *testing.T) {
		a := tableMetadataIter(&tr)
		b := tableMetadataIter(&tr)
		require.Equal(t, 0, cmpIter(a, b))
	})
	require.NoError(t, tr.Insert(newItem(key(5))))
	t.Run("exhausted_next", func(t *testing.T) {
		a := tableMetadataIter(&tr)
		b := tableMetadataIter(&tr)
		a.first()
		b.first()
		require.Equal(t, 0, cmpIter(a, b))
		b.next()
		require.False(t, b.valid())
		require.Equal(t, -1, cmpIter(a, b))
	})
	t.Run("exhausted_prev", func(t *testing.T) {
		a := tableMetadataIter(&tr)
		b := tableMetadataIter(&tr)
		a.first()
		b.first()
		b.prev()
		require.False(t, b.valid())
		require.Equal(t, 1, cmpIter(a, b))
		b.next()
		require.Equal(t, 0, cmpIter(a, b))
	})
}

func TestIterCmpRand(t *testing.T) {
	const itemCount = 65536
	const iterCount = 1000

	var tr btree[*TableMetadata]
	tr.bcmp = cmp
	for i := 0; i < itemCount; i++ {
		require.NoError(t, tr.Insert(newItem(key(i))))
	}

	seed := uint64(time.Now().UnixNano())
	rng := rand.New(rand.NewPCG(0, seed))
	iters1 := make([]*LevelIterator, iterCount)
	iters2 := make([]*LevelIterator, iterCount)
	for i := 0; i < iterCount; i++ {
		k := rng.IntN(itemCount)
		iter := LevelIterator{iter: tableMetadataIter(&tr)}
		iter.SeekGE(base.DefaultComparer.Compare, key(k).UserKey)
		iters1[i] = &iter
		iters2[i] = &iter
	}

	// All the iterators should be positioned, so sorting them by items and by
	// iterator comparisons should equal identical orderings.
	slices.SortStableFunc(iters1, func(a, b *LevelIterator) int { return cmpIter(a.iter, b.iter) })
	slices.SortStableFunc(iters2, func(a, b *LevelIterator) int { return cmp(a.iter.cur(), b.iter.cur()) })
	for i := 0; i < iterCount; i++ {
		if iters1[i] != iters2[i] {
			t.Fatalf("seed %d: iters out of order at index %d:\n%s\n\n%s",
				seed, i, iters1[i], iters2[i])
		}
	}
}

// TestBTreeSeek tests basic btree iterator operations on an iterator wrapped
// by a LevelIterator.
func TestBTreeSeek(t *testing.T) {
	const count = 513

	var tr btree[*TableMetadata]
	tr.bcmp = cmp
	for i := 0; i < count; i++ {
		require.NoError(t, tr.Insert(newItem(key(i*2))))
	}

	it := LevelIterator{iter: tableMetadataIter(&tr)}
	for i := 0; i < 2*count-1; i++ {
		item := it.SeekGE(base.DefaultComparer.Compare, key(i).UserKey)
		if item == nil {
			t.Fatalf("%d: expected valid iterator", i)
		}
		expected := key(2 * ((i + 1) / 2))
		if cmpKey(expected, item.Smallest()) != 0 {
			t.Fatalf("%d: expected %s, but found %s", i, expected, item.Smallest())
		}
	}
	it.SeekGE(base.DefaultComparer.Compare, key(2*count-1).UserKey)
	if it.iter.valid() {
		t.Fatalf("expected invalid iterator")
	}

	for i := 1; i < 2*count; i++ {
		item := it.SeekLT(base.DefaultComparer.Compare, key(i).UserKey)
		if item == nil {
			t.Fatalf("%d: expected valid iterator", i)
		}
		expected := key(2 * ((i - 1) / 2))
		if cmpKey(expected, item.Smallest()) != 0 {
			t.Fatalf("%d: expected %s, but found %s", i, expected, item.Smallest())
		}
	}
	it.SeekLT(base.DefaultComparer.Compare, key(0).UserKey)
	if it.iter.valid() {
		t.Fatalf("expected invalid iterator")
	}
}

func TestBTreeInsertDuplicateError(t *testing.T) {
	var tr btree[*TableMetadata]
	tr.bcmp = cmp
	require.NoError(t, tr.Insert(newItem(key(1))))
	require.NoError(t, tr.Insert(newItem(key(2))))
	require.NoError(t, tr.Insert(newItem(key(3))))
	wantErr := errors.Errorf("files %s and %s collided on sort keys",
		errors.Safe(base.FileNum(000000)), errors.Safe(base.FileNum(000000)))
	require.Error(t, wantErr, tr.Insert(newItem(key(2))))
}

// TestBTreeCloneConcurrentOperations tests that cloning a btree returns a new
// btree instance which is an exact logical copy of the original but that can be
// modified independently going forward.
func TestBTreeCloneConcurrentOperations(t *testing.T) {
	const cloneTestSize = 1000
	p := perm(cloneTestSize)

	var trees []*btree[*TableMetadata]
	treeC, treeDone := make(chan *btree[*TableMetadata]), make(chan struct{})
	go func() {
		for b := range treeC {
			trees = append(trees, b)
		}
		close(treeDone)
	}()

	var wg sync.WaitGroup
	var populate func(tr *btree[*TableMetadata], start int)
	populate = func(tr *btree[*TableMetadata], start int) {
		t.Logf("Starting new clone at %v", start)
		treeC <- tr
		for i := start; i < cloneTestSize; i++ {
			require.NoError(t, tr.Insert(p[i]))
			if i%(cloneTestSize/5) == 0 {
				wg.Add(1)
				c := tr.Clone()
				go populate(&c, i+1)
			}
		}
		wg.Done()
	}

	wg.Add(1)
	var tr btree[*TableMetadata]
	tr.bcmp = cmp
	go populate(&tr, 0)
	wg.Wait()
	close(treeC)
	<-treeDone

	t.Logf("Starting equality checks on %d trees", len(trees))
	want := rang(0, cloneTestSize-1)
	for i, tree := range trees {
		got := slices.Collect(tree.All())
		if !reflect.DeepEqual(strReprs(got), strReprs(want)) {
			t.Errorf("tree %v mismatch", i)
		}
	}

	t.Log("Removing half of items from first half")
	toRemove := want[cloneTestSize/2:]
	for i := 0; i < len(trees)/2; i++ {
		tree := trees[i]
		wg.Add(1)
		go func() {
			for _, item := range toRemove {
				tree.Delete(item, ignoreObsoleteFiles{})
			}
			wg.Done()
		}()
	}
	wg.Wait()

	t.Log("Checking all values again")
	for i, tree := range trees {
		var wantpart []*TableMetadata
		if i < len(trees)/2 {
			wantpart = want[:cloneTestSize/2]
		} else {
			wantpart = want
		}
		got := slices.Collect(tree.All())
		if !reflect.DeepEqual(strReprs(got), strReprs(wantpart)) {
			t.Errorf("tree %v mismatch, want %#v got %#v", i, strReprs(wantpart), strReprs(got))
		}
	}

	var obsoleteFiles ObsoleteFiles
	for i := range trees {
		trees[i].Release(&obsoleteFiles)
	}
	if len(obsoleteFiles.TableBackings) != len(p) {
		t.Errorf("got %d obsolete trees, expected %d", len(obsoleteFiles.TableBackings), len(p))
	}
}

// TestIterStack tests the interface of the iterStack type.
func TestIterStack(t *testing.T) {
	f := func(i int) iterFrame[*TableMetadata] {
		return iterFrame[*TableMetadata]{pos: int16(i)}
	}
	var is iterStack[*TableMetadata]
	for i := 1; i <= 2*len(iterStackArr[*TableMetadata]{}); i++ {
		var j int
		for j = 0; j < i; j++ {
			is.push(f(j))
		}
		require.Equal(t, j, is.len())
		for j--; j >= 0; j-- {
			require.Equal(t, f(j), is.pop())
		}
		is.reset()
	}
}

func TestIterEndSentinel(t *testing.T) {
	var tr btree[*TableMetadata]
	tr.bcmp = cmp
	require.NoError(t, tr.Insert(newItem(key(1))))
	require.NoError(t, tr.Insert(newItem(key(2))))
	require.NoError(t, tr.Insert(newItem(key(3))))
	iter := LevelIterator{iter: tableMetadataIter(&tr)}
	iter.SeekGE(base.DefaultComparer.Compare, key(3).UserKey)
	require.True(t, iter.iter.valid())
	iter.Next()
	require.False(t, iter.iter.valid())

	// If we seek into the end sentinel, prev should return us to a valid
	// position.
	iter.SeekGE(base.DefaultComparer.Compare, key(4).UserKey)
	require.False(t, iter.iter.valid())
	iter.Prev()
	require.True(t, iter.iter.valid())
}

// TestRandomizedBTree tests a random set of Insert, Delete and iteration
// operations, checking for equivalence with a map of filenums.
func TestRandomizedBTree(t *testing.T) {
	const maxFileNum = 50_000

	seed := uint64(time.Now().UnixNano())
	t.Log("seed", seed)
	rng := rand.New(rand.NewPCG(0, seed))

	var numOps int
	if buildtags.SlowBuild {
		// Reduce the number of ops in race mode so the test doesn't take very long.
		numOps = 1_000 + rng.IntN(4_000)
	} else {
		numOps = 10_000 + rng.IntN(40_000)
	}

	var metadataAlloc [maxFileNum]TableMetadata
	for i := 0; i < len(metadataAlloc); i++ {
		metadataAlloc[i].TableNum = base.FileNum(i)
		metadataAlloc[i].InitPhysicalBacking()
	}

	// Use a btree comparator that sorts by file number to make it easier to
	// prevent duplicates or overlaps.
	tree := btree[*TableMetadata]{
		bcmp: func(a *TableMetadata, b *TableMetadata) int {
			return stdcmp.Compare(a.TableNum, b.TableNum)
		},
	}

	type opDecl struct {
		fn     func()
		weight int
	}
	ref := map[base.FileNum]bool{}
	ops := []opDecl{
		{
			// Insert
			fn: func() {
				f := &metadataAlloc[rng.IntN(maxFileNum)]
				err := tree.Insert(f)
				if ref[f.TableNum] {
					require.Error(t, err, "btree.Insert should error if file already exists")
				} else {
					ref[f.TableNum] = true
					require.NoError(t, err)
				}
			},
			weight: 20,
		},
		{
			// Delete
			fn: func() {
				f := &metadataAlloc[rng.IntN(maxFileNum)]
				tree.Delete(f, ignoreObsoleteFiles{})
				delete(ref, f.TableNum)
			},
			weight: 10,
		},
		{
			// Iterate
			fn: func() {
				iter := tableMetadataIter(&tree)
				count := 0
				var prev base.FileNum
				for iter.first(); iter.valid(); iter.next() {
					fn := iter.cur().TableNum
					require.True(t, ref[fn])
					if count > 0 {
						require.Less(t, prev, fn)
					}
					count++
				}
				require.Equal(t, count, len(ref))
			},
			weight: 1,
		},
	}
	weightSum := 0
	for i := range ops {
		weightSum += ops[i].weight
	}

	for i := 0; i < numOps; i++ {
		w := rng.IntN(weightSum)
		for j := range ops {
			w -= ops[j].weight
			if w < 0 {
				ops[j].fn()
				break
			}
		}
	}
}

//////////////////////////////////////////
//              Benchmarks              //
//////////////////////////////////////////

// perm returns a random permutation of items with keys in the range [0, n).
func perm(n int) (out []*TableMetadata) {
	for _, i := range rand.Perm(n) {
		out = append(out, newItem(key(i)))
	}
	return out
}

// rang returns an ordered list of items with keys in the range [m, n].
func rang(m, n int) (out []*TableMetadata) {
	for i := m; i <= n; i++ {
		out = append(out, newItem(key(i)))
	}
	return out
}

func strReprs(items []*TableMetadata) []string {
	s := make([]string, len(items))
	for i := range items {
		s[i] = items[i].String()
	}
	return s
}

func forBenchmarkSizes(b *testing.B, f func(b *testing.B, count int)) {
	for _, count := range []int{16, 128, 1024, 8192, 65536} {
		b.Run(fmt.Sprintf("count=%d", count), func(b *testing.B) {
			f(b, count)
		})
	}
}

// BenchmarkBTreeInsert measures btree insertion performance.
func BenchmarkBTreeInsert(b *testing.B) {
	forBenchmarkSizes(b, func(b *testing.B, count int) {
		insertP := perm(count)
		b.ResetTimer()
		for i := 0; i < b.N; {
			var tr btree[*TableMetadata]
			tr.bcmp = cmp
			for _, item := range insertP {
				if err := tr.Insert(item); err != nil {
					b.Fatal(err)
				}
				i++
				if i >= b.N {
					return
				}
			}
		}
	})
}

// BenchmarkBTreeDelete measures btree deletion performance.
func BenchmarkBTreeDelete(b *testing.B) {
	forBenchmarkSizes(b, func(b *testing.B, count int) {
		insertP, removeP := perm(count), perm(count)
		b.ResetTimer()
		for i := 0; i < b.N; {
			b.StopTimer()
			var tr btree[*TableMetadata]
			tr.bcmp = cmp
			for _, item := range insertP {
				if err := tr.Insert(item); err != nil {
					b.Fatal(err)
				}
			}
			b.StartTimer()
			for _, item := range removeP {
				tr.Delete(item, ignoreObsoleteFiles{})
				i++
				if i >= b.N {
					return
				}
			}
			if tr.Count() > 0 {
				b.Fatalf("tree not empty: %s", &tr)
			}
		}
	})
}

// BenchmarkBTreeDeleteInsert measures btree deletion and insertion performance.
func BenchmarkBTreeDeleteInsert(b *testing.B) {
	forBenchmarkSizes(b, func(b *testing.B, count int) {
		insertP := perm(count)
		var tr btree[*TableMetadata]
		tr.bcmp = cmp
		for _, item := range insertP {
			if err := tr.Insert(item); err != nil {
				b.Fatal(err)
			}
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			item := insertP[i%count]
			tr.Delete(item, ignoreObsoleteFiles{})
			if err := tr.Insert(item); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkBTreeDeleteInsertCloneOnce measures btree deletion and insertion
// performance after the tree has been copy-on-write cloned once.
func BenchmarkBTreeDeleteInsertCloneOnce(b *testing.B) {
	forBenchmarkSizes(b, func(b *testing.B, count int) {
		insertP := perm(count)
		var tr btree[*TableMetadata]
		tr.bcmp = cmp
		for _, item := range insertP {
			if err := tr.Insert(item); err != nil {
				b.Fatal(err)
			}
		}
		tr = tr.Clone()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			item := insertP[i%count]
			tr.Delete(item, ignoreObsoleteFiles{})
			if err := tr.Insert(item); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkBTreeDeleteInsertCloneEachTime measures btree deletion and insertion
// performance while the tree is repeatedly copy-on-write cloned.
func BenchmarkBTreeDeleteInsertCloneEachTime(b *testing.B) {
	for _, release := range []bool{false, true} {
		b.Run(fmt.Sprintf("release=%t", release), func(b *testing.B) {
			forBenchmarkSizes(b, func(b *testing.B, count int) {
				insertP := perm(count)
				var tr, trRelease btree[*TableMetadata]
				tr.bcmp = cmp
				trRelease.bcmp = cmp
				for _, item := range insertP {
					if err := tr.Insert(item); err != nil {
						b.Fatal(err)
					}
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					item := insertP[i%count]
					if release {
						trRelease.Release(ignoreObsoleteFiles{})
						trRelease = tr
					}
					tr = tr.Clone()
					tr.Delete(item, ignoreObsoleteFiles{})
					if err := tr.Insert(item); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// BenchmarkBTreeIter measures the cost of creating a btree iterator.
func BenchmarkBTreeIter(b *testing.B) {
	var tr btree[*TableMetadata]
	tr.bcmp = cmp
	for i := 0; i < b.N; i++ {
		it := tableMetadataIter(&tr)
		it.first()
	}
}

// BenchmarkBTreeIterSeekGE measures the cost of seeking a btree iterator
// forward.
func BenchmarkBTreeIterSeekGE(b *testing.B) {
	rng := rand.New(rand.NewPCG(0, rand.Uint64()))
	forBenchmarkSizes(b, func(b *testing.B, count int) {
		var keys []InternalKey
		var tr btree[*TableMetadata]
		tr.bcmp = cmp

		for i := 0; i < count; i++ {
			s := key(i)
			keys = append(keys, s)
			if err := tr.Insert(newItem(s)); err != nil {
				b.Fatal(err)
			}
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			k := keys[rng.IntN(len(keys))]
			it := LevelIterator{iter: tableMetadataIter(&tr)}
			f := it.SeekGE(base.DefaultComparer.Compare, k.UserKey)
			if testing.Verbose() {
				if f == nil {
					b.Fatal("expected to find key")
				}
				if cmpKey(k, f.Smallest()) != 0 {
					b.Fatalf("expected %s, but found %s", k, f.Smallest())
				}
			}
		}
	})
}

// BenchmarkBTreeIterSeekLT measures the cost of seeking a btree iterator
// backward.
func BenchmarkBTreeIterSeekLT(b *testing.B) {
	rng := rand.New(rand.NewPCG(0, uint64(time.Now().UnixNano())))
	forBenchmarkSizes(b, func(b *testing.B, count int) {
		var keys []InternalKey
		var tr btree[*TableMetadata]
		tr.bcmp = cmp

		for i := 0; i < count; i++ {
			k := key(i)
			keys = append(keys, k)
			if err := tr.Insert(newItem(k)); err != nil {
				b.Fatal(err)
			}
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			j := rng.IntN(len(keys))
			k := keys[j]
			it := LevelIterator{iter: tableMetadataIter(&tr)}
			f := it.SeekLT(base.DefaultComparer.Compare, k.UserKey)
			if testing.Verbose() {
				if j == 0 {
					if f != nil {
						b.Fatal("unexpected key")
					}
				} else {
					if f == nil {
						b.Fatal("expected to find key")
					}
					k := keys[j-1]
					if cmpKey(k, f.Smallest()) != 0 {
						b.Fatalf("expected %s, but found %s", k, f.Smallest())
					}
				}
			}
		}
	})
}

// BenchmarkBTreeIterNext measures the cost of seeking a btree iterator to the
// next item in the tree.
func BenchmarkBTreeIterNext(b *testing.B) {
	var tr btree[*TableMetadata]
	tr.bcmp = cmp

	const count = 8 << 10
	for i := 0; i < count; i++ {
		item := newItem(key(i))
		if err := tr.Insert(item); err != nil {
			b.Fatal(err)
		}
	}

	it := tableMetadataIter(&tr)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !it.valid() {
			it.first()
		}
		it.next()
	}
}

// BenchmarkBTreeIterPrev measures the cost of seeking a btree iterator to the
// previous item in the tree.
func BenchmarkBTreeIterPrev(b *testing.B) {
	var tr btree[*TableMetadata]
	tr.bcmp = cmp

	const count = 8 << 10
	for i := 0; i < count; i++ {
		item := newItem(key(i))
		if err := tr.Insert(item); err != nil {
			b.Fatal(err)
		}
	}

	it := tableMetadataIter(&tr)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !it.valid() {
			it.first()
		}
		it.prev()
	}
}

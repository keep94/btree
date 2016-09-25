// Copyright 2014 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package btree implements in-memory B-Trees of arbitrary degree.
//
// btree implements an in-memory B-Tree for use as an ordered data structure.
// It is not meant for persistent storage solutions.
//
// It has a flatter structure than an equivalent red-black or other binary tree,
// which in some cases yields better memory usage and/or performance.
// See some discussion on the matter here:
//   http://google-opensource.blogspot.com/2013/01/c-containers-that-save-memory-and-time.html
// Note, though, that this project is in no way related to the C++ B-Tree
// implementation written about there.
//
// Within this tree, each node contains a slice of items and a (possibly nil)
// slice of children.  For basic numeric values or raw structs, this can cause
// efficiency differences when compared to equivalent C++ template code that
// stores values in arrays within the node:
//   * Due to the overhead of storing values as interfaces (each
//     value needs to be stored as the value itself, then 2 words for the
//     interface pointing to that value and its type), resulting in higher
//     memory use.
//   * Since interfaces can point to values anywhere in memory, values are
//     most likely not stored in contiguous blocks, resulting in a higher
//     number of cache misses.
// These issues don't tend to matter, though, when working with strings or other
// heap-allocated structures, since C++-equivalent structures also must store
// pointers and also distribute their values across the heap.
//
// This implementation is designed to be a drop-in replacement to gollrb.LLRB
// trees, (http://github.com/petar/gollrb), an excellent and probably the most
// widely used ordered tree implementation in the Go ecosystem currently.
// Its functions, therefore, exactly mirror those of
// llrb.LLRB where possible.  Unlike gollrb, though, we currently don't
// support storing multiple equivalent values.
package btree

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Item represents a single object in the tree.
type Item interface {
	// Less tests whether the current item is less than the given argument.
	//
	// This must provide a strict weak ordering.
	// If !a.Less(b) && !b.Less(a), we treat this to mean a == b (i.e. we can only
	// hold one of either a or b in the tree).
	Less(than Item) bool
}

const (
	DefaultFreeListSize = 32
)

// FreeList represents a free list of btree nodes. By default each
// BTree has its own FreeList, but multiple BTrees can share the same
// FreeList.
// Two Btrees using the same freelist are not safe for concurrent write access.
type FreeList struct {
	freelist []*node
}

// NewFreeList creates a new free list.
// size is the maximum size of the returned free list.
func NewFreeList(size int) *FreeList {
	return &FreeList{freelist: make([]*node, 0, size)}
}

func (f *FreeList) newNode() (n *node) {
	index := len(f.freelist) - 1
	if index < 0 {
		return new(node)
	}
	f.freelist, n = f.freelist[:index], f.freelist[index]
	return
}

func (f *FreeList) freeNode(n *node) {
	if len(f.freelist) < cap(f.freelist) {
		f.freelist = append(f.freelist, n)
	}
}

// ItemIterator allows callers of Ascend* to iterate in-order over portions of
// the tree.  When this function returns false, iteration will stop and the
// associated Ascend* function will immediately return.
type ItemIterator func(i Item) bool

// New creates a new B-Tree with the given degree.
//
// New(2), for example, will create a 2-3-4 tree (each node contains 1-3 items
// and 2-4 children).
func New(degree int) *BTree {
	return NewWithFreeList(degree, NewFreeList(DefaultFreeListSize))
}

// NewWithFreeList creates a new B-Tree that uses the given node free list.
func NewWithFreeList(degree int, f *FreeList) *BTree {
	if degree <= 1 {
		panic("bad degree")
	}
	return &BTree{
		op: &btreeOp{
			degree:   degree,
			freelist: f,
		},
	}
}

// items stores items in a node.
type items []Item

// insertAt inserts a value into the given index, pushing all subsequent values
// forward.
func (s *items) insertAt(index int, item Item) {
	*s = append(*s, nil)
	if index < len(*s) {
		copy((*s)[index+1:], (*s)[index:])
	}
	(*s)[index] = item
}

// removeAt removes a value at a given index, pulling all subsequent values
// back.
func (s *items) removeAt(index int) Item {
	item := (*s)[index]
	copy((*s)[index:], (*s)[index+1:])
	(*s)[len(*s)-1] = nil
	*s = (*s)[:len(*s)-1]
	return item
}

// pop removes and returns the last element in the list.
func (s *items) pop() (out Item) {
	index := len(*s) - 1
	out = (*s)[index]
	(*s)[index] = nil
	*s = (*s)[:index]
	return
}

// find returns the index where the given item should be inserted into this
// list.  'found' is true if the item already exists in the list at the given
// index.
func (s items) find(item Item) (index int, found bool) {
	i := sort.Search(len(s), func(i int) bool {
		return item.Less(s[i])
	})
	if i > 0 && !s[i-1].Less(item) {
		return i - 1, true
	}
	return i, false
}

// children stores child nodes in a node.
type children []*node

// insertAt inserts a value into the given index, pushing all subsequent values
// forward.
func (s *children) insertAt(index int, n *node) {
	*s = append(*s, nil)
	if index < len(*s) {
		copy((*s)[index+1:], (*s)[index:])
	}
	(*s)[index] = n
}

// removeAt removes a value at a given index, pulling all subsequent values
// back.
func (s *children) removeAt(index int) *node {
	n := (*s)[index]
	copy((*s)[index:], (*s)[index+1:])
	(*s)[len(*s)-1] = nil
	*s = (*s)[:len(*s)-1]
	return n
}

// pop removes and returns the last element in the list.
func (s *children) pop() (out *node) {
	index := len(*s) - 1
	out = (*s)[index]
	(*s)[index] = nil
	*s = (*s)[:index]
	return
}

// node is an internal node in a tree.
//
// It must at all times maintain the invariant that either
//   * len(children) == 0, len(items) unconstrained
//   * len(children) == len(items) + 1
type node struct {
	items    items
	children children
	op       *btreeOp
}

// split splits the given node at the given index.  The current node shrinks,
// and this function returns the item that existed at that index and a new node
// containing all items/children after it.
func (n *node) split(i int, writables copyOnWriteSet) (Item, *node) {
	item := n.items[i]
	hasChildren := len(n.children) > 0
	next := newNode(n.op, writables, hasChildren)
	next.items = append(next.items, n.items[i+1:]...)
	n.items = n.items[:i]
	if hasChildren {
		next.children = append(next.children, n.children[i+1:]...)
		n.children = n.children[:i+1]
	}
	return item, next
}

// maybeSplitChild checks if a child should be split, and if so splits it.
// Returns whether or not a split occurred.
func (n *node) maybeSplitChild(i, maxItems int, writables copyOnWriteSet) bool {
	if len(n.children[i].items) < maxItems {
		return false
	}
	n.children[i] = writables.writableNode(n.children[i])
	first := n.children[i]
	item, second := first.split(maxItems/2, writables)
	n.items.insertAt(i, item)
	n.children.insertAt(i+1, second)
	return true
}

// insert inserts an item into the subtree rooted at this node, making sure
// no nodes in the subtree exceed maxItems items.  Should an equivalent item be
// be found/replaced by insert, it will be returned.
func (n *node) insert(item Item, maxItems int, writables copyOnWriteSet) Item {
	i, found := n.items.find(item)
	if found {
		out := n.items[i]
		n.items[i] = item
		return out
	}
	if len(n.children) == 0 {
		n.items.insertAt(i, item)
		return nil
	}
	if n.maybeSplitChild(i, maxItems, writables) {
		inTree := n.items[i]
		switch {
		case item.Less(inTree):
			// no change, we want first split node
		case inTree.Less(item):
			i++ // we want second split node
		default:
			out := n.items[i]
			n.items[i] = item
			return out
		}
	}
	n.children[i] = writables.writableNode(n.children[i])
	return n.children[i].insert(item, maxItems, writables)
}

// get finds the given key in the subtree and returns it.
func (n *node) get(key Item) Item {
	i, found := n.items.find(key)
	if found {
		return n.items[i]
	} else if len(n.children) > 0 {
		return n.children[i].get(key)
	}
	return nil
}

// min returns the first item in the subtree.
func min(n *node) Item {
	if n == nil {
		return nil
	}
	for len(n.children) > 0 {
		n = n.children[0]
	}
	if len(n.items) == 0 {
		return nil
	}
	return n.items[0]
}

// max returns the last item in the subtree.
func max(n *node) Item {
	if n == nil {
		return nil
	}
	for len(n.children) > 0 {
		n = n.children[len(n.children)-1]
	}
	if len(n.items) == 0 {
		return nil
	}
	return n.items[len(n.items)-1]
}

// toRemove details what item to remove in a node.remove call.
type toRemove int

const (
	removeItem toRemove = iota // removes the given item
	removeMin                  // removes smallest item in the subtree
	removeMax                  // removes largest item in the subtree
)

// remove removes an item from the subtree rooted at this node.
func (n *node) remove(
	item Item, minItems int, typ toRemove, writables copyOnWriteSet) Item {
	var i int
	var found bool
	switch typ {
	case removeMax:
		if len(n.children) == 0 {
			return n.items.pop()
		}
		i = len(n.items)
	case removeMin:
		if len(n.children) == 0 {
			return n.items.removeAt(0)
		}
		i = 0
	case removeItem:
		i, found = n.items.find(item)
		if len(n.children) == 0 {
			if found {
				return n.items.removeAt(i)
			}
			return nil
		}
	default:
		panic("invalid type")
	}
	// If we get to here, we have children.
	if len(n.children[i].items) <= minItems {
		return n.growChildAndRemove(i, item, minItems, typ, writables)
	}
	// Either we had enough items to begin with, or we've done some
	// merging/stealing, because we've got enough now and we're ready to return
	// stuff.
	n.children[i] = writables.writableNode(n.children[i])
	child := n.children[i]
	if found {
		// The item exists at index 'i', and the child we've selected can give us a
		// predecessor, since if we've gotten here it's got > minItems items in it.
		out := n.items[i]
		// We use our special-case 'remove' call with typ=maxItem to pull the
		// predecessor of item i (the rightmost leaf of our immediate left child)
		// and set it into where we pulled the item from.
		n.items[i] = child.remove(nil, minItems, removeMax, writables)
		return out
	}
	// Final recursive call.  Once we're here, we know that the item isn't in this
	// node and that the child is big enough to remove from.
	return child.remove(item, minItems, typ, writables)
}

// growChildAndRemove grows child 'i' to make sure it's possible to remove an
// item from it while keeping it at minItems, then calls remove to actually
// remove it.
//
// Most documentation says we have to do two sets of special casing:
//   1) item is in this node
//   2) item is in child
// In both cases, we need to handle the two subcases:
//   A) node has enough values that it can spare one
//   B) node doesn't have enough values
// For the latter, we have to check:
//   a) left sibling has node to spare
//   b) right sibling has node to spare
//   c) we must merge
// To simplify our code here, we handle cases #1 and #2 the same:
// If a node doesn't have enough items, we make sure it does (using a,b,c).
// We then simply redo our remove call, and the second time (regardless of
// whether we're in case 1 or 2), we'll have enough items and can guarantee
// that we hit case A.
func (n *node) growChildAndRemove(
	i int,
	item Item,
	minItems int,
	typ toRemove,
	writables copyOnWriteSet) Item {
	if i > 0 && len(n.children[i-1].items) > minItems {
		// Steal from left child
		n.children[i] = writables.writableNode(n.children[i])
		child := n.children[i]
		n.children[i-1] = writables.writableNode(n.children[i-1])
		stealFrom := n.children[i-1]
		stolenItem := stealFrom.items.pop()
		child.items.insertAt(0, n.items[i-1])
		n.items[i-1] = stolenItem
		if len(stealFrom.children) > 0 {
			child.children.insertAt(0, stealFrom.children.pop())
		}
	} else if i < len(n.items) && len(n.children[i+1].items) > minItems {
		// steal from right child
		n.children[i] = writables.writableNode(n.children[i])
		child := n.children[i]
		n.children[i+1] = writables.writableNode(n.children[i+1])
		stealFrom := n.children[i+1]
		stolenItem := stealFrom.items.removeAt(0)
		child.items = append(child.items, n.items[i])
		n.items[i] = stolenItem
		if len(stealFrom.children) > 0 {
			child.children = append(child.children, stealFrom.children.removeAt(0))
		}
	} else {
		if i >= len(n.items) {
			i--
		}
		// merge with right child
		n.children[i] = writables.writableNode(n.children[i])
		child := n.children[i]
		mergeItem := n.items.removeAt(i)
		mergeChild := n.children.removeAt(i + 1)
		child.items = append(child.items, mergeItem)
		child.items = append(child.items, mergeChild.items...)
		child.children = append(child.children, mergeChild.children...)
		freeNode(mergeChild, n.op, writables)
	}
	return n.remove(item, minItems, typ, writables)
}

type direction int

const (
	descend = direction(-1)
	ascend  = direction(+1)
)

// iterate provides a simple method for iterating over elements in the tree.
//
// When ascending, the 'start' should be less than 'stop' and when descending,
// the 'start' should be greater than 'stop'. Setting 'includeStart' to true
// will force the iterator to include the first item when it equals 'start',
// thus creating a "greaterOrEqual" or "lessThanEqual" rather than just a
// "greaterThan" or "lessThan" queries.
func (n *node) iterate(dir direction, start, stop Item, includeStart bool, hit bool, iter ItemIterator) (bool, bool) {
	var ok bool
	switch dir {
	case ascend:
		for i := 0; i < len(n.items); i++ {
			if start != nil && n.items[i].Less(start) {
				continue
			}
			if len(n.children) > 0 {
				if hit, ok = n.children[i].iterate(dir, start, stop, includeStart, hit, iter); !ok {
					return hit, false
				}
			}
			if !includeStart && !hit && start != nil && !start.Less(n.items[i]) {
				hit = true
				continue
			}
			hit = true
			if stop != nil && !n.items[i].Less(stop) {
				return hit, false
			}
			if !iter(n.items[i]) {
				return hit, false
			}
		}
		if len(n.children) > 0 {
			if hit, ok = n.children[len(n.children)-1].iterate(dir, start, stop, includeStart, hit, iter); !ok {
				return hit, false
			}
		}
	case descend:
		for i := len(n.items) - 1; i >= 0; i-- {
			if start != nil && !n.items[i].Less(start) {
				if !includeStart || hit || start.Less(n.items[i]) {
					continue
				}
			}
			if len(n.children) > 0 {
				if hit, ok = n.children[i+1].iterate(dir, start, stop, includeStart, hit, iter); !ok {
					return hit, false
				}
			}
			if stop != nil && !stop.Less(n.items[i]) {
				return hit, false //	continue
			}
			hit = true
			if !iter(n.items[i]) {
				return hit, false
			}
		}
		if len(n.children) > 0 {
			if hit, ok = n.children[0].iterate(dir, start, stop, includeStart, hit, iter); !ok {
				return hit, false
			}
		}
	}
	return hit, true
}

// Used for testing/debugging purposes.
func (n *node) print(w io.Writer, level int) {
	fmt.Fprintf(w, "%sNODE:%v\n", strings.Repeat("  ", level), n.items)
	for _, c := range n.children {
		c.print(w, level+1)
	}
}

type copyOnWriteSet map[*node]bool

func (s copyOnWriteSet) newNode(op *btreeOp, withChildren bool) *node {
	result := &node{
		op:    op,
		items: make(items, 0, op.maxItems())}
	if withChildren {
		result.children = make(children, 0, op.maxItems()+1)
	}
	s[result] = true
	return result
}

func (s copyOnWriteSet) writableNode(n *node) *node {
	if s == nil || s[n] {
		return n
	}
	hasChildren := len(n.children) > 0
	result := s.newNode(n.op, hasChildren)
	result.items = append(result.items, n.items...)
	if hasChildren {
		result.children = append(result.children, n.children...)
	}
	return result
}

type btreeOp struct {
	degree   int
	freelist *FreeList
}

// maxItems returns the max number of items to allow per node.
func (o *btreeOp) maxItems() int {
	return o.degree*2 - 1
}

// minItems returns the min number of items to allow per node (ignored for the
// root node).
func (o *btreeOp) minItems() int {
	return o.degree - 1
}

func (o *btreeOp) newNode() (n *node) {
	n = o.freelist.newNode()
	n.op = o
	return
}

func (o *btreeOp) freeNode(n *node) {
	for i := range n.items {
		n.items[i] = nil // clear to allow GC
	}
	n.items = n.items[:0]
	for i := range n.children {
		n.children[i] = nil // clear to allow GC
	}
	n.children = n.children[:0]
	n.op = nil // clear to allow GC
	o.freelist.freeNode(n)
}

func newNode(op *btreeOp, writables copyOnWriteSet, withChildren bool) *node {
	if writables == nil {
		return op.newNode()
	}
	return writables.newNode(op, withChildren)
}

func freeNode(n *node, op *btreeOp, writables copyOnWriteSet) {
	if writables == nil {
		op.freeNode(n)
		return
	}
	delete(writables, n)
}

// BTree is an implementation of a B-Tree.
//
// BTree stores Item instances in an ordered structure, allowing easy insertion,
// removal, and iteration.
//
// Write operations are not safe for concurrent mutation by multiple
// goroutines, but Read operations are.
type BTree struct {
	op     *btreeOp
	length int
	root   *node
}

func (t *BTree) replaceOrInsert(item Item, writables copyOnWriteSet) Item {
	if item == nil {
		panic("nil item being added to BTree")
	}
	if t.root == nil {

		t.root = newNode(t.op, writables, false)
		t.root.items = append(t.root.items, item)
		t.length++
		return nil
	} else if len(t.root.items) >= t.op.maxItems() {
		t.root = writables.writableNode(t.root)
		item2, second := t.root.split(t.op.maxItems()/2, writables)
		oldroot := t.root
		t.root = newNode(t.op, writables, true)
		t.root.items = append(t.root.items, item2)
		t.root.children = append(t.root.children, oldroot, second)
	}
	t.root = writables.writableNode(t.root)
	out := t.root.insert(item, t.op.maxItems(), writables)
	if out == nil {
		t.length++
	}
	return out
}

// ReplaceOrInsert adds the given item to the tree.  If an item in the tree
// already equals the given one, it is removed from the tree and returned.
// Otherwise, nil is returned.
//
// nil cannot be added to the tree (will panic).
func (t *BTree) ReplaceOrInsert(item Item) Item {
	return t.replaceOrInsert(item, nil)
}

// Delete removes an item equal to the passed in item from the tree, returning
// it.  If no such item exists, returns nil.
func (t *BTree) Delete(item Item) Item {
	return t.deleteItem(item, removeItem, nil)
}

// DeleteMin removes the smallest item in the tree and returns it.
// If no such item exists, returns nil.
func (t *BTree) DeleteMin() Item {
	return t.deleteItem(nil, removeMin, nil)
}

// DeleteMax removes the largest item in the tree and returns it.
// If no such item exists, returns nil.
func (t *BTree) DeleteMax() Item {
	return t.deleteItem(nil, removeMax, nil)
}

func (t *BTree) deleteItem(
	item Item, typ toRemove, writables copyOnWriteSet) Item {
	if t.root == nil || len(t.root.items) == 0 {
		return nil
	}
	t.root = writables.writableNode(t.root)
	out := t.root.remove(item, t.op.minItems(), typ, writables)
	if len(t.root.items) == 0 && len(t.root.children) > 0 {
		oldroot := t.root
		t.root = t.root.children[0]
		freeNode(oldroot, t.op, writables)
	}
	if out != nil {
		t.length--
	}
	return out
}

// AscendRange calls the iterator for every value in the tree within the range
// [greaterOrEqual, lessThan), until iterator returns false.
func (t *BTree) AscendRange(greaterOrEqual, lessThan Item, iterator ItemIterator) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, greaterOrEqual, lessThan, true, false, iterator)
}

// AscendLessThan calls the iterator for every value in the tree within the range
// [first, pivot), until iterator returns false.
func (t *BTree) AscendLessThan(pivot Item, iterator ItemIterator) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, nil, pivot, false, false, iterator)
}

// AscendGreaterOrEqual calls the iterator for every value in the tree within
// the range [pivot, last], until iterator returns false.
func (t *BTree) AscendGreaterOrEqual(pivot Item, iterator ItemIterator) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, pivot, nil, true, false, iterator)
}

// Ascend calls the iterator for every value in the tree within the range
// [first, last], until iterator returns false.
func (t *BTree) Ascend(iterator ItemIterator) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, nil, nil, false, false, iterator)
}

// DescendRange calls the iterator for every value in the tree within the range
// [lessOrEqual, greaterThan), until iterator returns false.
func (t *BTree) DescendRange(lessOrEqual, greaterThan Item, iterator ItemIterator) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, lessOrEqual, greaterThan, true, false, iterator)
}

// DescendLessOrEqual calls the iterator for every value in the tree within the range
// [pivot, first], until iterator returns false.
func (t *BTree) DescendLessOrEqual(pivot Item, iterator ItemIterator) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, pivot, nil, true, false, iterator)
}

// DescendGreaterThan calls the iterator for every value in the tree within
// the range (pivot, last], until iterator returns false.
func (t *BTree) DescendGreaterThan(pivot Item, iterator ItemIterator) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, nil, pivot, false, false, iterator)
}

// Descend calls the iterator for every value in the tree within the range
// [last, first], until iterator returns false.
func (t *BTree) Descend(iterator ItemIterator) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, nil, nil, false, false, iterator)
}

// Get looks for the key item in the tree, returning it.  It returns nil if
// unable to find that item.
func (t *BTree) Get(key Item) Item {
	if t.root == nil {
		return nil
	}
	return t.root.get(key)
}

// Min returns the smallest item in the tree, or nil if the tree is empty.
func (t *BTree) Min() Item {
	return min(t.root)
}

// Max returns the largest item in the tree, or nil if the tree is empty.
func (t *BTree) Max() Item {
	return max(t.root)
}

// Has returns true if the given key is in the tree.
func (t *BTree) Has(key Item) bool {
	return t.Get(key) != nil
}

// Len returns the number of items currently in the tree.
func (t *BTree) Len() int {
	return t.length
}

// ImmutableBTree is an immutable version of BTree safe to use with
// multiple goroutines.
type ImmutableBTree struct {
	op     *btreeOp
	length int
	root   *node
}

// NewImmutable creates a an empty, immutable btree with given degree.
//
// New(2), for example, will create a 2-3-4 tree (each node contains 1-3 items
// and 2-4 children).
func NewImmutable(degree int) *ImmutableBTree {
	if degree <= 1 {
		panic("bad degree")
	}
	return &ImmutableBTree{
		op: &btreeOp{
			degree: degree,
		},
	}
}

// AscendRange calls the iterator for every value in the tree within the range
// [greaterOrEqual, lessThan), until iterator returns false.
func (t *ImmutableBTree) AscendRange(greaterOrEqual, lessThan Item, iterator ItemIterator) {
	bt := (*BTree)(t)
	bt.AscendRange(greaterOrEqual, lessThan, iterator)
}

// AscendLessThan calls the iterator for every value in the tree within the range
// [first, pivot), until iterator returns false.
func (t *ImmutableBTree) AscendLessThan(pivot Item, iterator ItemIterator) {
	bt := (*BTree)(t)
	bt.AscendLessThan(pivot, iterator)
}

// AscendGreaterOrEqual calls the iterator for every value in the tree within
// the range [pivot, last], until iterator returns false.
func (t *ImmutableBTree) AscendGreaterOrEqual(pivot Item, iterator ItemIterator) {
	bt := (*BTree)(t)
	bt.AscendGreaterOrEqual(pivot, iterator)
}

// Ascend calls the iterator for every value in the tree within the range
// [first, last], until iterator returns false.
func (t *ImmutableBTree) Ascend(iterator ItemIterator) {
	bt := (*BTree)(t)
	bt.Ascend(iterator)
}

// DescendRange calls the iterator for every value in the tree within the range
// [lessOrEqual, greaterThan), until iterator returns false.
func (t *ImmutableBTree) DescendRange(lessOrEqual, greaterThan Item, iterator ItemIterator) {
	bt := (*BTree)(t)
	bt.DescendRange(lessOrEqual, greaterThan, iterator)
}

// DescendLessOrEqual calls the iterator for every value in the tree within the range
// [pivot, first], until iterator returns false.
func (t *ImmutableBTree) DescendLessOrEqual(pivot Item, iterator ItemIterator) {
	bt := (*BTree)(t)
	bt.DescendLessOrEqual(pivot, iterator)
}

// DescendGreaterThan calls the iterator for every value in the tree within
// the range (pivot, last], until iterator returns false.
func (t *ImmutableBTree) DescendGreaterThan(pivot Item, iterator ItemIterator) {
	bt := (*BTree)(t)
	bt.DescendGreaterThan(pivot, iterator)
}

// Descend calls the iterator for every value in the tree within the range
// [last, first], until iterator returns false.
func (t *ImmutableBTree) Descend(iterator ItemIterator) {
	bt := (*BTree)(t)
	bt.Descend(iterator)
}

// Get looks for the key item in the tree, returning it.  It returns nil if
// unable to find that item.
func (t *ImmutableBTree) Get(key Item) Item {
	bt := (*BTree)(t)
	return bt.Get(key)
}

// Min returns the smallest item in the tree, or nil if the tree is empty.
func (t *ImmutableBTree) Min() Item {
	return min(t.root)
}

// Max returns the largest item in the tree, or nil if the tree is empty.
func (t *ImmutableBTree) Max() Item {
	return max(t.root)
}

// Has returns true if the given key is in the tree.
func (t *ImmutableBTree) Has(key Item) bool {
	return t.Get(key) != nil
}

// Len returns the number of items currently in the tree.
func (t *ImmutableBTree) Len() int {
	return t.length
}

// Builder builds ImmutableBTree intances.
//
// A Builder instance has all the same methods as a BTree instance plus a
// Set method and a Build method. Set changes a builder instance to have the
// same Items and degree as a given ImmutableBTree instance; Build returns
// an ImmutableBTree instance that has the same Items and degree as the
// Builder instance.
//
// Calling Set on a Builder runs in constant time and space. Mutating
// methods on Builder instances employ copy-on-write. The first mutating
// method called on a Builder instance after a Set method will copy O(log N)
// nodes of the ImmutableBTree instance passed to the Set method. As the
// Builder instance acquires its own copies of more and more nodes from
// the original ImmutableBTree instance, each successive call to its mutating
// methods copies fewer and fewer nodes.
//
// Calling Build also runs in constant time and space. After calling Build,
// the Builder instance shares all of its nodes with the built ImmutableBTree
// instance. The next call to a mutable method on the Builder instance will
// copy O(log N) nodes.
//
// Write operations are not safe for concurrent mutation by multiple
// goroutines, but Read operations are.
type Builder struct {
	copied    bool
	tree      *ImmutableBTree
	writables copyOnWriteSet
}

// NewBuilder returns a new Builder initialised with tree.
func NewBuilder(tree *ImmutableBTree) *Builder {
	result := &Builder{}
	return result.Set(tree)
}

// AscendRange calls the iterator for every value in the tree within the range
// [greaterOrEqual, lessThan), until iterator returns false.
func (t *Builder) AscendRange(greaterOrEqual, lessThan Item, iterator ItemIterator) {
	bt := (*BTree)(t.tree)
	bt.AscendRange(greaterOrEqual, lessThan, iterator)
}

// AscendLessThan calls the iterator for every value in the tree within the range
// [first, pivot), until iterator returns false.
func (t *Builder) AscendLessThan(pivot Item, iterator ItemIterator) {
	bt := (*BTree)(t.tree)
	bt.AscendLessThan(pivot, iterator)
}

// AscendGreaterOrEqual calls the iterator for every value in the tree within
// the range [pivot, last], until iterator returns false.
func (t *Builder) AscendGreaterOrEqual(pivot Item, iterator ItemIterator) {
	bt := (*BTree)(t.tree)
	bt.AscendGreaterOrEqual(pivot, iterator)
}

// Ascend calls the iterator for every value in the tree within the range
// [first, last], until iterator returns false.
func (t *Builder) Ascend(iterator ItemIterator) {
	bt := (*BTree)(t.tree)
	bt.Ascend(iterator)
}

// DescendRange calls the iterator for every value in the tree within the range
// [lessOrEqual, greaterThan), until iterator returns false.
func (t *Builder) DescendRange(lessOrEqual, greaterThan Item, iterator ItemIterator) {
	bt := (*BTree)(t.tree)
	bt.DescendRange(lessOrEqual, greaterThan, iterator)
}

// DescendLessOrEqual calls the iterator for every value in the tree within the range
// [pivot, first], until iterator returns false.
func (t *Builder) DescendLessOrEqual(pivot Item, iterator ItemIterator) {
	bt := (*BTree)(t.tree)
	bt.DescendLessOrEqual(pivot, iterator)
}

// DescendGreaterThan calls the iterator for every value in the tree within
// the range (pivot, last], until iterator returns false.
func (t *Builder) DescendGreaterThan(pivot Item, iterator ItemIterator) {
	bt := (*BTree)(t.tree)
	bt.DescendGreaterThan(pivot, iterator)
}

// Descend calls the iterator for every value in the tree within the range
// [last, first], until iterator returns false.
func (t *Builder) Descend(iterator ItemIterator) {
	bt := (*BTree)(t.tree)
	bt.Descend(iterator)
}

// Get looks for the key item in the tree, returning it.  It returns nil if
// unable to find that item.
func (t *Builder) Get(key Item) Item {
	bt := (*BTree)(t.tree)
	return bt.Get(key)
}

// Min returns the smallest item in the tree, or nil if the tree is empty.
func (t *Builder) Min() Item {
	return min(t.tree.root)
}

// Max returns the largest item in the tree, or nil if the tree is empty.
func (t *Builder) Max() Item {
	return max(t.tree.root)
}

// Has returns true if the given key is in the tree.
func (t *Builder) Has(key Item) bool {
	return t.tree.Get(key) != nil
}

// Len returns the number of items currently in the tree.
func (t *Builder) Len() int {
	return t.tree.length
}

// Set sets this Builder to tree and returns a reference to itself.
func (t *Builder) Set(tree *ImmutableBTree) *Builder {
	t.tree = tree
	t.copied = false
	t.writables = make(copyOnWriteSet)
	return t
}

// ReplaceOrInsert adds the given item to the tree.  If an item in the tree
// already equals the given one, it is removed from the tree and returned.
// Otherwise, nil is returned.
//
// nil cannot be added to the tree (will panic).
func (t *Builder) ReplaceOrInsert(item Item) Item {
	bt := t.writableBTree()
	return bt.replaceOrInsert(item, t.writables)
}

// Delete removes an item equal to the passed in item from the tree, returning
// it.  If no such item exists, returns nil.
func (t *Builder) Delete(item Item) Item {
	bt := t.writableBTree()
	return bt.deleteItem(item, removeItem, t.writables)
}

// DeleteMin removes the smallest item in the tree and returns it.
// If no such item exists, returns nil.
func (t *Builder) DeleteMin() Item {
	bt := t.writableBTree()
	return bt.deleteItem(nil, removeMin, t.writables)
}

// DeleteMax removes the largest item in the tree and returns it.
// If no such item exists, returns nil.
func (t *Builder) DeleteMax() Item {
	bt := t.writableBTree()
	return bt.deleteItem(nil, removeMax, t.writables)
}

// Build returns the immutable btree.
func (t *Builder) Build() *ImmutableBTree {
	result := t.tree
	t.Set(result)
	return result
}

func (t *Builder) writableBTree() *BTree {
	if !t.copied {
		t.copied = true
		acopy := *t.tree
		t.tree = &acopy
	}
	return (*BTree)(t.tree)
}

// Int implements the Item interface for integers.
type Int int

// Less returns true if int(a) < int(b).
func (a Int) Less(b Item) bool {
	return a < b.(Int)
}

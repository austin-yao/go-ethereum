// Copyright 2022 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package trie

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unsafe"
	"os"
	"encoding/json"

	"github.com/ethereum/go-ethereum/common"
)

// memoryNode is all the information we know about a single cached trie node
// in the memory.
type memoryNode struct {
	hash common.Hash // Node hash, computed by hashing rlp value, empty for deleted nodes
	size uint16      // Byte size of the useful cached data, 0 for deleted nodes
	node node        // Cached collapsed trie node, or raw rlp data, nil for deleted nodes
}

// memoryNodeSize is the raw size of a memoryNode data structure without any
// node data included. It's an approximate size, but should be a lot better
// than not counting them.
// nolint:unused
var memoryNodeSize = int(reflect.TypeOf(memoryNode{}).Size())

// memorySize returns the total memory size used by this node.
// nolint:unused
func (n *memoryNode) memorySize(pathlen int) int {
	return int(n.size) + memoryNodeSize + pathlen
}

// rlp returns the raw rlp encoded blob of the cached trie node, either directly
// from the cache, or by regenerating it from the collapsed node.
// nolint:unused
func (n *memoryNode) rlp() []byte {
	if node, ok := n.node.(rawNode); ok {
		return node
	}
	return nodeToBytes(n.node)
}

// obj returns the decoded and expanded trie node, either directly from the cache,
// or by regenerating it from the rlp encoded blob.
// nolint:unused
func (n *memoryNode) obj() node {
	if node, ok := n.node.(rawNode); ok {
		return mustDecodeNode(n.hash[:], node)
	}
	return expandNode(n.hash[:], n.node)
}

// isDeleted returns the indicator if the node is marked as deleted.
func (n *memoryNode) isDeleted() bool {
	return n.hash == (common.Hash{})
}

// nodeWithPrev wraps the memoryNode with the previous node value.
// nolint: unused
type nodeWithPrev struct {
	*memoryNode
	prev []byte // RLP-encoded previous value, nil means it's non-existent
}

// unwrap returns the internal memoryNode object.
// nolint:unused
func (n *nodeWithPrev) unwrap() *memoryNode {
	return n.memoryNode
}

// memorySize returns the total memory size used by this node. It overloads
// the function in memoryNode by counting the size of previous value as well.
// nolint: unused
func (n *nodeWithPrev) memorySize(pathlen int) int {
	return n.memoryNode.memorySize(pathlen) + len(n.prev)
}

// NodeSet contains all dirty nodes collected during the commit operation.
// Each node is keyed by path. It's not thread-safe to use.
type NodeSet struct {
	owner   common.Hash            // the identifier of the trie
	// nodes is a mapping to a memoryNode
	// what does the string represent? just a path?
	// we need to keep track of the path, it is important
	nodes   map[string]*memoryNode // the set of dirty nodes(inserted, updated, deleted)
	leaves  []*leaf                // the list of dirty leaves
	updates int                    // the count of updated and inserted nodes
	deletes int                    // the count of deleted nodes

	// my new thing
	totalSize int
	touched map[string]bool
	sizes map[string]int

	// The list of accessed nodes, which records the original node value.
	// The origin value is expected to be nil for newly inserted node
	// and is expected to be non-nil for other types(updated, deleted).
	accessList map[string][]byte
}

// NewNodeSet initializes an empty node set to be used for tracking dirty nodes
// from a specific account or storage trie. The owner is zero for the account
// trie and the owning account address hash for storage tries.
func NewNodeSet(owner common.Hash, accessList map[string][]byte) *NodeSet {
	return &NodeSet{
		owner:      owner,
		nodes:      make(map[string]*memoryNode),
		accessList: accessList,
		touched: make(map[string]bool),
		sizes: make(map[string]int),
	}
}

// forEachWithOrder iterates the dirty nodes with the order from bottom to top,
// right to left, nodes with the longest path will be iterated first.
func (set *NodeSet) forEachWithOrder(callback func(path string, n *memoryNode)) {
	var paths sort.StringSlice
	for path := range set.nodes {
		paths = append(paths, path)
	}
	// Bottom-up, longest path first
	sort.Sort(sort.Reverse(paths))
	for _, path := range paths {
		callback(path, set.nodes[path])
	}
}

// markUpdated marks the node as dirty(newly-inserted or updated).
// a lot of the updating in this function is no longer necessary,
// since we only need to keep track of the touched nodes now.
// keeping just in case we need to revert
func (set *NodeSet) markUpdated(path []byte, node *memoryNode) {
	set.nodes[string(path)] = node
	set.updates += 1
	
	if set.touched[string(path)] == false {
		f, err := os.OpenFile("data/nodes.txt", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			panic(err)
		}
		defer f.Close()

		jsonData, err := json.Marshal(node.hash)
		if err != nil {
			fmt.Println(err)
		} else {
			f.WriteString(string(jsonData) + "\n")
		}

		set.totalSize += int(unsafe.Sizeof(*node))
		set.touched[string(path)] = true
		set.sizes[string(path)] = int(unsafe.Sizeof(*node))
	} else {
		// updating the totalSize to be the most recent
		set.totalSize -= set.sizes[string(path)]
		set.sizes[string(path)] = int(unsafe.Sizeof(*node))
		set.totalSize += int(unsafe.Sizeof(node))
	}
}

// markDeleted marks the node as deleted.
func (set *NodeSet) markDeleted(path []byte) {
	// this is the node that was deleted
	// node := set.nodes[string(path)]
	set.nodes[string(path)] = &memoryNode{}
	set.deletes += 1

	if set.touched[string(path)] == false {
		// these are just empty nodes
		set.totalSize += int(unsafe.Sizeof(memoryNode{}))
		set.touched[string(path)] = true
		set.sizes[string(path)] = int(unsafe.Sizeof(memoryNode{}))
	} else {
		set.totalSize -= set.sizes[string(path)]
		set.sizes[string(path)] = int(unsafe.Sizeof(memoryNode{}))
		set.totalSize += int(unsafe.Sizeof(memoryNode{}))
	}
}

// addLeaf collects the provided leaf node into set.
func (set *NodeSet) addLeaf(node *leaf) {
	set.leaves = append(set.leaves, node)
}

// Size returns the number of dirty nodes in set.
func (set *NodeSet) Size() (int, int) {
	return set.updates, set.deletes
}

// new function
func (set *NodeSet) TotalSize() (int) {
	set.touched = make(map[string]bool)
	set.sizes = make(map[string]int)
	ans := set.totalSize
	set.totalSize = 0
	return ans;
}

// Hashes returns the hashes of all updated nodes. TODO(rjl493456442) how can
// we get rid of it?
func (set *NodeSet) Hashes() []common.Hash {
	var ret []common.Hash
	for _, node := range set.nodes {
		ret = append(ret, node.hash)
	}
	return ret
}

// Summary returns a string-representation of the NodeSet.
func (set *NodeSet) Summary() string {
	var out = new(strings.Builder)
	fmt.Fprintf(out, "nodeset owner: %v\n", set.owner)
	if set.nodes != nil {
		for path, n := range set.nodes {
			// Deletion
			if n.isDeleted() {
				fmt.Fprintf(out, "  [-]: %x prev: %x\n", path, set.accessList[path])
				continue
			}
			// Insertion
			origin, ok := set.accessList[path]
			if !ok {
				fmt.Fprintf(out, "  [+]: %x -> %v\n", path, n.hash)
				continue
			}
			// Update
			fmt.Fprintf(out, "  [*]: %x -> %v prev: %x\n", path, n.hash, origin)
		}
	}
	for _, n := range set.leaves {
		fmt.Fprintf(out, "[leaf]: %v\n", n)
	}
	return out.String()
}

// MergedNodeSet represents a merged dirty node set for a group of tries.
type MergedNodeSet struct {
	sets map[common.Hash]*NodeSet
}

// NewMergedNodeSet initializes an empty merged set.
func NewMergedNodeSet() *MergedNodeSet {
	return &MergedNodeSet{sets: make(map[common.Hash]*NodeSet)}
}

// NewWithNodeSet constructs a merged nodeset with the provided single set.
func NewWithNodeSet(set *NodeSet) *MergedNodeSet {
	merged := NewMergedNodeSet()
	merged.Merge(set)
	return merged
}

// Merge merges the provided dirty nodes of a trie into the set. The assumption
// is held that no duplicated set belonging to the same trie will be merged twice.
// isn't actually owning, just changing the parent pointer.
func (set *MergedNodeSet) Merge(other *NodeSet) error {
	_, present := set.sets[other.owner]
	if present {
		return fmt.Errorf("duplicate trie for owner %#x", other.owner)
	}
	set.sets[other.owner] = other
	return nil
}
// <------ MY FUNCTIONS ----->

// gets the size of all the nodes
func (set *MergedNodeSet) Size() (int, int) {
	var updates = 0
	var deleted = 0
	for _, other_set := range set.sets {
		numUpdated, numDeleted := other_set.Size()
		updates += numUpdated
		deleted += numDeleted
	}

	return updates, deleted
}

func (set *MergedNodeSet) TotalSize() (int) {
	var totalSize = 0
	for _, other_set := range set.sets {
		// totalSize changes each time (will get erased)
		// will only record the size of the most recent block
		// we need something global
		sizeToAdd := other_set.TotalSize()
		totalSize += sizeToAdd
	}

	return totalSize
}

// Adds the size of newly dirty nodes to our totalSize by iterating through touched nodes.
func (set *MergedNodeSet) Combine(otherMerged *MergedNodeSet, nodeSizes map[string]int, cumSize int) (error, int) {
	// var size = 0
	for _, other := range otherMerged.sets {
		// checking each nodeSet in otherMerged.sets
		// lets iterate through each touched node. this will get slow!!
		for path, _ := range other.touched {
			value, exists := nodeSizes[path]
			if exists {
				cumSize -= value
				currNode := other.nodes[path]
				cumSize += int(unsafe.Sizeof(*currNode))
				nodeSizes[path] = int(unsafe.Sizeof(*currNode))
			} else {
				currNode := other.nodes[path]
				cumSize += int(unsafe.Sizeof(*currNode))
				nodeSizes[path] = int(unsafe.Sizeof(*currNode))
			}
		}
	}
	return nil, cumSize
}

// counts the number of distinct nodes. not used, only for testing
func (set *MergedNodeSet) countNodes(otherMerged *MergedNodeSet, nodeSizes map[string]int, cumSize int) (error, int) {
	var size = 0
	for _, other := range otherMerged.sets {
		_, present := set.sets[other.owner]
		if present == false {
			set.sets[other.owner] = other
			size += int(unsafe.Sizeof(other))
		} else {
			for path, node := range other.nodes {
				if exists := set.sets[other.owner].nodes[path]; exists == nil {
					set.sets[other.owner].nodes[path] = node
					size += int(unsafe.Sizeof(node))
				}
			}
		}
	}
	return nil, size
}

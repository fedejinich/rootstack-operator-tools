package rsktrie

import (
	"container/list"
)

// IterationElement represents a node and its path during iteration.
type IterationElement struct {
	nodeKey *TrieKeySlice
	node    *Trie
}

func NewIterationElement(nodeKey *TrieKeySlice, node *Trie) *IterationElement {
	return &IterationElement{nodeKey: nodeKey, node: node}
}

func (ie *IterationElement) GetNode() *Trie {
	return ie.node
}

func (ie *IterationElement) GetNodeKey() *TrieKeySlice {
	return ie.nodeKey
}

func (ie *IterationElement) String() string {
	encodedFullKey := ie.nodeKey.Expand()
	out := make([]byte, len(encodedFullKey))
	for i, b := range encodedFullKey {
		if b == 0 {
			out[i] = '0'
		} else {
			out[i] = '1'
		}
	}
	return string(out)
}

// InOrderIterator traverses the trie in-order.
type InOrderIterator struct {
	visiting *list.List // Stack
}

func NewInOrderIterator(root *Trie) *InOrderIterator {
	it := &InOrderIterator{visiting: list.New()}
	traversedPath := root.sharedPath
	it.visiting.PushFront(NewIterationElement(traversedPath, root)) // Push root
	it.pushLeftmostNode(traversedPath, root)
	return it
}

func (it *InOrderIterator) HasNext() bool {
	return it.visiting.Len() > 0
}

func (it *InOrderIterator) Next() *IterationElement {
	if it.visiting.Len() == 0 {
		return nil
	}
	element := it.visiting.Remove(it.visiting.Front()).(*IterationElement)
	node := element.node

	rightNode := node.RetrieveNode(1)
	if rightNode != nil {
		rightNodeKey := element.nodeKey.RebuildSharedPath(1, rightNode.sharedPath)
		it.visiting.PushFront(NewIterationElement(rightNodeKey, rightNode))
		it.pushLeftmostNode(rightNodeKey, rightNode)
	}
	return element
}

func (it *InOrderIterator) pushLeftmostNode(nodeKey *TrieKeySlice, node *Trie) {
	leftNode := node.RetrieveNode(0)
	if leftNode != nil {
		leftNodeKey := nodeKey.RebuildSharedPath(0, leftNode.sharedPath)
		it.visiting.PushFront(NewIterationElement(leftNodeKey, leftNode))
		it.pushLeftmostNode(leftNodeKey, leftNode)
	}
}

// PreOrderIterator traverses the trie pre-order.
type PreOrderIterator struct {
	visiting *list.List
}

func NewPreOrderIterator(root *Trie) *PreOrderIterator {
	it := &PreOrderIterator{visiting: list.New()}
	traversedPath := root.sharedPath
	it.visiting.PushFront(NewIterationElement(traversedPath, root))
	return it
}

func (it *PreOrderIterator) HasNext() bool {
	return it.visiting.Len() > 0
}

func (it *PreOrderIterator) Next() *IterationElement {
	if it.visiting.Len() == 0 {
		return nil
	}
	element := it.visiting.Remove(it.visiting.Front()).(*IterationElement)
	node := element.node
	nodeKey := element.nodeKey

	// Push right then left (LIFO stack)
	rightNode := node.RetrieveNode(1)
	if rightNode != nil {
		rightNodeKey := nodeKey.RebuildSharedPath(1, rightNode.sharedPath)
		it.visiting.PushFront(NewIterationElement(rightNodeKey, rightNode))
	}

	leftNode := node.RetrieveNode(0)
	if leftNode != nil {
		leftNodeKey := nodeKey.RebuildSharedPath(0, leftNode.sharedPath)
		it.visiting.PushFront(NewIterationElement(leftNodeKey, leftNode))
	}

	return element
}

// PostOrderIterator traverses the trie post-order.
type PostOrderIterator struct {
	visiting           *list.List
	visitingRightChild *list.List
}

func NewPostOrderIterator(root *Trie) *PostOrderIterator {
	it := &PostOrderIterator{
		visiting:           list.New(),
		visitingRightChild: list.New(),
	}
	traversedPath := root.sharedPath
	it.visiting.PushFront(NewIterationElement(traversedPath, root))
	it.visitingRightChild.PushFront(false)
	it.pushLeftmostNodeRecord(traversedPath, root)
	return it
}

func (it *PostOrderIterator) HasNext() bool {
	return it.visiting.Len() > 0
}

func (it *PostOrderIterator) Next() *IterationElement {
	if it.visiting.Len() == 0 {
		return nil
	}

	// Peek
	element := it.visiting.Front().Value.(*IterationElement)
	node := element.node

	rightNode := node.RetrieveNode(1)
	visitedRight := it.visitingRightChild.Front().Value.(bool)

	if rightNode == nil || visitedRight {
		it.visiting.Remove(it.visiting.Front())
		it.visitingRightChild.Remove(it.visitingRightChild.Front())
		return element
	} else {
		// Visit right subtree
		it.visitingRightChild.Remove(it.visitingRightChild.Front())
		it.visitingRightChild.PushFront(true) // Mark as visited right

		rightNodeKey := element.nodeKey.RebuildSharedPath(1, rightNode.sharedPath)
		it.visiting.PushFront(NewIterationElement(rightNodeKey, rightNode))
		it.visitingRightChild.PushFront(false) // New node, right not visited

		it.pushLeftmostNodeRecord(rightNodeKey, rightNode)
		return it.Next()
	}
}

func (it *PostOrderIterator) pushLeftmostNodeRecord(nodeKey *TrieKeySlice, node *Trie) {
	leftNode := node.RetrieveNode(0)
	if leftNode != nil {
		leftNodeKey := nodeKey.RebuildSharedPath(0, leftNode.sharedPath)
		it.visiting.PushFront(NewIterationElement(leftNodeKey, leftNode))
		it.visitingRightChild.PushFront(false)
		it.pushLeftmostNodeRecord(leftNodeKey, leftNode)
	}
}

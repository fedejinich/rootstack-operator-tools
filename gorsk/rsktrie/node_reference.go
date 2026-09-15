package rsktrie

import (
	"bytes"
	"log"
)

type NodeReference struct {
	store    TrieStore
	lazyNode *Trie
	lazyHash []byte
}

func NewNodeReference(store TrieStore, node *Trie, hash []byte) *NodeReference {
	nr := &NodeReference{store: store, lazyNode: node, lazyHash: hash}
	// If node is provided and empty, reset
	if node != nil && node.IsEmptyTrie() {
		nr.lazyNode = nil
		nr.lazyHash = nil
	}
	return nr
}

func NodeReferenceEmpty() *NodeReference {
	return &NodeReference{}
}

func (n *NodeReference) IsEmpty() bool {
	return n.lazyHash == nil && n.lazyNode == nil
}

// GetHash returns the hash. Calculates if missing. Returns a copy so
// callers cannot mutate the cached lazyHash.
func (n *NodeReference) GetHash() []byte {
	if n.lazyHash == nil {
		if n.lazyNode == nil {
			return nil
		}
		n.lazyHash = n.lazyNode.GetHash()
	}
	out := make([]byte, len(n.lazyHash))
	copy(out, n.lazyHash)
	return out
}

// GetNode returns the node. Retrieves from store if missing.
func (n *NodeReference) GetNode() *Trie {
	if n.lazyNode != nil {
		return n.lazyNode
	}

	if n.lazyHash == nil {
		return nil
	}

	// Verify-only callers (e.g. proof deserialization with no backing store)
	// construct NodeReferences with store=nil. Treat that as "node not
	// resolvable" instead of panicking.
	if n.store == nil {
		return nil
	}

	n.lazyNode = n.store.Retrieve(n.lazyHash)
	if n.lazyNode == nil {
		log.Printf("Broken database: missing node for hash %x", n.lazyHash)
		// panic("Broken database") // OR return nil
		return nil
	}

	return n.lazyNode
}

func (n *NodeReference) SerializeInto(buf *bytes.Buffer) {
	if !n.IsEmpty() {
		if n.IsEmbeddable() {
			serialized := n.GetSerialized()
			buf.Write(NewVarInt(uint64(len(serialized))).Encode())
			buf.Write(serialized)
		} else {
			h := n.GetHash()
			if h == nil {
				panic("Hash should exist")
			}
			buf.Write(h)
		}
	}
}

func (n *NodeReference) GetSerialized() []byte {
	return n.lazyNode.ToMessage()
}

func (n *NodeReference) IsEmbeddable() bool {
	if n.lazyNode == nil {
		return false
	}
	return n.lazyNode.IsEmbeddable()
}

func (n *NodeReference) ReferenceSize() int {
	if node := n.GetNode(); node != nil {
		// Java: trie.getChildrenSize().value + externalValueLength + trie.getMessageLength();
		var externalValueLength int = 0
		if node.HasLongValue() {
			externalValueLength = node.valueLength.Int()
		}
		// Go trie.GetChildrenSize() returns *VarInt
		return int(node.GetChildrenSize().Value) + externalValueLength + node.GetMessageLength()
	}
	return 0
}

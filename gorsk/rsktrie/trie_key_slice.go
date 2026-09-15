package rsktrie

// TrieKeySlice represents an immutable slice of a trie key.
type TrieKeySlice struct {
	expandedKey []byte
	offset      int
	limit       int
}

func NewTrieKeySlice(expandedKey []byte, offset, limit int) *TrieKeySlice {
	return &TrieKeySlice{
		expandedKey: expandedKey,
		offset:      offset,
		limit:       limit,
	}
}

func TrieKeySliceFromKey(key []byte) *TrieKeySlice {
	if key == nil {
		return TrieKeySliceEmpty()
	}
	expandedKey := PathEncoderDecode(key, len(key)*8)
	return NewTrieKeySlice(expandedKey, 0, len(expandedKey))
}

func TrieKeySliceFromEncoded(src []byte, offset, keyLength, encodedLength int) *TrieKeySlice {
	encodedKey := make([]byte, encodedLength)
	copy(encodedKey, src[offset:offset+encodedLength])
	expandedKey := PathEncoderDecode(encodedKey, keyLength)
	return NewTrieKeySlice(expandedKey, 0, len(expandedKey))
}

// TrieKeySliceFromEncodedFull creates a TrieKeySlice from already-extracted encoded bytes
func TrieKeySliceFromEncodedFull(encodedBytes []byte, keyLength int) *TrieKeySlice {
	expandedKey := PathEncoderDecode(encodedBytes, keyLength)
	return NewTrieKeySlice(expandedKey, 0, len(expandedKey))
}

func TrieKeySliceEmpty() *TrieKeySlice {
	return NewTrieKeySlice([]byte{}, 0, 0)
}

func (t *TrieKeySlice) Length() int {
	return t.limit - t.offset
}

func (t *TrieKeySlice) Get(i int) byte {
	return t.expandedKey[t.offset+i]
}

func (t *TrieKeySlice) Encode() []byte {
	// Copy the slice to avoid exposing internal array in encode (if PathEncoder modified it, but it doesn't)
	// Actually PathEncoder creates new array.
	slice := t.expandedKey[t.offset:t.limit]
	return PathEncoderEncode(slice)
}

func (t *TrieKeySlice) Slice(from, to int) *TrieKeySlice {
	if from < 0 {
		panic("The start position must not be lower than 0")
	}
	if from > to {
		panic("The start position must not be greater than the end position")
	}

	newOffset := t.offset + from
	if newOffset > t.limit {
		panic("The start position must not exceed the key length")
	}

	newLimit := t.offset + to
	if newLimit > t.limit {
		panic("The end position must not exceed the key length")
	}

	return NewTrieKeySlice(t.expandedKey, newOffset, newLimit)
}

func (t *TrieKeySlice) CommonPath(other *TrieKeySlice) *TrieKeySlice {
	l := t.Length()
	if other.Length() < l {
		l = other.Length()
	}

	for i := 0; i < l; i++ {
		if t.Get(i) != other.Get(i) {
			return t.Slice(0, i)
		}
	}
	return t.Slice(0, l)
}

func (t *TrieKeySlice) RebuildSharedPath(implicitByte byte, childSharedPath *TrieKeySlice) *TrieKeySlice {
	length := t.Length()
	childLength := childSharedPath.Length()
	newLength := length + 1 + childLength

	newExpandedKey := make([]byte, newLength)
	// Copy this
	copy(newExpandedKey[0:], t.expandedKey[t.offset:t.limit])
	// Set implicit
	newExpandedKey[length] = implicitByte
	// Copy child
	copy(newExpandedKey[length+1:], childSharedPath.expandedKey[childSharedPath.offset:childSharedPath.limit])

	return NewTrieKeySlice(newExpandedKey, 0, newLength)
}

func (t *TrieKeySlice) LeftPad(paddingLength int) *TrieKeySlice {
	if paddingLength == 0 {
		return t
	}
	currentLength := t.Length()
	paddedExpandedKey := make([]byte, currentLength+paddingLength)
	copy(paddedExpandedKey[paddingLength:], t.expandedKey[t.offset:t.limit])
	return NewTrieKeySlice(paddedExpandedKey, 0, len(paddedExpandedKey))
}

func (t *TrieKeySlice) Expand() []byte {
	ex := make([]byte, t.Length())
	copy(ex, t.expandedKey[t.offset:t.limit])
	return ex
}

// PathEncoder helpers

func PathEncoderEncode(path []byte) []byte {
	if path == nil {
		panic("path is null")
	}
	lpath := len(path)
	lencoded := (lpath / 8)
	if lpath%8 != 0 {
		lencoded++
	}

	encoded := make([]byte, lencoded)
	nbyte := 0

	for k := 0; k < lpath; k++ {
		offset := k % 8
		if k > 0 && offset == 0 {
			nbyte++
		}

		if path[k] == 0 {
			continue
		}

		encoded[nbyte] |= 0x80 >> offset
	}
	return encoded
}

func PathEncoderDecode(encoded []byte, bitLength int) []byte {
	if encoded == nil {
		panic("encoded is null")
	}
	path := make([]byte, bitLength)

	for k := 0; k < bitLength; k++ {
		nbyte := k / 8
		offset := k % 8

		if (encoded[nbyte]>>(7-offset))&0x01 != 0 {
			path[k] = 1
		}
	}
	return path
}

func CalculateEncodedLength(keyLength int) int {
	l := keyLength / 8
	if keyLength%8 != 0 {
		l++
	}
	return l
}

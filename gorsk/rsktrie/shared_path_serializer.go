package rsktrie

import (
	"bytes"
)

type SharedPathSerializer struct {
	sharedPath *TrieKeySlice
	lshared    int
}

func NewSharedPathSerializer(sharedPath *TrieKeySlice) *SharedPathSerializer {
	return &SharedPathSerializer{
		sharedPath: sharedPath,
		lshared:    sharedPath.Length(),
	}
}

func (s *SharedPathSerializer) SerializedLength() int {
	if !s.IsPresent() {
		return 0
	}
	return s.LsharedSize() + CalculateEncodedLength(s.lshared)
}

func (s *SharedPathSerializer) IsPresent() bool {
	return s.lshared > 0
}

func (s *SharedPathSerializer) SerializeInto(buf *bytes.Buffer) {
	SerializeIntoSharedPath(s.sharedPath, buf)
}

func SerializeIntoSharedPath(sharedPath *TrieKeySlice, buf *bytes.Buffer) {
	if !SharedPathIsPresent(sharedPath) {
		return
	}
	lshared := sharedPath.Length()
	encoded := sharedPath.Encode()
	SerializeBytes(buf, lshared, encoded)
}

func SharedPathIsPresent(sharedPath *TrieKeySlice) bool {
	return sharedPath.Length() > 0
}

func SerializeBytes(buf *bytes.Buffer, lshared int, encode []byte) {
	if 1 <= lshared && lshared <= 32 {
		buf.WriteByte(byte(lshared - 1))
	} else if 160 <= lshared && lshared <= 382 {
		buf.WriteByte(byte(lshared - 128))
	} else {
		buf.WriteByte(255)
		buf.Write(NewVarInt(uint64(lshared)).Encode())
	}
	buf.Write(encode)
}

func (s *SharedPathSerializer) LsharedSize() int {
	if !s.IsPresent() {
		return 0
	}
	return CalculateVarIntSize(s.lshared)
}

func CalculateVarIntSize(lshared int) int {
	if 1 <= lshared && lshared <= 32 {
		return 1
	}
	if 160 <= lshared && lshared <= 382 {
		return 1
	}
	return 1 + NewVarInt(uint64(lshared)).Size
}

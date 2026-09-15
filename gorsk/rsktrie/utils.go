package rsktrie

import (
	"encoding/binary"
	"fmt"
)

// Uint24 represents a 24-bit unsigned integer.
type Uint24 uint32

const Uint24Bytes = 3

func (u Uint24) Int() int {
	return int(u)
}

func (u Uint24) Encode() []byte {
	b := make([]byte, 3)
	b[0] = byte(u >> 16)
	b[1] = byte(u >> 8)
	b[2] = byte(u)
	return b
}

func DecodeUint24(b []byte, offset int) (Uint24, error) {
	if offset < 0 || offset+Uint24Bytes > len(b) {
		return 0, fmt.Errorf("decode uint24: need %d bytes at offset %d, buffer length %d", Uint24Bytes, offset, len(b))
	}
	val := uint32(b[offset])<<16 | uint32(b[offset+1])<<8 | uint32(b[offset+2])
	return Uint24(val), nil
}

// VarInt represents a variable-length integer (Bitcoin style).
type VarInt struct {
	Value uint64
	Size  int
}

func NewVarInt(val uint64) VarInt {
	var size int
	if val < 253 {
		size = 1
	} else if val <= 0xFFFF {
		size = 3
	} else if val <= 0xFFFFFFFF {
		size = 5
	} else {
		size = 9
	}
	return VarInt{Value: val, Size: size}
}

func ReadVarInt(buf []byte, offset int) (VarInt, error) {
	if len(buf) <= offset {
		return VarInt{}, fmt.Errorf("buffer too short: len=%d offset=%d", len(buf), offset)
	}
	first := buf[offset]
	if first < 253 {
		return VarInt{Value: uint64(first), Size: 1}, nil
	} else if first == 253 {
		if len(buf) < offset+3 {
			return VarInt{}, fmt.Errorf("buffer too short for VarInt16")
		}
		val := binary.LittleEndian.Uint16(buf[offset+1 : offset+3])
		return VarInt{Value: uint64(val), Size: 3}, nil
	} else if first == 254 {
		if len(buf) < offset+5 {
			return VarInt{}, fmt.Errorf("buffer too short for VarInt32")
		}
		val := binary.LittleEndian.Uint32(buf[offset+1 : offset+5])
		return VarInt{Value: uint64(val), Size: 5}, nil
	} else {
		if len(buf) < offset+9 {
			return VarInt{}, fmt.Errorf("buffer too short for VarInt64")
		}
		val := binary.LittleEndian.Uint64(buf[offset+1 : offset+9])
		return VarInt{Value: val, Size: 9}, nil
	}
}

func (v VarInt) Encode() []byte {
	if v.Value < 253 {
		return []byte{byte(v.Value)}
	} else if v.Value <= 0xFFFF {
		b := make([]byte, 3)
		b[0] = 253
		binary.LittleEndian.PutUint16(b[1:], uint16(v.Value))
		return b
	} else if v.Value <= 0xFFFFFFFF {
		b := make([]byte, 5)
		b[0] = 254
		binary.LittleEndian.PutUint32(b[1:], uint32(v.Value))
		return b
	} else {
		b := make([]byte, 9)
		b[0] = 255
		binary.LittleEndian.PutUint64(b[1:], v.Value)
		return b
	}
}

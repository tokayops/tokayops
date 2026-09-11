// Package keys is the identity grammar of outbound delivery: the byte
// encodings that decide when two things are the same commitment, the same
// admission, or the same content.
//
// # The encoding is the contract
//
// These bytes end up in rows that outlive everything around them - an
// admission claim is never deleted - so a change to the encoding is a change of
// identity for every row already written. That is why the grammar is spelled
// out here rather than derived from a struct, why every protocol carries a
// literal naming itself, and why the tests pin the bytes with golden vectors
// instead of comparing one implementation to another.
//
//	be32(n)      = n as an unsigned 32-bit big-endian integer, exactly 4 bytes
//	be64(n)      = n as a SIGNED 64-bit big-endian integer, exactly 8 bytes
//	enc(b)       = be32(len(b)) || b
//	encOpt(x)    = enc("0") when absent, enc("1") || enc(x) when present
//	list(v)      = be32(len(v)) || enc(v[0]) || ... || enc(v[len(v)-1])
//	tagged(n, b) = be32(n) || b
//
// Length before value, on every field including times, is what keeps two
// fields from bleeding into one another: without it {"a", "bc"} and {"ab", "c"}
// are the same bytes, and a one-member list "x,y" is a two-member list "x",
// "y". Times are integers of nanoseconds rather than formatted strings because
// a format drops trailing zeros, which would make the length depend on the
// value and any later format change a silent change of identity.
//
// Absence and emptiness are different values: encOpt says which one it is, so
// "no message override" and "an override that is the empty string" cannot
// collide.
//
// # What never enters
//
// Process clocks. Not now(), not the moment of encoding, not a timestamp
// derived from either. An identity that moves with the wall clock is not an
// identity, and a repeat of the same admission after a delay would be a
// different one.
//
// # Why this package has its own primitives
//
// The job dedup codec encodes the same way, and its grammar is pinned by a
// golden vector of its own. It is also being removed. Importing a package on
// its way out to build the identities that replace it would mean rewriting
// every call site when it goes; thirty lines live here instead, and the two
// stay byte-identical because both are pinned.
package keys

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// ErrContract is what every refusal in this package wraps.
//
// A caller cannot recover from any of them by retrying: an unknown enum
// literal, an empty identifier or an oversized request id is a statement that
// does not exist in this grammar, and encoding it anyway would produce an
// identity nothing else can reproduce.
var ErrContract = errors.New("outbound keys: contract violation")

func contractf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrContract, fmt.Sprintf(format, args...))
}

// enc writes a length-prefixed field.
func enc(buf *bytes.Buffer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	buf.Write(length[:])
	buf.Write(value)
}

// encStr is enc for the common case of a string field.
func encStr(buf *bytes.Buffer, value string) {
	enc(buf, []byte(value))
}

// encOpt distinguishes absence from emptiness, in that order: the marker
// first, so a present empty value and an absent one differ in the first four
// bytes rather than in a length nobody reads.
func encOpt(buf *bytes.Buffer, value *string) {
	if value == nil {
		encStr(buf, "0")
		return
	}
	encStr(buf, "1")
	encStr(buf, *value)
}

// encOptBytes is encOpt for a digest or any other byte field.
func encOptBytes(buf *bytes.Buffer, value []byte) {
	if value == nil {
		encStr(buf, "0")
		return
	}
	encStr(buf, "1")
	enc(buf, value)
}

// encOptInt64 is encOpt for a number that may be absent - a revision that was
// never applied is not revision zero.
func encOptInt64(buf *bytes.Buffer, value *int64) {
	if value == nil {
		encStr(buf, "0")
		return
	}
	encStr(buf, "1")
	enc(buf, int64Bytes(*value))
}

// encBool writes a boolean as one of two fixed literals. Not a Go bool
// formatted by whatever the standard library does today: the spelling is part
// of the identity.
func encBool(buf *bytes.Buffer, value bool) {
	if value {
		encStr(buf, "1")
		return
	}
	encStr(buf, "0")
}

// encList writes a count and then every member, each length-prefixed. The
// caller decides the order; nothing here sorts, because the order of a list is
// part of what is being identified and hiding a sort in the codec would make
// two different states hash the same.
func encList(buf *bytes.Buffer, values [][]byte) {
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], uint32(len(values)))
	buf.Write(count[:])
	for _, v := range values {
		enc(buf, v)
	}
}

// tagged writes a field's assigned number before it.
//
// The number is not a skip marker: a field's body is not length-prefixed as a
// whole - several of them write more than one value - so a reader that meets a
// tag it does not know cannot step over it. What the number buys is that
// position and meaning cannot drift apart: reordering two fields, or reusing a
// number for something else, changes the bytes and is therefore visible. A
// field nobody knows about means the material was written by another version of
// the protocol, which is a new version with its own encoder rather than
// something to be skipped.
func tagged(buf *bytes.Buffer, tag uint32, write func(*bytes.Buffer)) {
	var number [4]byte
	binary.BigEndian.PutUint32(number[:], tag)
	buf.Write(number[:])
	write(buf)
}

// int64Bytes is the eight-byte form of a signed integer - times in
// nanoseconds, offsets, revisions and indexes all use it.
func int64Bytes(v int64) []byte {
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], uint64(v))
	return out[:]
}

// sortBytewise orders encoded members the way the grammar says lists of
// independent items are ordered: by their bytes, so the order cannot depend on
// how a database returned them or on how a map iterated.
func sortBytewise(values [][]byte) {
	sort.Slice(values, func(i, j int) bool {
		return bytes.Compare(values[i], values[j]) < 0
	})
}

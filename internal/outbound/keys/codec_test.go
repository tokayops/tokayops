package keys

import (
	"bytes"
	"testing"
)

// The codec tests pin the primitives the whole grammar is built from. They are
// deliberately about bytes rather than about behaviour: everything else in this
// package is one composition of these four rules, and a change here changes the
// identity of every row ever written under it.

func TestEncPrefixesLength(t *testing.T) {
	var buf bytes.Buffer
	encStr(&buf, "abc")

	want := []byte{0, 0, 0, 3, 'a', 'b', 'c'}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("enc(\"abc\") = % x, want % x", buf.Bytes(), want)
	}
}

// TestEncIsUnambiguous is the reason for the length prefix. Without it the two
// pairs below are the same six bytes, and two different identities would share
// one key.
func TestEncIsUnambiguous(t *testing.T) {
	var first, second bytes.Buffer
	encStr(&first, "a")
	encStr(&first, "bc")
	encStr(&second, "ab")
	encStr(&second, "c")

	if bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("{\"a\",\"bc\"} and {\"ab\",\"c\"} encoded to the same bytes")
	}
}

// TestEncOptSeparatesAbsenceFromEmptiness pins the distinction the whole
// grammar leans on: a field nobody set and a field set to nothing are different
// statements, and a step with no message override is not a step whose override
// is the empty string.
func TestEncOptSeparatesAbsenceFromEmptiness(t *testing.T) {
	empty := ""

	var absent, present bytes.Buffer
	encOpt(&absent, nil)
	encOpt(&present, &empty)

	if bytes.Equal(absent.Bytes(), present.Bytes()) {
		t.Fatal("an absent value and an empty one encoded the same")
	}
	if got := absent.Bytes(); !bytes.Equal(got, []byte{0, 0, 0, 1, '0'}) {
		t.Fatalf("absence encoded as % x", got)
	}
}

func TestEncListCountsMembers(t *testing.T) {
	var one, two bytes.Buffer
	encList(&one, [][]byte{[]byte("xy")})
	encList(&two, [][]byte{[]byte("x"), []byte("y")})

	if bytes.Equal(one.Bytes(), two.Bytes()) {
		t.Fatal("a one-member list and a two-member list encoded the same")
	}
}

// TestEncListKeepsCallerOrder proves the codec does not sort behind the
// caller's back. Order is part of what some lists identify, and a sort hidden
// here would make two different states hash alike.
func TestEncListKeepsCallerOrder(t *testing.T) {
	var forward, backward bytes.Buffer
	encList(&forward, [][]byte{[]byte("a"), []byte("b")})
	encList(&backward, [][]byte{[]byte("b"), []byte("a")})

	if bytes.Equal(forward.Bytes(), backward.Bytes()) {
		t.Fatal("encList reordered its input")
	}
}

func TestSortBytewiseIsTotal(t *testing.T) {
	values := [][]byte{[]byte("b"), []byte("a"), []byte("ab"), []byte("")}
	sortBytewise(values)

	want := []string{"", "a", "ab", "b"}
	for i, v := range values {
		if string(v) != want[i] {
			t.Fatalf("sorted[%d] = %q, want %q", i, v, want[i])
		}
	}
}

// TestInt64BytesIsSigned pins the width and the signedness. A negative instant
// - anything before 1970 - has to encode as eight bytes like any other, or the
// length of a field would depend on its value.
func TestInt64BytesIsSigned(t *testing.T) {
	if got := int64Bytes(-1); !bytes.Equal(got, []byte{255, 255, 255, 255, 255, 255, 255, 255}) {
		t.Fatalf("int64Bytes(-1) = % x", got)
	}
	if got := len(int64Bytes(0)); got != 8 {
		t.Fatalf("int64Bytes(0) is %d bytes, want 8", got)
	}
}

func TestTaggedWritesItsNumber(t *testing.T) {
	var buf bytes.Buffer
	tagged(&buf, 7, func(b *bytes.Buffer) { encStr(b, "x") })

	want := []byte{0, 0, 0, 7, 0, 0, 0, 1, 'x'}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("tagged(7, enc(\"x\")) = % x, want % x", buf.Bytes(), want)
	}
}

func TestEncBoolIsTwoLiterals(t *testing.T) {
	var yes, no bytes.Buffer
	encBool(&yes, true)
	encBool(&no, false)

	if bytes.Equal(yes.Bytes(), no.Bytes()) {
		t.Fatal("true and false encoded the same")
	}
	if !bytes.Equal(yes.Bytes(), []byte{0, 0, 0, 1, '1'}) {
		t.Fatalf("true encoded as % x", yes.Bytes())
	}
}

package chain

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

// ItemShape is what the log may say of an input item: its kind, the size of its
// encoding and, for a message, the number of content parts. Never its content.
type ItemShape struct {
	Kind  string
	Size  int
	Parts int
}

// WSDivergence says where a request's history left the chain it would continue.
type WSDivergence struct {
	At  int // the first item that differs
	Had int // items of the request the chain holds
	Has int // items of this request
	// Was is the chain's item at At; Now this request's, zero when it ends before.
	Was, Now ItemShape
	// SameHead and SameTail are the equal leading and trailing bytes of the two
	// encodings when At is the chain's last item; -1 otherwise.
	SameHead, SameTail int
	// Reply is whether the chain has output; ReplyAt where this request holds it
	// (the reply the chain continues), -1 when it does not.
	Reply   bool
	ReplyAt int
	// Since is the time since the chain's response completed.
	Since time.Duration
}

func (c *WSChain) divergence(next *codex.Request, at int) *WSDivergence {
	d := &WSDivergence{At: at, Had: len(c.Hashes), Has: len(next.Input), SameHead: -1, SameTail: -1, Reply: len(c.output) > 0, ReplyAt: -1}
	if at < len(c.shapes) {
		d.Was = c.shapes[at]
	}
	if at < len(next.Input) {
		d.Now = itemShape(next.Input[at])
		if b, err := json.Marshal(next.Input[at]); err == nil {
			d.Now.Size = len(b)
			if at == len(c.Hashes)-1 && c.Last != nil {
				d.SameHead, d.SameTail = commonEnds(c.Last, b)
			}
		}
	}
	if d.Reply {
		d.ReplyAt = c.findOutput(next.Input)
	}
	return d
}

// findOutput is the index where input holds the chain's whole output in order,
// or -1.
func (c *WSChain) findOutput(input []any) int {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(c.output[0], &head) != nil {
		return -1
	}
	want := head.Type
	if want == "message" {
		want = "assistant message"
	}
	for j := range input {
		if itemShape(input[j]).Kind != want {
			continue
		}
		if _, why := c.matchOutput(input, j); why == "" {
			return j
		}
	}
	return -1
}

// commonEnds returns the lengths of the common prefix and suffix of a and b;
// together they never exceed the shorter one.
func commonEnds(a, b []byte) (head, tail int) {
	n := min(len(a), len(b))
	for head < n && a[head] == b[head] {
		head++
	}
	for tail < n-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	return head, tail
}

// itemShape is ItemParts fit for the log: an unknown type is "other".
func itemShape(item any) ItemShape {
	typ, role, _, parts := ItemParts(item)
	switch typ {
	case "":
		return ItemShape{Kind: "other"}
	case "message":
		return ItemShape{Kind: messageKind(role), Parts: parts}
	}
	return ItemShape{Kind: safeItemType(typ)}
}

func messageKind(role string) string {
	switch role {
	case "user", "assistant", "developer", "system":
		return role + " message"
	}
	return "message"
}

var itemTypeName = regexp.MustCompile(`^[a-z_]{1,32}$`)

// safeItemType is a type name fit for the log, "other" for anything else.
func safeItemType(kind string) string {
	if itemTypeName.MatchString(kind) {
		return kind
	}
	return "other"
}

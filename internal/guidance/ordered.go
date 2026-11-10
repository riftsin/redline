package guidance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// node is a JSON value that keeps object key order, so guidance renders in
// the order it was written.
type node struct {
	str    string
	isStr  bool
	list   []node
	isList bool
	keys   []string
	vals   []node
}

func decodeOrdered(data []byte) (node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := decodeValue(dec)
	if err != nil {
		return node{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return node{}, fmt.Errorf("trailing data after JSON value")
	}
	return n, nil
}

func decodeValue(dec *json.Decoder) (node, error) {
	tok, err := dec.Token()
	if err != nil {
		return node{}, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '[':
			n := node{isList: true}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return node{}, err
				}
				n.list = append(n.list, v)
			}
			_, err := dec.Token()
			return n, err
		case '{':
			var n node
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return node{}, err
				}
				v, err := decodeValue(dec)
				if err != nil {
					return node{}, err
				}
				n.keys = append(n.keys, k.(string))
				n.vals = append(n.vals, v)
			}
			_, err := dec.Token()
			return n, err
		}
	case string:
		return node{str: t, isStr: true}, nil
	case json.Number:
		return node{str: t.String(), isStr: true}, nil
	case bool:
		return node{str: fmt.Sprint(t), isStr: true}, nil
	case nil:
		return node{str: "null", isStr: true}, nil
	}
	return node{}, fmt.Errorf("unexpected JSON token %v", tok)
}

func (n node) get(key string) node {
	for i, k := range n.keys {
		if k == key {
			return n.vals[i]
		}
	}
	return node{}
}

func (n node) strings() []string {
	out := make([]string, len(n.list))
	for i, v := range n.list {
		out[i] = v.str
	}
	return out
}

// label turns a JSON key such as "design_rules" into "Design rules".
func label(key string) string {
	s := strings.ReplaceAll(key, "_", " ")
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "redline") {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// writeText renders n as indented plain text: scalars inline, long or
// multi-line strings as blocks, lists of short words on one line, other
// lists as "- " items.
func writeText(b *strings.Builder, n node, indent string) {
	switch {
	case n.isStr:
		writeBlock(b, n.str, indent)
	case n.isList:
		if shortWords(n) {
			b.WriteString(indent + strings.Join(n.strings(), ", ") + "\n")
			return
		}
		for _, item := range n.list {
			if item.isStr {
				writeBullet(b, item.str, indent)
				continue
			}
			var inner strings.Builder
			writeText(&inner, item, indent+"  ")
			text := inner.String()
			b.WriteString(indent + "- " + strings.TrimPrefix(text, indent+"  "))
		}
	default:
		for i, key := range n.keys {
			v := n.vals[i]
			if v.isStr && !strings.Contains(v.str, "\n") {
				b.WriteString(indent + label(key) + ": " + v.str + "\n")
				continue
			}
			b.WriteString(indent + label(key) + ":\n")
			writeText(b, v, indent+"  ")
		}
	}
}

func shortWords(n node) bool {
	if len(n.list) == 0 {
		return true
	}
	for _, item := range n.list {
		if !item.isStr || len(item.str) > 32 || strings.Contains(item.str, "\n") {
			return false
		}
	}
	return true
}

func writeBullet(b *strings.Builder, s, indent string) {
	if !strings.Contains(s, "\n") {
		b.WriteString(indent + "- " + s + "\n")
		return
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	b.WriteString(indent + "- " + lines[0] + "\n")
	for _, l := range lines[1:] {
		b.WriteString(indent + "  " + l + "\n")
	}
}

func writeBlock(b *strings.Builder, s, indent string) {
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if l == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(indent + l + "\n")
	}
}

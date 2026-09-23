// Package pvl parses ISIS Parameter Value Language labels: nested Object and
// Group blocks of Keyword = Value lines.
package pvl

import (
	"fmt"
	"strconv"
	"strings"
)

// Node is an Object or Group in a label (the root has an empty Kind).
type Node struct {
	Kind     string // "Object" or "Group"
	Name     string
	Keywords []Keyword
	Children []*Node
}

// Keyword is one Name = Value line. Arrays have several values; units in
// <angle brackets> are dropped.
type Keyword struct {
	Name   string
	Values []string
}

// Child returns the first direct child Object/Group with the given name.
func (n *Node) Child(name string) *Node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if strings.EqualFold(c.Name, name) {
			return c
		}
	}
	return nil
}

// Find searches depth-first for an Object/Group with the given name.
func (n *Node) Find(name string) *Node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if strings.EqualFold(c.Name, name) {
			return c
		}
		if f := c.Find(name); f != nil {
			return f
		}
	}
	return nil
}

// Key returns the named keyword (case-insensitive), or nil.
func (n *Node) Key(name string) *Keyword {
	if n == nil {
		return nil
	}
	for i := range n.Keywords {
		if strings.EqualFold(n.Keywords[i].Name, name) {
			return &n.Keywords[i]
		}
	}
	return nil
}

// Has reports whether the keyword exists.
func (n *Node) Has(name string) bool { return n.Key(name) != nil }

// Str returns the keyword's first value, or "".
func (n *Node) Str(name string) string {
	k := n.Key(name)
	if k == nil || len(k.Values) == 0 {
		return ""
	}
	return k.Values[0]
}

// Float parses the keyword's first value.
func (n *Node) Float(name string) (float64, error) {
	s := n.Str(name)
	if s == "" {
		return 0, fmt.Errorf("missing keyword %s", name)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("keyword %s: bad number %q", name, s)
	}
	return v, nil
}

// FloatOr is Float with a default for a missing or malformed value.
func (n *Node) FloatOr(name string, def float64) float64 {
	v, err := n.Float(name)
	if err != nil {
		return def
	}
	return v
}

// Int parses the keyword's first value as a whole number.
func (n *Node) Int(name string) (int, error) {
	v, err := n.Float(name)
	return int(v), err
}

// ── tokenizer ──────────────────────────────────────────────────────────────

type tokKind int

const (
	tkEOF tokKind = iota
	tkWord
	tkString
	tkUnit
	tkPunct
)

type token struct {
	kind tokKind
	text string
}

type lexer struct {
	s    string
	pos  int
	peek *token
}

// skipSpace moves past whitespace and comments. NUL bytes (the padding
// between an attached label and the data) end the input.
func (l *lexer) skipSpace() {
	s := l.s
	for l.pos < len(s) {
		c := s[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f':
			l.pos++
		case c == '/' && l.pos+1 < len(s) && s[l.pos+1] == '*':
			end := strings.Index(s[l.pos+2:], "*/")
			if end < 0 {
				l.pos = len(s)
			} else {
				l.pos += end + 4
			}
		case c == '#':
			for l.pos < len(s) && s[l.pos] != '\n' {
				l.pos++
			}
		case c == 0:
			l.pos = len(s)
		default:
			return
		}
	}
}

func (l *lexer) next() token {
	if l.peek != nil {
		t := *l.peek
		l.peek = nil
		return t
	}
	l.skipSpace()
	s := l.s
	if l.pos >= len(s) {
		return token{kind: tkEOF}
	}
	c := s[l.pos]
	switch c {
	case '=', '(', ')', '{', '}', ',':
		l.pos++
		return token{kind: tkPunct, text: string(c)}
	case '"', '\'':
		end := strings.IndexByte(s[l.pos+1:], c)
		var txt string
		if end < 0 {
			txt = s[l.pos+1:]
			l.pos = len(s)
		} else {
			txt = s[l.pos+1 : l.pos+1+end]
			l.pos += end + 2
		}
		// collapse multi-line strings
		txt = strings.Join(strings.Fields(txt), " ")
		return token{kind: tkString, text: txt}
	case '<':
		end := strings.IndexByte(s[l.pos:], '>')
		if end < 0 {
			end = len(s) - l.pos - 1
		}
		txt := s[l.pos+1 : l.pos+end]
		l.pos += end + 1
		return token{kind: tkUnit, text: txt}
	}
	start := l.pos
	for l.pos < len(s) {
		c := s[l.pos]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '=' || c == '(' || c == ')' ||
			c == '{' || c == '}' || c == ',' || c == '<' || c == '"' || c == 0 {
			break
		}
		l.pos++
	}
	return token{kind: tkWord, text: s[start:l.pos]}
}

func (l *lexer) unread(t token) { l.peek = &t }

// Parse reads a label up to its top-level End statement; complete reports
// whether End was reached (false means the text was cut short).
func Parse(text string) (root *Node, complete bool, err error) {
	l := &lexer{s: text}
	root = &Node{}
	stack := []*Node{root}
	for {
		t := l.next()
		if t.kind == tkEOF {
			return root, false, nil
		}
		if t.kind != tkWord && t.kind != tkString {
			continue // stray punctuation; be lenient
		}
		name := t.text
		lname := strings.ToLower(name)
		switch lname {
		case "end":
			return root, true, nil
		case "end_object", "endobject", "end_group", "endgroup":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		eq := l.next()
		if eq.kind != tkPunct || eq.text != "=" {
			// keyword without a value
			l.unread(eq)
			cur := stack[len(stack)-1]
			cur.Keywords = append(cur.Keywords, Keyword{Name: name})
			continue
		}
		vals, err := parseValue(l)
		if err != nil {
			return root, false, fmt.Errorf("label keyword %s: %w", name, err)
		}
		cur := stack[len(stack)-1]
		if lname == "object" || lname == "group" {
			n := &Node{Kind: name, Name: firstOr(vals, "")}
			cur.Children = append(cur.Children, n)
			stack = append(stack, n)
			continue
		}
		cur.Keywords = append(cur.Keywords, Keyword{Name: name, Values: vals})
	}
}

func firstOr(v []string, d string) string {
	if len(v) > 0 {
		return v[0]
	}
	return d
}

func parseValue(l *lexer) ([]string, error) {
	t := l.next()
	switch {
	case t.kind == tkPunct && (t.text == "(" || t.text == "{"):
		var vals []string
		depth := 1
		for depth > 0 {
			e := l.next()
			switch e.kind {
			case tkEOF:
				return vals, fmt.Errorf("unterminated array")
			case tkPunct:
				switch e.text {
				case "(", "{":
					depth++
				case ")", "}":
					depth--
				}
			case tkWord, tkString:
				vals = append(vals, e.text)
			}
		}
		skipUnit(l)
		return vals, nil
	case t.kind == tkWord || t.kind == tkString:
		skipUnit(l)
		return []string{t.text}, nil
	case t.kind == tkEOF:
		return nil, fmt.Errorf("unexpected end of label")
	default:
		l.unread(t)
		return nil, nil
	}
}

func skipUnit(l *lexer) {
	t := l.next()
	if t.kind != tkUnit {
		l.unread(t)
	}
}

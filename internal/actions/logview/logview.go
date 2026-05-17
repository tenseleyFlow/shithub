// SPDX-License-Identifier: AGPL-3.0-or-later

// Package logview parses already-persisted Actions step logs into the small
// view model used by the web UI. It does not ingest, scrub, or rewrite logs;
// downloads and live streaming continue to use the raw stored bytes.
package logview

import (
	"strconv"
	"strings"
)

const (
	KindLine  = "line"
	KindGroup = "group"
)

// Document is a parsed log document. LineCount counts physical log lines,
// including matched group marker lines that are rendered as summaries.
type Document struct {
	Nodes      []Node
	LineCount  int
	GroupCount int
}

func (d Document) HasLines() bool {
	return d.LineCount > 0
}

func (d Document) HasGroups() bool {
	return d.GroupCount > 0
}

// Node is either a plain log line or a collapsible group.
type Node struct {
	Kind  string
	Line  Line
	Group *Group
}

// Line is a visible physical log line.
type Line struct {
	Number int
	Anchor string
	Text   string
}

// Group represents a ::group:: marker and its nested contents.
type Group struct {
	Number   int
	Anchor   string
	Title    string
	Children []Node
	Closed   bool
}

// Parse recognizes GitHub Actions-style ::group:: and ::endgroup:: command
// markers. Matched group markers become summaries, matched end markers are
// hidden, and malformed/unmatched markers remain plain log lines.
func Parse(text string) Document {
	var doc Document
	var stack []*Group
	for i, line := range splitLines(text) {
		number := i + 1
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "::group::"):
			group := &Group{
				Number: number,
				Anchor: anchor(number),
				Title:  strings.TrimPrefix(line, "::group::"),
			}
			doc.GroupCount++
			appendNode(&doc.Nodes, stack, Node{Kind: KindGroup, Group: group})
			stack = append(stack, group)
		case line == "::endgroup::":
			if len(stack) == 0 {
				appendNode(&doc.Nodes, stack, lineNode(number, line))
				continue
			}
			stack[len(stack)-1].Closed = true
			stack = stack[:len(stack)-1]
		default:
			appendNode(&doc.Nodes, stack, lineNode(number, line))
		}
		doc.LineCount = number
	}
	return doc
}

func appendNode(root *[]Node, stack []*Group, node Node) {
	if len(stack) == 0 {
		*root = append(*root, node)
		return
	}
	parent := stack[len(stack)-1]
	parent.Children = append(parent.Children, node)
}

func lineNode(number int, text string) Node {
	return Node{
		Kind: KindLine,
		Line: Line{
			Number: number,
			Anchor: anchor(number),
			Text:   text,
		},
	}
}

func anchor(number int) string {
	return "L" + strconv.Itoa(number)
}

func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	text = strings.TrimSuffix(text, "\n")
	return strings.Split(text, "\n")
}

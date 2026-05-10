// SPDX-License-Identifier: AGPL-3.0-or-later

// Package expr is the strict-allowlist expression evaluator for
// `${{ … }}` blocks in workflow files.
//
// The evaluator is intentionally tiny:
//   - Allowed namespaces: secrets, env, vars, shithub.event, shithub.run_id,
//     shithub.sha, shithub.ref, shithub.actor.
//   - Allowed functions: contains, startsWith, endsWith,
//     success(), failure(), always(), cancelled().
//   - Operators: && || ! == != binary string concat (none — we don't
//     support arithmetic or anything else in v1).
//
// Anything outside that set is an evaluation error. This is the load-
// bearing security surface — the more we accept, the more attack
// surface we open. Future expansion goes through a reviewer-required
// note in the commit message (per the campaign §"Risks": "block any
// S41 PR that adds an evaluator function without a security note").
//
// Every produced Value carries a Tainted bool. References that
// resolve into the shithub.event.* namespace are tagged Tainted=true;
// taint propagates through string concatenation, comparisons (the
// boolean output isn't tainted, but the comparison operands' values
// are checked), and function returns.
package expr

import (
	"fmt"
	"strings"
	"unicode"
)

// TokenKind classifies a lexed token.
type TokenKind int

const (
	TokInvalid TokenKind = iota
	TokIdent             // foo, secrets, shithub
	TokDot               // .
	TokLParen            // (
	TokRParen            // )
	TokComma             // ,
	TokString            // 'literal' (single-quoted only — GHA convention)
	TokBool              // true | false
	TokNull              // null
	TokAnd               // &&
	TokOr                // ||
	TokNot               // !
	TokEq                // ==
	TokNe                // !=
	TokEOF
)

// Token is a single lexed unit. Pos is the byte offset in the original
// source (useful for diagnostic spans).
type Token struct {
	Kind  TokenKind
	Value string
	Pos   int
}

func (k TokenKind) String() string {
	switch k {
	case TokIdent:
		return "identifier"
	case TokDot:
		return "."
	case TokLParen:
		return "("
	case TokRParen:
		return ")"
	case TokComma:
		return ","
	case TokString:
		return "string literal"
	case TokBool:
		return "boolean"
	case TokNull:
		return "null"
	case TokAnd:
		return "&&"
	case TokOr:
		return "||"
	case TokNot:
		return "!"
	case TokEq:
		return "=="
	case TokNe:
		return "!="
	case TokEOF:
		return "end of input"
	}
	return "invalid"
}

// Lex returns the token stream for src or an error on the first lexical
// problem. Whitespace is skipped silently. The lexer doesn't strip the
// surrounding `${{ … }}` — the caller does that before calling Lex.
func Lex(src string) ([]Token, error) {
	var out []Token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '.':
			out = append(out, Token{Kind: TokDot, Value: ".", Pos: i})
			i++
		case c == '(':
			out = append(out, Token{Kind: TokLParen, Value: "(", Pos: i})
			i++
		case c == ')':
			out = append(out, Token{Kind: TokRParen, Value: ")", Pos: i})
			i++
		case c == ',':
			out = append(out, Token{Kind: TokComma, Value: ",", Pos: i})
			i++
		case c == '\'':
			tok, n, err := lexString(src[i:], i)
			if err != nil {
				return nil, err
			}
			out = append(out, tok)
			i += n
		case c == '&':
			if i+1 < len(src) && src[i+1] == '&' {
				out = append(out, Token{Kind: TokAnd, Value: "&&", Pos: i})
				i += 2
			} else {
				return nil, fmt.Errorf("expr: stray '&' at offset %d (expected '&&')", i)
			}
		case c == '|':
			if i+1 < len(src) && src[i+1] == '|' {
				out = append(out, Token{Kind: TokOr, Value: "||", Pos: i})
				i += 2
			} else {
				return nil, fmt.Errorf("expr: stray '|' at offset %d (expected '||')", i)
			}
		case c == '!':
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, Token{Kind: TokNe, Value: "!=", Pos: i})
				i += 2
			} else {
				out = append(out, Token{Kind: TokNot, Value: "!", Pos: i})
				i++
			}
		case c == '=':
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, Token{Kind: TokEq, Value: "==", Pos: i})
				i += 2
			} else {
				return nil, fmt.Errorf("expr: stray '=' at offset %d (expected '==')", i)
			}
		case isIdentStart(c):
			tok, n := lexIdent(src[i:], i)
			out = append(out, tok)
			i += n
		default:
			return nil, fmt.Errorf("expr: unexpected character %q at offset %d", c, i)
		}
	}
	out = append(out, Token{Kind: TokEOF, Pos: i})
	return out, nil
}

func lexString(src string, basePos int) (Token, int, error) {
	if len(src) < 2 {
		return Token{}, 0, fmt.Errorf("expr: unterminated string at offset %d", basePos)
	}
	// Walk until matching '. GHA expressions do NOT support backslash
	// escapes; the only escape is doubling the quote: '' produces '.
	var b strings.Builder
	i := 1 // skip opening '
	for i < len(src) {
		c := src[i]
		if c == '\'' {
			if i+1 < len(src) && src[i+1] == '\'' {
				b.WriteByte('\'')
				i += 2
				continue
			}
			return Token{Kind: TokString, Value: b.String(), Pos: basePos}, i + 1, nil
		}
		b.WriteByte(c)
		i++
	}
	return Token{}, 0, fmt.Errorf("expr: unterminated string at offset %d", basePos)
}

func lexIdent(src string, basePos int) (Token, int) {
	i := 0
	for i < len(src) && isIdentChar(src[i]) {
		i++
	}
	v := src[:i]
	switch v {
	case "true", "false":
		return Token{Kind: TokBool, Value: v, Pos: basePos}, i
	case "null":
		return Token{Kind: TokNull, Value: v, Pos: basePos}, i
	}
	return Token{Kind: TokIdent, Value: v, Pos: basePos}, i
}

func isIdentStart(c byte) bool {
	return unicode.IsLetter(rune(c)) || c == '_'
}

func isIdentChar(c byte) bool {
	return unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '_'
}

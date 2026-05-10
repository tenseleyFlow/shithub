// SPDX-License-Identifier: AGPL-3.0-or-later

package expr

import "fmt"

// Expr is the AST node interface. Each concrete type captures one
// kind of expression we support.
type Expr interface {
	exprNode()
}

// LitString is a quoted string literal in the expression source.
type LitString struct{ V string }

// LitBool is `true` or `false`.
type LitBool struct{ V bool }

// LitNull is the `null` keyword. Used in equality checks against
// possibly-missing context fields.
type LitNull struct{}

// Ref is a dotted reference like `secrets.MY_SECRET` or
// `shithub.event.pull_request.title`. Path is a non-empty slice of
// identifiers (the lexer enforces leading-letter idents).
type Ref struct{ Path []string }

// Call is a function invocation: `contains('foo', 'bar')`. Name is
// validated against the allowlist at eval time, not parse time, so
// we can produce a precise error pointing at the call site.
type Call struct {
	Name string
	Args []Expr
}

// Unary is `!x`.
type Unary struct {
	Op string // "!"
	X  Expr
}

// Binary is `x op y`. Op ∈ {==, !=, &&, ||}. Concat would go here too
// if we supported it (we don't in v1 — we tell users to use shell-side
// concat in `run:`).
type Binary struct {
	Op string
	L  Expr
	R  Expr
}

func (LitString) exprNode() {}
func (LitBool) exprNode()   {}
func (LitNull) exprNode()   {}
func (Ref) exprNode()       {}
func (Call) exprNode()      {}
func (Unary) exprNode()     {}
func (Binary) exprNode()    {}

// Parse turns a token stream into an AST. The grammar is small enough
// that we hand-write a recursive-descent parser with explicit
// precedence: || → && → equality → unary → primary.
func Parse(tokens []Token) (Expr, error) {
	p := &parser{tokens: tokens}
	e, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != TokEOF {
		t := p.peek()
		return nil, fmt.Errorf("expr: unexpected %s after expression at offset %d", t.Kind, t.Pos)
	}
	return e, nil
}

type parser struct {
	tokens []Token
	pos    int
}

func (p *parser) peek() Token { return p.tokens[p.pos] }
func (p *parser) advance() Token {
	t := p.tokens[p.pos]
	p.pos++
	return t
}

func (p *parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == TokOr {
		op := p.advance().Value
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = Binary{Op: op, L: left, R: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Expr, error) {
	left, err := p.parseEquality()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == TokAnd {
		op := p.advance().Value
		right, err := p.parseEquality()
		if err != nil {
			return nil, err
		}
		left = Binary{Op: op, L: left, R: right}
	}
	return left, nil
}

func (p *parser) parseEquality() (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.peek().Kind == TokEq || p.peek().Kind == TokNe {
		op := p.advance().Value
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = Binary{Op: op, L: left, R: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (Expr, error) {
	if p.peek().Kind == TokNot {
		p.advance()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return Unary{Op: "!", X: x}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.Kind {
	case TokString:
		p.advance()
		return LitString{V: t.Value}, nil
	case TokBool:
		p.advance()
		return LitBool{V: t.Value == "true"}, nil
	case TokNull:
		p.advance()
		return LitNull{}, nil
	case TokLParen:
		p.advance()
		e, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().Kind != TokRParen {
			return nil, fmt.Errorf("expr: expected ')' at offset %d", p.peek().Pos)
		}
		p.advance()
		return e, nil
	case TokIdent:
		return p.parseRefOrCall()
	}
	return nil, fmt.Errorf("expr: unexpected %s at offset %d", t.Kind, t.Pos)
}

func (p *parser) parseRefOrCall() (Expr, error) {
	first := p.advance()
	// Function call: ident immediately followed by '('
	if p.peek().Kind == TokLParen {
		p.advance()
		var args []Expr
		if p.peek().Kind != TokRParen {
			for {
				a, err := p.parseOr()
				if err != nil {
					return nil, err
				}
				args = append(args, a)
				if p.peek().Kind == TokComma {
					p.advance()
					continue
				}
				break
			}
		}
		if p.peek().Kind != TokRParen {
			return nil, fmt.Errorf("expr: expected ')' to close call at offset %d", p.peek().Pos)
		}
		p.advance()
		return Call{Name: first.Value, Args: args}, nil
	}
	// Otherwise it's a dotted reference. Walk subsequent `.ident`
	// segments. The base segment IS allowed to stand alone (e.g.
	// `secrets`) but we'll catch that at eval as "namespace requires
	// a member".
	path := []string{first.Value}
	for p.peek().Kind == TokDot {
		p.advance()
		next := p.peek()
		if next.Kind != TokIdent {
			return nil, fmt.Errorf("expr: expected identifier after '.' at offset %d", next.Pos)
		}
		p.advance()
		path = append(path, next.Value)
	}
	return Ref{Path: path}, nil
}

package postgres

import (
	"errors"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// pgFTSExpression preserves the Boolean structure of prepared FTS queries.
// PostgreSQL uses literal ILIKE terms instead of SQLite's token index, so short
// CJK terms remain searchable without changing the archive or its indexes.
func pgFTSExpression(pattern, column string, pb *paramBuilder) (string, error) {
	tokens, err := pgFTSTokens(pattern)
	if err != nil {
		return "", err
	}
	if len(tokens) == 0 {
		return "FALSE", nil
	}
	p := ftsParser{tokens: tokens, column: column, pb: pb}
	expr, err := p.expression(1)
	if err != nil || p.pos != len(tokens) {
		return "", &db.SearchInputError{Msg: "search: invalid FTS query: invalid Boolean expression"}
	}
	return expr, nil
}

func pgFTSTokens(pattern string) ([]ftsToken, error) {
	input := db.PrepareFTSQuery(pattern)
	var tokens []ftsToken
	for i := 0; i < len(input); {
		if strings.ContainsRune(" \t\n\r", rune(input[i])) {
			i++
			continue
		}
		if input[i] == '(' || input[i] == ')' {
			tokens = append(tokens, ftsToken{kind: string(input[i])})
			i++
			continue
		}
		if input[i] != '"' {
			start := i
			for i < len(input) && !strings.ContainsRune(" \t\n\r()\"", rune(input[i])) {
				i++
			}
			word := input[start:i]
			if word != "AND" && word != "OR" && word != "NOT" {
				tokens = append(tokens, ftsToken{kind: "term", text: word})
			} else {
				tokens = append(tokens, ftsToken{kind: word})
			}
			continue
		}
		i++
		var term strings.Builder
		closed := false
		for i < len(input) {
			if input[i] == '"' {
				i++
				if i < len(input) && input[i] == '"' {
					term.WriteByte('"')
					i++
					continue
				}
				closed = true
				break
			}
			term.WriteByte(input[i])
			i++
		}
		if !closed {
			return nil, &db.SearchInputError{Msg: "search: invalid FTS query: invalid quoted term"}
		}
		// Keep prefix markers literal, as in the other substring search paths.
		for i < len(input) && input[i] == '*' {
			term.WriteByte(input[i])
			i++
		}
		if strings.TrimSpace(term.String()) != "" {
			tokens = append(tokens, ftsToken{kind: "term", text: term.String()})
		}
	}
	return tokens, nil
}

type ftsToken struct{ kind, text string }

type ftsParser struct {
	tokens []ftsToken
	column string
	pos    int
	pb     *paramBuilder
}

func (p *ftsParser) atom() (string, error) {
	if p.pos >= len(p.tokens) {
		return "", errors.New("missing term")
	}
	tok := p.tokens[p.pos]
	p.pos++
	switch tok.kind {
	case "term":
		return p.column + " ILIKE '%'||" + p.pb.add(escapeLike(tok.text)) + "||'%' ESCAPE E'\\\\'", nil
	case "(":
		expr, err := p.expression(1)
		if err != nil || p.pos >= len(p.tokens) || p.tokens[p.pos].kind != ")" {
			return "", errors.New("unclosed group")
		}
		p.pos++
		return "(" + expr + ")", nil
	default:
		return "", errors.New("expected term")
	}
}

func (p *ftsParser) expression(minPrec int) (string, error) {
	left, err := p.atom()
	if err != nil {
		return "", err
	}
	for p.pos < len(p.tokens) {
		op := p.tokens[p.pos].kind
		prec, implicit := 0, false
		switch op {
		case "OR":
			prec = 1
		case "AND":
			prec = 2
		case "NOT":
			prec = 3
		case "term":
			prec = 4
			implicit = true
			op = "AND"
		}
		if prec < minPrec {
			break
		}
		if !implicit {
			p.pos++
		}
		right, err := p.expression(prec + 1)
		if err != nil {
			return "", err
		}
		if op == "NOT" {
			left = "(" + left + " AND NOT (" + right + "))"
		} else {
			left = "(" + left + " " + op + " " + right + ")"
		}
	}
	return left, nil
}

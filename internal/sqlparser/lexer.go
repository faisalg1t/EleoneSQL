// Package sqlparser implements a hand-written lexer and recursive-descent
// parser for the subset of SQL EleoneSQL supports: DDL (CREATE/DROP TABLE,
// CREATE INDEX), DML (INSERT/SELECT/UPDATE/DELETE) and transaction control
// (BEGIN/COMMIT/ROLLBACK).
package sqlparser

import (
	"fmt"
	"strings"
	"unicode"
)

type TokenType int

const (
	TokEOF TokenType = iota
	TokIdent
	TokNumber
	TokString
	TokKeyword
	TokPunct // , ( ) ; . = <> <= >= < > + - * /
)

type Token struct {
	Type TokenType
	Text string
	Pos  int
}

var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "INSERT": true, "INTO": true,
	"VALUES": true, "UPDATE": true, "SET": true, "DELETE": true, "CREATE": true,
	"TABLE": true, "DROP": true, "INDEX": true, "ON": true, "UNIQUE": true,
	"PRIMARY": true, "KEY": true, "NOT": true, "NULL": true, "AND": true,
	"OR": true, "ORDER": true, "BY": true, "ASC": true, "DESC": true,
	"LIMIT": true, "JOIN": true, "INNER": true, "LEFT": true, "BEGIN": true,
	"COMMIT": true, "ROLLBACK": true, "TRANSACTION": true, "TRUE": true,
	"FALSE": true, "AS": true, "SHOW": true, "TABLES": true, "COUNT": true,
	"DISTINCT": true,
}

type Lexer struct {
	src  []rune
	pos  int
	toks []Token
}

func Tokenize(src string) ([]Token, error) {
	l := &Lexer{src: []rune(src)}
	for {
		tok, err := l.next()
		if err != nil {
			return nil, err
		}
		l.toks = append(l.toks, tok)
		if tok.Type == TokEOF {
			break
		}
	}
	return l.toks, nil
}

func (l *Lexer) peekRune() rune {
	if l.pos >= len(l.src) {
		return 0
	}
	return l.src[l.pos]
}

func (l *Lexer) next() (Token, error) {
	for l.pos < len(l.src) && unicode.IsSpace(l.src[l.pos]) {
		l.pos++
	}
	if l.pos >= len(l.src) {
		return Token{Type: TokEOF, Pos: l.pos}, nil
	}
	start := l.pos
	c := l.src[l.pos]

	// line comments
	if c == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '-' {
		for l.pos < len(l.src) && l.src[l.pos] != '\n' {
			l.pos++
		}
		return l.next()
	}

	switch {
	case c == '\'':
		l.pos++
		var sb strings.Builder
		for l.pos < len(l.src) {
			if l.src[l.pos] == '\'' {
				if l.pos+1 < len(l.src) && l.src[l.pos+1] == '\'' {
					sb.WriteRune('\'')
					l.pos += 2
					continue
				}
				l.pos++
				return Token{Type: TokString, Text: sb.String(), Pos: start}, nil
			}
			sb.WriteRune(l.src[l.pos])
			l.pos++
		}
		return Token{}, fmt.Errorf("sqlparser: unterminated string literal at %d", start)

	case unicode.IsDigit(c):
		for l.pos < len(l.src) && (unicode.IsDigit(l.src[l.pos]) || l.src[l.pos] == '.') {
			l.pos++
		}
		return Token{Type: TokNumber, Text: string(l.src[start:l.pos]), Pos: start}, nil

	case unicode.IsLetter(c) || c == '_':
		for l.pos < len(l.src) && (unicode.IsLetter(l.src[l.pos]) || unicode.IsDigit(l.src[l.pos]) || l.src[l.pos] == '_') {
			l.pos++
		}
		text := string(l.src[start:l.pos])
		if keywords[strings.ToUpper(text)] {
			return Token{Type: TokKeyword, Text: strings.ToUpper(text), Pos: start}, nil
		}
		return Token{Type: TokIdent, Text: text, Pos: start}, nil

	case c == '"':
		// double-quoted identifier
		l.pos++
		for l.pos < len(l.src) && l.src[l.pos] != '"' {
			l.pos++
		}
		text := string(l.src[start+1 : l.pos])
		l.pos++
		return Token{Type: TokIdent, Text: text, Pos: start}, nil

	case c == '<' || c == '>' || c == '!' || c == '=':
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '=' || (c == '<' && l.src[l.pos] == '>')) {
			l.pos++
		}
		return Token{Type: TokPunct, Text: string(l.src[start:l.pos]), Pos: start}, nil

	case strings.ContainsRune(",()=.;+-*/%", c):
		l.pos++
		return Token{Type: TokPunct, Text: string(c), Pos: start}, nil

	default:
		return Token{}, fmt.Errorf("sqlparser: unexpected character %q at %d", c, start)
	}
}

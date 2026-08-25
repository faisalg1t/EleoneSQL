package sqlparser

import (
	"fmt"
	"strconv"
	"strings"
)

type Parser struct {
	toks []Token
	pos  int
}

// Parse parses a single SQL statement (optionally followed by a trailing
// semicolon).
func Parse(src string) (Statement, error) {
	toks, err := Tokenize(src)
	if err != nil {
		return nil, err
	}
	p := &Parser{toks: toks}
	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	p.skipPunct(";")
	if p.cur().Type != TokEOF {
		return nil, fmt.Errorf("sqlparser: unexpected trailing input near %q", p.cur().Text)
	}
	return stmt, nil
}

func (p *Parser) cur() Token { return p.toks[p.pos] }
func (p *Parser) advance() Token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *Parser) isKeyword(kw string) bool {
	t := p.cur()
	return t.Type == TokKeyword && t.Text == kw
}

func (p *Parser) isPunct(s string) bool {
	t := p.cur()
	return t.Type == TokPunct && t.Text == s
}

func (p *Parser) expectKeyword(kw string) error {
	if !p.isKeyword(kw) {
		return fmt.Errorf("sqlparser: expected %s, got %q", kw, p.cur().Text)
	}
	p.advance()
	return nil
}

func (p *Parser) expectPunct(s string) error {
	if !p.isPunct(s) {
		return fmt.Errorf("sqlparser: expected %q, got %q", s, p.cur().Text)
	}
	p.advance()
	return nil
}

func (p *Parser) skipPunct(s string) {
	if p.isPunct(s) {
		p.advance()
	}
}

func (p *Parser) expectIdent() (string, error) {
	t := p.cur()
	if t.Type != TokIdent {
		return "", fmt.Errorf("sqlparser: expected identifier, got %q", t.Text)
	}
	p.advance()
	return t.Text, nil
}

func (p *Parser) parseStatement() (Statement, error) {
	switch {
	case p.isKeyword("CREATE"):
		return p.parseCreate()
	case p.isKeyword("DROP"):
		return p.parseDrop()
	case p.isKeyword("INSERT"):
		return p.parseInsert()
	case p.isKeyword("SELECT"):
		return p.parseSelect()
	case p.isKeyword("UPDATE"):
		return p.parseUpdate()
	case p.isKeyword("DELETE"):
		return p.parseDelete()
	case p.isKeyword("BEGIN"):
		p.advance()
		p.skipPunct(";")
		if p.isKeyword("TRANSACTION") {
			p.advance()
		}
		return &BeginStmt{}, nil
	case p.isKeyword("COMMIT"):
		p.advance()
		return &CommitStmt{}, nil
	case p.isKeyword("ROLLBACK"):
		p.advance()
		return &RollbackStmt{}, nil
	case p.isKeyword("SHOW"):
		p.advance()
		if err := p.expectKeyword("TABLES"); err != nil {
			return nil, err
		}
		return &ShowTablesStmt{}, nil
	default:
		return nil, fmt.Errorf("sqlparser: unexpected token %q", p.cur().Text)
	}
}

func (p *Parser) parseCreate() (Statement, error) {
	p.advance() // CREATE
	unique := false
	if p.isKeyword("UNIQUE") {
		unique = true
		p.advance()
	}
	switch {
	case p.isKeyword("TABLE"):
		p.advance()
		name, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		var cols []ColumnSpec
		for {
			col, err := p.parseColumnSpec()
			if err != nil {
				return nil, err
			}
			cols = append(cols, col)
			if p.isPunct(",") {
				p.advance()
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return &CreateTableStmt{Table: name, Columns: cols}, nil

	case p.isKeyword("INDEX"):
		p.advance()
		idxName, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("ON"); err != nil {
			return nil, err
		}
		table, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		col, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return &CreateIndexStmt{Name: idxName, Table: table, Column: col, Unique: unique}, nil
	}
	return nil, fmt.Errorf("sqlparser: expected TABLE or INDEX after CREATE, got %q", p.cur().Text)
}

func (p *Parser) parseColumnSpec() (ColumnSpec, error) {
	name, err := p.expectIdent()
	if err != nil {
		return ColumnSpec{}, err
	}
	typTok := p.cur()
	if typTok.Type != TokIdent && typTok.Type != TokKeyword {
		return ColumnSpec{}, fmt.Errorf("sqlparser: expected type for column %s, got %q", name, typTok.Text)
	}
	p.advance()
	spec := ColumnSpec{Name: name, Type: typTok.Text}
	for {
		switch {
		case p.isKeyword("PRIMARY"):
			p.advance()
			if err := p.expectKeyword("KEY"); err != nil {
				return spec, err
			}
			spec.PrimaryKey = true
			spec.NotNull = true
		case p.isKeyword("UNIQUE"):
			p.advance()
			spec.Unique = true
		case p.isKeyword("NOT"):
			p.advance()
			if err := p.expectKeyword("NULL"); err != nil {
				return spec, err
			}
			spec.NotNull = true
		default:
			return spec, nil
		}
	}
}

func (p *Parser) parseDrop() (Statement, error) {
	p.advance() // DROP
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	name, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	return &DropTableStmt{Table: name}, nil
}

func (p *Parser) parseInsert() (Statement, error) {
	p.advance() // INSERT
	if err := p.expectKeyword("INTO"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	var cols []string
	if p.isPunct("(") {
		p.advance()
		for {
			c, err := p.expectIdent()
			if err != nil {
				return nil, err
			}
			cols = append(cols, c)
			if p.isPunct(",") {
				p.advance()
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	if err := p.expectKeyword("VALUES"); err != nil {
		return nil, err
	}
	var rows [][]Expr
	for {
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		var vals []Expr
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			vals = append(vals, e)
			if p.isPunct(",") {
				p.advance()
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		rows = append(rows, vals)
		if p.isPunct(",") {
			p.advance()
			continue
		}
		break
	}
	return &InsertStmt{Table: table, Columns: cols, Rows: rows}, nil
}

func (p *Parser) parseSelect() (Statement, error) {
	p.advance() // SELECT
	stmt := &SelectStmt{}
	if p.isPunct("*") {
		p.advance()
		stmt.Star = true
	} else {
		for {
			item, err := p.parseSelectItem()
			if err != nil {
				return nil, err
			}
			stmt.Items = append(stmt.Items, item)
			if p.isPunct(",") {
				p.advance()
				continue
			}
			break
		}
	}
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	stmt.Table = table
	stmt.Alias = table
	if p.cur().Type == TokIdent {
		stmt.Alias, _ = p.expectIdent()
	} else if p.isKeyword("AS") {
		p.advance()
		stmt.Alias, err = p.expectIdent()
		if err != nil {
			return nil, err
		}
	}

	for p.isKeyword("JOIN") || p.isKeyword("INNER") || p.isKeyword("LEFT") {
		if p.isKeyword("INNER") || p.isKeyword("LEFT") {
			p.advance()
		}
		if err := p.expectKeyword("JOIN"); err != nil {
			return nil, err
		}
		jt, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		alias := jt
		if p.cur().Type == TokIdent {
			alias, _ = p.expectIdent()
		} else if p.isKeyword("AS") {
			p.advance()
			alias, err = p.expectIdent()
			if err != nil {
				return nil, err
			}
		}
		if err := p.expectKeyword("ON"); err != nil {
			return nil, err
		}
		on, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Joins = append(stmt.Joins, JoinClause{Table: jt, Alias: alias, On: on})
	}

	if p.isKeyword("WHERE") {
		p.advance()
		w, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Where = w
	}
	if p.isKeyword("ORDER") {
		p.advance()
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			desc := false
			if p.isKeyword("ASC") {
				p.advance()
			} else if p.isKeyword("DESC") {
				desc = true
				p.advance()
			}
			stmt.OrderBy = append(stmt.OrderBy, OrderTerm{Expr: e, Desc: desc})
			if p.isPunct(",") {
				p.advance()
				continue
			}
			break
		}
	}
	if p.isKeyword("LIMIT") {
		p.advance()
		t := p.cur()
		if t.Type != TokNumber {
			return nil, fmt.Errorf("sqlparser: expected number after LIMIT, got %q", t.Text)
		}
		p.advance()
		n, err := strconv.Atoi(t.Text)
		if err != nil {
			return nil, err
		}
		stmt.Limit = n
		stmt.HasLim = true
	}
	return stmt, nil
}

func (p *Parser) parseSelectItem() (SelectItem, error) {
	if p.isKeyword("COUNT") {
		p.advance()
		if err := p.expectPunct("("); err != nil {
			return SelectItem{}, err
		}
		if p.isPunct("*") {
			p.advance()
		} else {
			// COUNT(col) treated the same as COUNT(*) (NULLs not tracked
			// separately in this starter engine).
			if _, err := p.parseExpr(); err != nil {
				return SelectItem{}, err
			}
		}
		if err := p.expectPunct(")"); err != nil {
			return SelectItem{}, err
		}
		item := SelectItem{Expr: &CountStarExpr{}, Alias: "count"}
		if p.isKeyword("AS") {
			p.advance()
			a, err := p.expectIdent()
			if err != nil {
				return SelectItem{}, err
			}
			item.Alias = a
		}
		return item, nil
	}
	e, err := p.parseExpr()
	if err != nil {
		return SelectItem{}, err
	}
	item := SelectItem{Expr: e}
	if cr, ok := e.(*ColumnRef); ok {
		item.Alias = cr.Name
	}
	if p.isKeyword("AS") {
		p.advance()
		a, err := p.expectIdent()
		if err != nil {
			return SelectItem{}, err
		}
		item.Alias = a
	} else if p.cur().Type == TokIdent {
		a, _ := p.expectIdent()
		item.Alias = a
	}
	return item, nil
}

func (p *Parser) parseUpdate() (Statement, error) {
	p.advance() // UPDATE
	table, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("SET"); err != nil {
		return nil, err
	}
	var assigns []Assignment
	for {
		col, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct("="); err != nil {
			return nil, err
		}
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		assigns = append(assigns, Assignment{Column: col, Value: val})
		if p.isPunct(",") {
			p.advance()
			continue
		}
		break
	}
	stmt := &UpdateStmt{Table: table, Set: assigns}
	if p.isKeyword("WHERE") {
		p.advance()
		w, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Where = w
	}
	return stmt, nil
}

func (p *Parser) parseDelete() (Statement, error) {
	p.advance() // DELETE
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	stmt := &DeleteStmt{Table: table}
	if p.isKeyword("WHERE") {
		p.advance()
		w, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Where = w
	}
	return stmt, nil
}

// --- expression parsing (precedence climbing) ---------------------------

func (p *Parser) parseExpr() (Expr, error) { return p.parseOr() }

func (p *Parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.isKeyword("OR") {
		p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: "OR", Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseAnd() (Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.isKeyword("AND") {
		p.advance()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: "AND", Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseNot() (Expr, error) {
	if p.isKeyword("NOT") {
		p.advance()
		e, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: "NOT", Expr: e}, nil
	}
	return p.parseComparison()
}

var cmpOps = map[string]bool{"=": true, "<>": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true}

func (p *Parser) parseComparison() (Expr, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	if p.cur().Type == TokPunct && cmpOps[p.cur().Text] {
		op := p.advance().Text
		if op == "!=" {
			op = "<>"
		}
		right, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		return &BinaryExpr{Op: op, Left: left, Right: right}, nil
	}
	return left, nil
}

func (p *Parser) parseAdditive() (Expr, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for p.cur().Type == TokPunct && (p.cur().Text == "+" || p.cur().Text == "-") {
		op := p.advance().Text
		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: op, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseMultiplicative() (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.cur().Type == TokPunct && (p.cur().Text == "*" || p.cur().Text == "/") {
		op := p.advance().Text
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: op, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseUnary() (Expr, error) {
	if p.cur().Type == TokPunct && p.cur().Text == "-" {
		p.advance()
		e, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: "-", Expr: e}, nil
	}
	return p.parsePrimary()
}

func (p *Parser) parsePrimary() (Expr, error) {
	t := p.cur()
	switch {
	case t.Type == TokNumber:
		p.advance()
		if strings.Contains(t.Text, ".") {
			f, err := strconv.ParseFloat(t.Text, 64)
			if err != nil {
				return nil, err
			}
			return &LiteralExpr{Kind: "float", F: f}, nil
		}
		i, err := strconv.ParseInt(t.Text, 10, 64)
		if err != nil {
			return nil, err
		}
		return &LiteralExpr{Kind: "int", I: i}, nil

	case t.Type == TokString:
		p.advance()
		return &LiteralExpr{Kind: "string", S: t.Text}, nil

	case t.Type == TokKeyword && t.Text == "NULL":
		p.advance()
		return &LiteralExpr{Kind: "null"}, nil

	case t.Type == TokKeyword && t.Text == "TRUE":
		p.advance()
		return &LiteralExpr{Kind: "bool", B: true}, nil

	case t.Type == TokKeyword && t.Text == "FALSE":
		p.advance()
		return &LiteralExpr{Kind: "bool", B: false}, nil

	case t.Type == TokPunct && t.Text == "(":
		p.advance()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return e, nil

	case t.Type == TokIdent:
		p.advance()
		name := t.Text
		if p.isPunct(".") {
			p.advance()
			col, err := p.expectIdent()
			if err != nil {
				return nil, err
			}
			return &ColumnRef{Table: name, Name: col}, nil
		}
		return &ColumnRef{Name: name}, nil

	default:
		return nil, fmt.Errorf("sqlparser: unexpected token %q in expression", t.Text)
	}
}

package sqlparser

// Statement is implemented by every top-level SQL statement AST node.
type Statement interface{ stmt() }

type ColumnSpec struct {
	Name       string
	Type       string
	PrimaryKey bool
	Unique     bool
	NotNull    bool
}

type CreateTableStmt struct {
	Table   string
	Columns []ColumnSpec
}

type DropTableStmt struct {
	Table string
}

type CreateIndexStmt struct {
	Name   string
	Table  string
	Column string
	Unique bool
}

type InsertStmt struct {
	Table   string
	Columns []string // empty = all columns, in schema order
	Rows    [][]Expr
}

type OrderTerm struct {
	Expr Expr
	Desc bool
}

type JoinClause struct {
	Table string
	Alias string
	On    Expr
}

type SelectItem struct {
	Expr  Expr
	Alias string
}

type SelectStmt struct {
	Items   []SelectItem // empty Items with Star=true means SELECT *
	Star    bool
	Table   string
	Alias   string
	Joins   []JoinClause
	Where   Expr
	OrderBy []OrderTerm
	Limit   int
	HasLim  bool
}

type UpdateStmt struct {
	Table string
	Set   []Assignment
	Where Expr
}

type Assignment struct {
	Column string
	Value  Expr
}

type DeleteStmt struct {
	Table string
	Where Expr
}

type BeginStmt struct{}
type CommitStmt struct{}
type RollbackStmt struct{}
type ShowTablesStmt struct{}

func (*CreateTableStmt) stmt() {}
func (*DropTableStmt) stmt()   {}
func (*CreateIndexStmt) stmt() {}
func (*InsertStmt) stmt()      {}
func (*SelectStmt) stmt()      {}
func (*UpdateStmt) stmt()      {}
func (*DeleteStmt) stmt()      {}
func (*BeginStmt) stmt()       {}
func (*CommitStmt) stmt()      {}
func (*RollbackStmt) stmt()    {}
func (*ShowTablesStmt) stmt()  {}

// --- expressions -------------------------------------------------------

type Expr interface{ expr() }

type LiteralExpr struct {
	Kind string // "int","float","string","bool","null"
	I    int64
	F    float64
	S    string
	B    bool
}

type ColumnRef struct {
	Table string // optional qualifier
	Name  string
}

type BinaryExpr struct {
	Op    string // "=","<>","<","<=",">",">=","AND","OR","+","-","*","/"
	Left  Expr
	Right Expr
}

type UnaryExpr struct {
	Op   string // "NOT", "-"
	Expr Expr
}

type CountStarExpr struct{}

func (*LiteralExpr) expr()   {}
func (*ColumnRef) expr()     {}
func (*BinaryExpr) expr()    {}
func (*UnaryExpr) expr()     {}
func (*CountStarExpr) expr() {}

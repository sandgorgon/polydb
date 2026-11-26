package lang

import "github.com/sandgorgon/polydb/value"

// Node is any syntax tree node.
type Node interface{ Position() Pos }

// At records a node's position; embedded in every node.
type At struct{ P Pos }

func (a At) Position() Pos { return a.P }

// ===== Statements =====

// Stmt is a statement.
type Stmt interface {
	Node
	stmt()
}

func (*CreateDatabase) stmt() {}
func (*DropDatabase) stmt()   {}
func (*Use) stmt()            {}
func (*SetSetting) stmt()     {}
func (*DefineKind) stmt()     {}
func (*DefineRelation) stmt() {}
func (*DefineFormat) stmt()   {}
func (*DefineIndex) stmt()    {}
func (*AlterKind) stmt()      {}
func (*AlterRelation) stmt()  {}
func (*AlterUser) stmt()      {}
func (*AlterDatabase) stmt()  {}
func (*Drop) stmt()           {}
func (*Create) stmt()         {}
func (*Update) stmt()         {}
func (*Delete) stmt()         {}
func (*Relate) stmt()         {}
func (*Unrelate) stmt()       {}
func (*Push) stmt()           {}
func (*Pop) stmt()            {}
func (*Pipeline) stmt()       {}
func (*Begin) stmt()          {}
func (*Commit) stmt()         {}
func (*Rollback) stmt()       {}
func (*CreateUser) stmt()     {}
func (*CreateRole) stmt()     {}
func (*Grant) stmt()          {}
func (*Describe) stmt()       {}
func (*ShowStmt) stmt()       {}
func (*Analyze) stmt()        {}
func (*Backup) stmt()         {}
func (*Export) stmt()         {}
func (*Rebuild) stmt()        {}
func (*Import) stmt()         {}
func (*Restore) stmt()        {}
func (*Explain) stmt()        {}
func (*History) stmt()        {}

type CreateDatabase struct {
	At
	Name string
}

type DropDatabase struct {
	At
	Name string
}

type Use struct {
	At
	Name string
}

// SetSetting is a session setting: set name = value.
type SetSetting struct {
	At
	Name  string
	Value Expr
}

// FieldDef is a field in a kind, relation, format or record definition.
type FieldDef struct {
	At
	Name     string
	Type     *value.Type
	Doc      *string
	Unique   bool
	Default  Expr
	Check    Expr
	OnDelete string // "", "dangle" or "restrict"
	History  *HistoryOpt
	Column   *int // csv and tsv format columns: "at n"
}

// HistoryOpt is "history [keep expr]".
type HistoryOpt struct {
	Keep Expr // nil: keep everything
}

type DefineKind struct {
	At
	Name    string
	Doc     *string
	Closed  bool
	History *HistoryOpt
	Fields  []*FieldDef
}

type DefineRelation struct {
	At
	Name     string
	From, To string // a kind name, "any" or "field"
	Doc      *string
	MaxOut   *int
	MaxIn    *int
	OnDelete string // "", "cascade" or "restrict"
	Props    []*FieldDef
}

type DefineFormat struct {
	At
	Name   string
	Base   string
	Doc    *string
	Header bool // csv and tsv: the first line names the columns
	Fields []*FieldDef
}

type DefineIndex struct {
	At
	Name   string
	Kind   string
	Unique bool
	Text   bool
	Keys   []IndexKey
}

// IndexKey is a field, optionally followed by a constant doc path.
type IndexKey struct {
	Field string
	Path  []DocStep
}

// AlterKind is one "alter kind" action. Exactly one action field is set.
type AlterKind struct {
	At
	Name      string
	AddField  *FieldDef
	DropField string
	Alter     *FieldDef
	RenameTo  string
	SetClosed *bool // true: closed, false: extensible
}

// AlterRelation is one "alter relation" action.
type AlterRelation struct {
	At
	Name      string
	AddProp   *FieldDef
	DropProp  string
	Alter     *FieldDef
	RenameTo  string
	SetOption *RelOption
}

// RelOption is one relation option.
type RelOption struct {
	Doc      *string
	MaxOut   *int
	MaxIn    *int
	OnDelete string
}

// AlterUser changes a user's password or keys.
type AlterUser struct {
	At
	Name     string
	Password Expr
	AddKey   *string
	KeyLabel *string
	DropKey  *string
}

// AlterDatabase is "alter database name set a, b": it changes the
// database's doc, ::labels or ::props (spec/rhizome.md §4).
type AlterDatabase struct {
	At
	Name    string
	Assigns []*Assign
}

// Drop is "drop kind|relation|format|index|user|role name".
type Drop struct {
	At
	What string
	Name string
}

// Init is one "key: value" in a create.
type Init struct {
	At
	Key   string
	Value Expr
	Doc   *string
}

// AddField is one field in an "add" clause.
type AddField struct {
	At
	Name     string
	Type     *value.Type // nil: any
	Value    Expr
	Doc      *string
	Check    Expr
	OnDelete string
	History  *HistoryOpt
}

type Create struct {
	At
	Kind   string // "" for kindless
	Handle string // "" for none
	Inits  []*Init
	Adds   []*AddField
}

// Update applies actions in order to one target.
type Update struct {
	At
	Target  Expr // *HandleRef, *Lit (ID), *ParamRef or *NameRef (pipeline variable)
	Actions []UpdateAction
}

// UpdateAction is one of *SetAction, *UnsetAction, *AddAction, *DropAction.
type UpdateAction interface{ action() }

func (*SetAction) action()   {}
func (*UnsetAction) action() {}
func (*AddAction) action()   {}
func (*DropAction) action()  {}

type SetAction struct{ Assigns []*Assign }
type UnsetAction struct{ Names []string }
type AddAction struct{ Fields []*AddField }
type DropAction struct{ Names []string }

// Assign is "lvalue op expr".
type Assign struct {
	At
	Target *LValue
	Op     string // "=", "+=", "-=", "*=", "/=", "++=", "--="
	Value  Expr
}

// LValue is a field or metadata property, optionally with element steps:
// name, name[i], name.sub, ::labels.
type LValue struct {
	Meta  bool
	Name  string
	Steps []LStep
}

// LStep is ".name" (Field set) or "[expr]".
type LStep struct {
	Field string
	Index Expr
}

type Delete struct {
	At
	Target Expr
}

// NodeRef is an entity, or one of its fields: target[.field].
type NodeRef struct {
	Target Expr
	Field  string
}

type Relate struct {
	At
	From   NodeRef
	Edge   *Edge
	To     NodeRef
	Handle string
}

// Unrelate removes relations matching From–Edge–To, or one relation by
// Target.
type Unrelate struct {
	At
	From   *NodeRef
	Edge   *Edge
	To     *NodeRef
	Target Expr
}

// FieldRef is target.field.
type FieldRef struct {
	Target Expr
	Field  string
}

type Push struct {
	At
	Field    FieldRef
	Value    Expr
	Priority Expr
}

type Pop struct {
	At
	Count Expr
	Field FieldRef
	Wait  Expr
	As    string
}

// Pipeline is a query: find source | steps.
type Pipeline struct {
	At
	Source Source
	AsOf   Expr
	Steps  []Step
}

// Source is either Vars (with an optional In) or a Start expression bound
// to StartAs.
type Source struct {
	Vars    []KindVar
	In      Expr
	Start   Expr // *HandleRef, *Lit (ID) or *ParamRef
	StartAs string
}

// KindVar is "[Kind] var".
type KindVar struct {
	Kind string
	Var  string
}

type Begin struct {
	At
	ReadOnly bool
}

type Commit struct{ At }
type Rollback struct{ At }

type CreateUser struct {
	At
	Name     string
	Password Expr
}

type CreateRole struct {
	At
	Name string
}

// Grant is grant or revoke (Revoke true) of permissions on a scope, or of
// a role (Role set).
type Grant struct {
	At
	Revoke bool
	Perms  []string
	Scope  Scope
	Role   string
	Who    string
}

// Scope is "server", "database name" or "kind [db.]name".
type Scope struct {
	What     string // "server", "database" or "kind"
	Database string
	Kind     string
}

// Describe is "describe target" or "describe kind|relation|format|index name".
type Describe struct {
	At
	Target Expr
	What   string
	Name   string
}

// ShowStmt is a "show" statement.
type ShowStmt struct {
	At
	What string // databases, kinds, relations, formats, indexes, users, roles, sessions, grants, keys, stats
	For  string // grants, keys: user; stats: kind
}

// Backup is "backup database d to 'path'": write a consistent copy of a
// database to a directory under the server's backup directory.
type Backup struct {
	At
	Name string
	Path Expr // text: a constant or parameter
}

// Restore is "restore database d from 'path'": create database d from a
// backup.
type Restore struct {
	At
	Name string
	Path Expr
}

// Export is "export database d", "export security [without passwords]"
// or "export (query)", then "as format" and "to path" when given.
// Without a path the export is the statement's result, a bytes value.
type Export struct {
	At
	What        string // "database", "security" or "query"
	Name        string // the database
	Query       Stmt   // a pipeline
	NoPasswords bool
	Format      string // "" for the default
	Path        Expr   // text; nil: return the export
}

// Import is "import into database d from source [with new ids]" or
// "import security from source [replace]". The source is a path (text)
// or the export itself (bytes, usually an uploaded parameter).
type Import struct {
	At
	What    string // "database" or "security"
	Name    string
	Source  Expr
	NewIDs  bool
	Replace bool
}

// Rebuild is "rebuild index name": build an index again from the data.
type Rebuild struct {
	At
	Index string
}

// Analyze is "analyze kind K": sample the kind's statistics now.
type Analyze struct {
	At
	Kind string
}

type Explain struct {
	At
	Analyze bool
	Stmt    Stmt
}

type History struct {
	At
	Field FieldRef
}

// ===== Pipeline steps =====

// Step is one pipeline step.
type Step interface {
	Node
	step()
}

func (*Where) step()     {}
func (*Follow) step()    {}
func (*Let) step()       {}
func (*GroupBy) step()   {}
func (*Having) step()    {}
func (*Sort) step()      {}
func (*Distinct) step()  {}
func (*Skip) step()      {}
func (*Limit) step()     {}
func (*Show) step()      {}
func (*Bind) step()      {}
func (*UnionStep) step() {}
func (*DataStep) step()  {}

type Where struct {
	At
	Cond Expr
}

// Follow is a follow or match step (Match true).
type Follow struct {
	At
	Match   bool
	PathVar string
	Pattern *Pattern
}

type Let struct {
	At
	Name  string
	Value Expr
}

// Named is "expr as name".
type Named struct {
	Expr Expr
	Name string // "" if not given
}

type GroupBy struct {
	At
	Keys []Named
}

type Having struct {
	At
	Cond Expr
}

type SortKey struct {
	Expr Expr
	Desc bool
}

type Sort struct {
	At
	Keys []SortKey
}

type Distinct struct {
	At
	Exprs []Expr
}

type Skip struct {
	At
	N Expr
}

type Limit struct {
	At
	N Expr
}

type Show struct {
	At
	Items []Named
}

type Bind struct {
	At
	Var    string
	Handle string
}

type UnionStep struct {
	At
	Pipeline *Pipeline
}

// DataStep ends a pipeline with a data change applied to every row.
type DataStep struct {
	At
	Stmt Stmt // *Update, *Delete, *Unrelate, *Push or *Pop
}

// ===== Graph patterns =====

// Pattern is node edge node edge node …
type Pattern struct {
	Nodes []*PNode
	Edges []*Edge
}

// PNode is a pattern node: [Kind] var[.field], @handle[.field] or #id.
type PNode struct {
	At
	Kind   string
	Var    string
	Handle string
	ID     *value.ID
	Field  string
}

// Direction of an edge.
type Direction uint8

const (
	DirOut    Direction = iota // ~type~>
	DirIn                      // <~type~
	DirEither                  // ~type~
)

// Edge is a relation step in a pattern or relate statement.
type Edge struct {
	At
	Dir      Direction
	Types    []string // empty with AnyType
	AnyType  bool     // ~*~>
	Var      string
	Props    *MapLit // property filter or relate properties
	HasDepth bool
	Min, Max int // with HasDepth; Max -1 means up to max_depth
}

// ===== Expressions =====

// Expr is an expression.
type Expr interface {
	Node
	expr()
}

func (*Lit) expr()        {}
func (*NameRef) expr()    {}
func (*HandleRef) expr()  {}
func (*ParamRef) expr()   {}
func (*MetaRef) expr()    {}
func (*Unary) expr()      {}
func (*Binary) expr()     {}
func (*Is) expr()         {}
func (*Between) expr()    {}
func (*FieldExpr) expr()  {}
func (*MetaExpr) expr()   {}
func (*DBMetaExpr) expr() {}
func (*IndexExpr) expr()  {}
func (*DocPath) expr()    {}
func (*Call) expr()       {}
func (*If) expr()         {}
func (*Cast) expr()       {}
func (*Exists) expr()     {}
func (*ListLit) expr()    {}
func (*SetLit) expr()     {}
func (*MapLit) expr()     {}

// Lit is a literal value, including true, false, null, nan, inf and IDs.
type Lit struct {
	At
	Val value.Value
}

// NameRef is a bare name: a variable, or a field of the update target.
type NameRef struct {
	At
	Name string
}

type HandleRef struct {
	At
	Name string
}

type ParamRef struct {
	At
	Name string
}

// MetaRef is "::name", metadata of the update target.
type MetaRef struct {
	At
	Name string
}

// Unary is "-x" or "not x".
type Unary struct {
	At
	Op string
	X  Expr
}

// Binary operators: or and = != < <= > >= in "not in" has matches
// "starts with" "ends with" contains + - ++ -- * / % =>.
type Binary struct {
	At
	Op   string
	X, Y Expr
}

// Is is "x is [not] null|type|Kind".
type Is struct {
	At
	X    Expr
	Not  bool
	Null bool
	Type *value.Type
	Kind string
}

type Between struct {
	At
	X, Lo, Hi Expr
	Not       bool
}

// FieldExpr is x.name.
type FieldExpr struct {
	At
	X    Expr
	Name string
}

// DBMetaExpr is database::name, metadata of the database in use.
type DBMetaExpr struct {
	At
	Name string
}

// MetaExpr is x::name.
type MetaExpr struct {
	At
	X    Expr
	Name string
}

// IndexExpr is x[i].
type IndexExpr struct {
	At
	X     Expr
	Index Expr
}

// DocPath is x->step.
type DocPath struct {
	At
	X    Expr
	Step DocStep
}

// DocStep is one "->" step: a name (Quoted if written as text), an XML
// attribute (Attr), or [expr].
type DocStep struct {
	Name   string
	Quoted bool
	Attr   bool
	Index  Expr
}

type Call struct {
	At
	Name string
	Args []Expr
}

type If struct {
	At
	Cond, Then, Else Expr
}

type Cast struct {
	At
	X    Expr
	Type *value.Type
}

type Exists struct {
	At
	X Expr
}

type ListLit struct {
	At
	Elems []Expr
}

type SetLit struct {
	At
	Elems []Expr
}

// MapLit is {k: v, …}. A bare identifier key is a text key (Bare true).
type MapLit struct {
	At
	Entries []Entry
}

type Entry struct {
	Key   Expr
	Bare  bool
	Value Expr
}

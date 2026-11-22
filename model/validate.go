package model

import (
	"errors"
	"fmt"

	"github.com/sandgorgon/polydb/value"
)

// FieldContext says where a FieldDef appears; the options allowed differ.
type FieldContext uint8

const (
	KindField    FieldContext = iota // declared by a kind
	AddedField                       // added to one entity
	RelationProp                     // a relation type's property
	FormatField                      // part of a derived format's shape
)

var contextNames = [...]string{"kind field", "added field", "relation property", "format field"}

func (c FieldContext) String() string { return contextNames[c] }

// MaxDocLen is the longest doc string allowed (spec/types.md §14).
const MaxDocLen = 64 << 10

// ValidateFieldDef checks one field definition on its own. References to
// kinds and formats in its type are checked by the catalog.
func ValidateFieldDef(fd *FieldDef, ctx FieldContext) error {
	if err := value.ValidateName("field", fd.Name); err != nil {
		return err
	}
	if fd.Type == nil {
		return fmt.Errorf("field %q has no type", fd.Name)
	}
	if err := fd.Type.Validate(); err != nil {
		return fmt.Errorf("field %q: %w", fd.Name, err)
	}
	if len(fd.Doc) > MaxDocLen {
		return fmt.Errorf("field %q: doc longer than %d bytes", fd.Name, MaxDocLen)
	}
	what := func(opt string) error {
		return fmt.Errorf("field %q: %s isn't allowed on a %s", fd.Name, opt, ctx)
	}
	if fd.Unique {
		if ctx != KindField {
			return what("unique")
		}
		if !fd.Type.Keyable() {
			return fmt.Errorf("field %q: unique needs a keyable type, not %s", fd.Name, fd.Type)
		}
	}
	if fd.Default != "" && ctx != KindField {
		return what("default")
	}
	if ctx == FormatField && (fd.Check != "" || fd.History.On || fd.OnDelete != RefDangle) {
		return what("an option other than doc")
	}
	// Refs inside collections and any fields always dangle (spec/types.md
	// §4.12), so restrict needs the field itself to be a ref.
	if fd.OnDelete == RefRestrict && fd.Type.Code != value.CodeRef {
		return fmt.Errorf("field %q: on delete restrict only applies to ref fields, not %s", fd.Name, fd.Type)
	}
	if fd.History.Keep < 0 || (fd.History.Keep > 0 && !fd.History.On) {
		return fmt.Errorf("field %q: invalid history setting", fd.Name)
	}
	return nil
}

func validateFields(owner string, fields []FieldDef, ctx FieldContext) error {
	seen := map[string]bool{}
	for i := range fields {
		fd := &fields[i]
		if err := ValidateFieldDef(fd, ctx); err != nil {
			return fmt.Errorf("%s: %w", owner, err)
		}
		if seen[fd.Name] {
			return fmt.Errorf("%s: duplicate field %q", owner, fd.Name)
		}
		seen[fd.Name] = true
	}
	return nil
}

func validateDoc(owner, doc string) error {
	if len(doc) > MaxDocLen {
		return fmt.Errorf("%s: doc longer than %d bytes", owner, MaxDocLen)
	}
	return nil
}

// Validate checks a kind on its own.
func (k *Kind) Validate() error {
	if err := value.ValidateName("kind", k.Name); err != nil {
		return err
	}
	owner := "kind " + k.Name
	if err := validateDoc(owner, k.Doc); err != nil {
		return err
	}
	if k.History.Keep < 0 || (k.History.Keep > 0 && !k.History.On) {
		return fmt.Errorf("%s: invalid history setting", owner)
	}
	return validateFields(owner, k.Fields, KindField)
}

// Validate checks a relation type on its own.
func (r *RelationType) Validate() error {
	if err := value.ValidateName("relation", r.Name); err != nil {
		return err
	}
	owner := "relation " + r.Name
	if err := validateDoc(owner, r.Doc); err != nil {
		return err
	}
	if r.MaxOut < 0 || r.MaxIn < 0 {
		return fmt.Errorf("%s: maximums can't be negative", owner)
	}
	for _, e := range []Endpoint{r.From, r.To} {
		if e.Mode > EndpointField || (e.Mode == EndpointKind) == e.Kind.IsZero() {
			return fmt.Errorf("%s: invalid endpoint", owner)
		}
	}
	if !r.Defined && (r.Doc != "" || r.MaxOut != 0 || r.MaxIn != 0 || r.OnDelete != RelCascade ||
		len(r.Props) > 0 || r.From.Mode != EndpointAny || r.To.Mode != EndpointAny) {
		return fmt.Errorf("%s: an undefined relation type can't have options", owner)
	}
	return validateFields(owner, r.Props, RelationProp)
}

// Validate checks a derived format on its own.
func (f *Format) Validate() error {
	if err := value.ValidateName("format", f.Name); err != nil {
		return err
	}
	owner := "format " + f.Name
	if err := validateDoc(owner, f.Doc); err != nil {
		return err
	}
	if f.Base == "" {
		return fmt.Errorf("%s: no base format", owner)
	}
	if len(f.Fields) == 0 {
		return fmt.Errorf("%s: needs at least one field", owner)
	}
	return validateFields(owner, f.Fields, FormatField)
}

// Validate checks an index on its own.
func (ix *Index) Validate() error {
	if err := value.ValidateName("index", ix.Name); err != nil {
		return err
	}
	owner := "index " + ix.Name
	switch {
	case ix.Kind.IsZero():
		return fmt.Errorf("%s: no kind", owner)
	case len(ix.Keys) == 0:
		return fmt.Errorf("%s: needs at least one field", owner)
	case ix.Text && (len(ix.Keys) != 1 || ix.Unique):
		return fmt.Errorf("%s: a text index has exactly one field and can't be unique", owner)
	}
	for _, k := range ix.Keys {
		if err := value.ValidateName("field", k.Field); err != nil {
			return fmt.Errorf("%s: %w", owner, err)
		}
		for _, s := range k.Path {
			if s.IsPos && (s.Attr || s.Name != "" || s.Pos < 0) || !s.IsPos && s.Name == "" {
				return fmt.Errorf("%s: invalid path step", owner)
			}
		}
	}
	return nil
}

// ErrClosedKind is returned when adding a field to an entity of a closed
// kind.
var ErrClosedKind = errors.New("the kind is closed: entities can't add their own fields")

// CheckAddField checks that an entity of kind k (nil if kindless), which
// already has fields named in has, may add field fd (spec/rhizome.md §6.2).
func CheckAddField(k *Kind, has func(name string) bool, fd *FieldDef) error {
	if k != nil && k.Closed {
		return ErrClosedKind
	}
	if err := ValidateFieldDef(fd, AddedField); err != nil {
		return err
	}
	if k != nil {
		if _, ok := k.Field(fd.Name); ok {
			return fmt.Errorf("field %q is declared by kind %s; use set", fd.Name, k.Name)
		}
	}
	if has(fd.Name) {
		return fmt.Errorf("field %q already exists; use set", fd.Name)
	}
	return nil
}

// UnknownField returns the error for a write to a field that doesn't
// exist, suggesting the closest existing name.
func UnknownField(name string, existing []string) error {
	if s := Suggest(name, existing); s != "" {
		return fmt.Errorf("no field %q (did you mean %q?)", name, s)
	}
	return fmt.Errorf("no field %q", name)
}

package engine

import (
	"slices"

	"github.com/sandgorgon/polydb/fulltext"
	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/storage"
	"github.com/sandgorgon/polydb/value"
)

// TextSearch returns the entities whose field (one of a full-text
// index's fields) matches a search query, in ID order. It gives the
// same answer as matching the query against each entity's text.
func (tx *Tx) TextSearch(ix *model.Index, field string, q *fulltext.Query) ([]value.ID, error) {
	fieldNo := slices.IndexFunc(ix.Keys, func(k model.IndexKey) bool { return k.Field == field })
	if !ix.Text || fieldNo < 0 {
		return nil, errorf(CodeInternal, "index %s isn't a full-text index on %s", ix.Name, field)
	}
	var result map[value.ID]bool
	var exclude []map[value.ID]bool
	for _, c := range q.Clauses {
		set := map[value.ID]bool{}
		for _, it := range c.Any {
			got, err := tx.textItem(ix, fieldNo, it)
			if err != nil {
				return nil, err
			}
			for id := range got {
				set[id] = true
			}
		}
		switch {
		case c.Not:
			exclude = append(exclude, set)
		case result == nil:
			result = set
		default:
			for id := range result {
				if !set[id] {
					delete(result, id)
				}
			}
		}
	}
	out := make([]value.ID, 0, len(result))
	for id := range result {
		excluded := false
		for _, ex := range exclude {
			if ex[id] {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, func(a, b value.ID) int { return value.Compare(a, b) })
	return out, nil
}

// postings reads the positions of the words with a prefix (an exact word
// when exact) in one field, by entity.
func (tx *Tx) postings(ix *model.Index, fieldNo int, word string, exact bool) (map[value.ID][]int, error) {
	prefix := keys.TextPrefix(ix.ID, word)
	if exact {
		prefix = keys.TextWord(ix.ID, word)
	}
	out := map[value.ID][]int{}
	it := tx.t.Iter(storage.IterOptions{Lower: prefix, Upper: storage.PrefixEnd(prefix)})
	defer it.Close()
	for it.Next() {
		_, id, f, err := keys.ParseTextEntry(it.Key())
		if err != nil {
			return nil, wrap(CodeInternal, err)
		}
		if f != fieldNo {
			continue
		}
		ps, err := keys.DecodePositions(it.Value())
		if err != nil {
			return nil, wrap(CodeInternal, err)
		}
		out[id] = append(out[id], ps...)
	}
	return out, wrap(CodeInternal, it.Err())
}

// textItem returns the entities matching one query item.
func (tx *Tx) textItem(ix *model.Index, fieldNo int, it fulltext.Item) (map[value.ID]bool, error) {
	first, err := tx.postings(ix, fieldNo, it.Words[0], !it.Prefix)
	if err != nil {
		return nil, err
	}
	out := map[value.ID]bool{}
	if len(it.Words) == 1 {
		for id := range first {
			out[id] = true
		}
		return out, nil
	}
	// A phrase: each later word at the next position.
	rest := make([]map[value.ID][]int, len(it.Words)-1)
	for i, w := range it.Words[1:] {
		if rest[i], err = tx.postings(ix, fieldNo, w, true); err != nil {
			return nil, err
		}
	}
	for id, starts := range first {
		for _, p := range starts {
			ok := true
			for i := range rest {
				if !slices.Contains(rest[i][id], p+i+1) {
					ok = false
					break
				}
			}
			if ok {
				out[id] = true
				break
			}
		}
	}
	return out, nil
}

package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// Statistics (spec/rhizome.md §7.5, §12) describe the values of a kind's
// fields from a random sample of its entities: how many have a value or
// null, about how many distinct values there are, the most common ones,
// and a histogram of the rest. The planner uses them to estimate how
// many entities a condition passes. Values are kept as their key
// encodings (value.Key), which sort like the values and like index
// entries, so ranges of an index map straight onto a histogram.
//
// A database samples a kind in the background when it has no
// statistics yet, or once about a tenth of it has changed since the last
// sample (Options.StatsRefresh); DB.Analyze samples one at once.

const (
	statsSample  = 10000 // entities sampled per kind
	statsBuckets = 64    // histogram buckets
	statsCommon  = 16    // most common values kept
	// statsMinChanges is the fewest changes that make a small kind's
	// statistics stale.
	statsMinChanges = 50
	statsVersion    = 1
)

// KindStats are a kind's statistics.
type KindStats struct {
	Version  int
	At       time.Time // when the sample was taken
	Entities int       // the kind's entities then
	Sampled  int       // entities sampled
	Columns  []*ColumnStats
}

// ColumnStats describe one field, or one doc path of an index key, over
// the sample.
type ColumnStats struct {
	Key      model.IndexKey
	Present  int // sampled entities with a value other than null
	Nulls    int // sampled entities holding null
	Keyed    int // present values that can be keys (the histogram's)
	Distinct int // estimated distinct present values in the whole kind
	Common   []Common
	// Bounds are the histogram of the keyed values that aren't common:
	// equal-count buckets between consecutive bounds, inclusive.
	Bounds [][]byte
}

// Common is a frequent value: its key, how it displays, and how many
// sampled entities have it.
type Common struct {
	Key   []byte
	Text  string
	Count int
}

// Column returns the statistics of an index key, or nil.
func (s *KindStats) Column(k model.IndexKey) *ColumnStats {
	for _, c := range s.Columns {
		if sameKey(c.Key, k) {
			return c
		}
	}
	return nil
}

func sameKey(a, b model.IndexKey) bool {
	return a.Field == b.Field && slices.Equal(a.Path, b.Path)
}

// ColumnName names an index key: the field, then its path steps.
func ColumnName(k model.IndexKey) string {
	var b strings.Builder
	b.WriteString(k.Field)
	for _, p := range k.Path {
		b.WriteString("->")
		switch {
		case p.IsPos:
			b.WriteString("[" + value.Int(int64(p.Pos)).String() + "]")
		case p.Attr:
			b.WriteString("@" + p.Name)
		default:
			b.WriteString(p.Name)
		}
	}
	return b.String()
}

// EqFraction estimates the share of the kind's entities whose value has
// the given key.
func (c *ColumnStats) EqFraction(key []byte, sampled int) float64 {
	if sampled == 0 {
		return 0
	}
	other := c.Keyed
	for _, m := range c.Common {
		if bytes.Equal(m.Key, key) {
			return float64(m.Count) / float64(sampled)
		}
		other -= m.Count
	}
	if other <= 0 {
		return 0
	}
	if len(c.Bounds) > 0 && (bytes.Compare(key, c.Bounds[0]) < 0 || bytes.Compare(key, c.Bounds[len(c.Bounds)-1]) > 0) {
		return 0.5 / float64(sampled) // outside everything sampled: rare
	}
	d := max(c.Distinct-len(c.Common), 1)
	return float64(other) / float64(sampled) / float64(d)
}

// RangeFraction estimates the share of the kind's entities whose value's
// key is in [lo, hi); a nil bound is open.
func (c *ColumnStats) RangeFraction(lo, hi []byte, sampled int) float64 {
	if sampled == 0 {
		return 0
	}
	in := func(k []byte) bool {
		return (lo == nil || bytes.Compare(k, lo) >= 0) && (hi == nil || bytes.Compare(k, hi) < 0)
	}
	n, other := 0.0, c.Keyed
	for _, m := range c.Common {
		if in(m.Key) {
			n += float64(m.Count)
		}
		other -= m.Count
	}
	if b := c.Bounds; len(b) >= 2 && other > 0 {
		buckets := len(b) - 1
		share := 0.0
		for i := range buckets {
			from, to := b[i], b[i+1]
			switch {
			case hi != nil && bytes.Compare(from, hi) >= 0, lo != nil && bytes.Compare(to, lo) < 0:
				// outside
			case in(from) && in(to):
				share++
			default:
				share += 0.5
			}
		}
		n += float64(other) * share / float64(buckets)
	} else if len(b) == 1 && in(b[0]) {
		n += float64(other)
	}
	return n / float64(sampled)
}

// --- storage ---

func statsKey(kind value.ID) []byte { return keys.Meta("stats/" + string(kind[:])) }

// KindStats returns a kind's stored statistics, or nil if it has none.
func (tx *Tx) KindStats(k *model.Kind) (*KindStats, error) {
	b, ok, err := tx.get(statsKey(k.ID))
	if err != nil || !ok {
		return nil, err
	}
	var s KindStats
	if err := json.Unmarshal(b, &s); err != nil || s.Version != statsVersion {
		return nil, nil // unreadable or older: sample again
	}
	return &s, nil
}

// Analyze samples a kind's statistics now and stores them.
func (db *DB) Analyze(kind string) (*KindStats, error) {
	var s *KindStats
	err := db.Retry(3, func(tx *Tx) error {
		k, ok := tx.Catalog().Kind(kind)
		if !ok {
			return tx.unknownKind(kind)
		}
		start := db.changes(k.ID).Load()
		var err error
		if s, err = tx.SampleStats(k); err != nil {
			return err
		}
		b, err := json.Marshal(s)
		if err != nil {
			return wrap(CodeInternal, err)
		}
		tx.t.Set(statsKey(k.ID), b)
		db.changes(k.ID).Add(-start)
		return nil
	})
	return s, err
}

// SampleStats samples a kind's statistics without storing them.
func (tx *Tx) SampleStats(k *model.Kind) (*KindStats, error) {
	seed := binary.BigEndian.Uint64(k.ID[8:])
	rng := rand.New(rand.NewPCG(seed, uint64(tx.Now().UnixNano())))
	var ids []value.ID
	n := 0
	err := tx.Entities(k.Name, func(id value.ID) bool {
		n++
		if len(ids) < statsSample {
			ids = append(ids, id)
		} else if j := rng.IntN(n); j < statsSample {
			ids[j] = id // reservoir sampling
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	s := &KindStats{Version: statsVersion, At: tx.Now(), Entities: n, Sampled: len(ids)}
	var cols []model.IndexKey
	for _, fd := range k.Fields {
		cols = append(cols, model.IndexKey{Field: fd.Name})
	}
	for _, ix := range tx.Catalog().IndexesOn(k.ID) {
		for _, key := range ix.Keys {
			if !slices.ContainsFunc(cols, func(c model.IndexKey) bool { return sameKey(c, key) }) {
				cols = append(cols, key)
			}
		}
	}
	type acc struct {
		present, nulls int
		keyed          [][]byte
		all            map[string]int    // key or encoding of every present value
		text           map[string]string // key → how the value displays
	}
	accs := make([]acc, len(cols))
	for i := range accs {
		accs[i].all, accs[i].text = map[string]int{}, map[string]string{}
	}
	for _, id := range ids {
		// Large values stay references: their encodings, which carry the
		// content hash, still count distinct values.
		st, err := tx.loadEntityAs(id, false)
		if err != nil {
			return nil, err
		}
		for i, c := range cols {
			v, ok := tx.columnValue(c, st.fields)
			a := &accs[i]
			switch {
			case !ok:
			case value.IsNull(v):
				a.nulls++
			default:
				a.present++
				if key, err := value.Key(v); err == nil {
					a.keyed = append(a.keyed, key)
					a.all[string(key)]++
					if _, ok := a.text[string(key)]; !ok {
						a.text[string(key)] = shorten(v.String(), 40)
					}
				} else {
					a.all[string(value.Encode(v))]++
				}
			}
		}
	}
	for i, c := range cols {
		a := accs[i]
		cs := &ColumnStats{Key: c, Present: a.present, Nulls: a.nulls, Keyed: len(a.keyed)}
		cs.Distinct = estimateDistinct(a.all, a.present, n*a.present/max(len(ids), 1))
		cs.Common, cs.Bounds = distribution(a.keyed, len(a.all))
		for j := range cs.Common {
			cs.Common[j].Text = a.text[string(cs.Common[j].Key)]
		}
		s.Columns = append(s.Columns, cs)
	}
	return s, nil
}

// columnValue reads an index key's value from an entity's fields; ok is
// false when the field is absent or the path finds nothing.
func (tx *Tx) columnValue(k model.IndexKey, fields map[string]*model.Field) (value.Value, bool) {
	f, ok := fields[k.Field]
	if !ok || f.Value == nil {
		return nil, false
	}
	if len(k.Path) == 0 {
		return f.Value, true
	}
	d, ok := f.Value.(value.Doc)
	if !ok {
		return nil, false
	}
	format, ok := tx.Catalog().Format(d.Format())
	if !ok {
		return nil, false
	}
	return lang.IndexValue(format, d, k.Path)
}

// estimateDistinct estimates the distinct values in a population of
// total from a sample's value counts (the Haas–Stokes "Duj1" estimator,
// which is exact when the sample is everything).
func estimateDistinct(counts map[string]int, sampled, total int) int {
	d := len(counts)
	if sampled == 0 || d == 0 {
		return 0
	}
	f1 := 0
	for _, c := range counts {
		if c == 1 {
			f1++
		}
	}
	n, N := float64(sampled), float64(max(total, sampled))
	est := n * float64(d) / (n - float64(f1) + float64(f1)*n/N)
	return int(min(max(est, float64(d)), N) + 0.5)
}

// distribution picks the most common keys (those well above the average
// frequency) and builds an equal-count histogram of the others.
func distribution(keyed [][]byte, distinct int) ([]Common, [][]byte) {
	if len(keyed) == 0 {
		return nil, nil
	}
	slices.SortFunc(keyed, bytes.Compare)
	var runs []Common
	for i := 0; i < len(keyed); {
		j := i + 1
		for j < len(keyed) && bytes.Equal(keyed[j], keyed[i]) {
			j++
		}
		runs = append(runs, Common{Key: keyed[i], Count: j - i})
		i = j
	}
	avg := float64(len(keyed)) / float64(max(distinct, 1))
	byCount := slices.Clone(runs)
	slices.SortStableFunc(byCount, func(a, b Common) int { return b.Count - a.Count })
	var common []Common
	for _, r := range byCount {
		if len(common) == statsCommon || r.Count < 2 || float64(r.Count) < 1.25*avg && len(runs) > statsCommon {
			break
		}
		common = append(common, r)
	}
	isCommon := map[string]bool{}
	for _, c := range common {
		isCommon[string(c.Key)] = true
	}
	var rest [][]byte
	for _, k := range keyed {
		if !isCommon[string(k)] {
			rest = append(rest, k)
		}
	}
	slices.SortFunc(common, func(a, b Common) int { return bytes.Compare(a.Key, b.Key) })
	if len(rest) == 0 {
		return common, nil
	}
	buckets := min(statsBuckets, len(rest))
	var bounds [][]byte
	for i := 0; i <= buckets; i++ {
		k := rest[min(i*len(rest)/buckets, len(rest)-1)]
		if i == buckets {
			k = rest[len(rest)-1]
		}
		bounds = append(bounds, k)
	}
	return common, bounds
}

// shorten cuts s to at most n characters, marking the cut with "…".
func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// --- freshness ---

// changes is the counter of committed entity changes to a kind since its
// last sample (kept in memory; a restart starts it again).
func (db *DB) changes(kind value.ID) *atomic.Int64 {
	c, _ := db.statChanges.LoadOrStore(kind, new(atomic.Int64))
	return c.(*atomic.Int64)
}

// refreshStats samples the kinds whose statistics are missing or stale.
func (db *DB) refreshStats(ctx context.Context) {
	for _, k := range db.Catalog().Kinds() {
		if ctx.Err() != nil {
			return
		}
		var stale bool
		db.View(func(tx *Tx) error {
			s, err := tx.KindStats(k)
			if err != nil {
				return err
			}
			if s == nil {
				n, err := tx.CountEntitiesUpTo(k.Name, 1)
				stale = err == nil && n > 0
				return err
			}
			stale = db.changes(k.ID).Load() >= int64(max(statsMinChanges, s.Entities/10))
			return nil
		})
		if stale {
			db.Analyze(k.Name) // a failure is retried on the next round
		}
	}
}

// every runs fn every interval in the background until stop is closed;
// a negative interval never runs it. done is closed when it has stopped.
func every(interval time.Duration, stop, done chan struct{}, fn func(ctx context.Context)) {
	if interval < 0 {
		close(done)
		return
	}
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-stop
			cancel()
		}()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				fn(ctx)
			}
		}
	}()
}

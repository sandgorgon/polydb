package query

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sandgorgon/polydb/engine"
	"github.com/sandgorgon/polydb/lang"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/security"
	"github.com/sandgorgon/polydb/storage/lsm"
	"github.com/sandgorgon/polydb/storage/vfs"
	"github.com/sandgorgon/polydb/value"
)

// planData defines people with indexes of every sort and fills them with
// random values, including absent ones.
// dataOpts vary how planData stores its data.
type dataOpts struct {
	disk bool // an LSM store with small tables, so data is flushed and compacted
	late bool // indexes defined after the data, so big kinds build in the background
}

const planIndexes = `
define index person_age on Person(age);
define index city_age on Person(city, age);
define index age_name on Person(age, name);
define index person_friend on Person(friend);
define index cfg_port on Person(cfg->port);
define text index person_bio on Person(bio);
define index pet_owner on Pet(owner);
`

func planData(t *testing.T, n int, seed uint64, opts ...dataOpts) *session {
	var o dataOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	s := newSession(t)
	if o.disk {
		eng, err := engine.Open(engine.Options{Dir: "/data", FS: vfs.NewMem(),
			Storage: lsm.Options{MemtableSize: 4 << 10, TableSize: 8 << 10, LevelBaseSize: 32 << 10, BlockSize: 512}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { eng.Close() })
		if s.db, err = eng.CreateDatabase("test"); err != nil {
			t.Fatal(err)
		}
	}
	s.run(`
define kind Person {
  name   : text
  email  : text unique
  age    : int?
  city   : text?
  score  : float?
  bio    : text?
  cfg    : doc<json>?
  misc   : any
  friend : ref<Person>?
};
define kind Pet { name: text, owner: ref<Person> };
`)
	if !o.late {
		s.run(planIndexes)
	}
	rng := rand.New(rand.NewPCG(seed, 7))
	cities := []string{"Oslo", "Osaka", "Lima", "Lyon", ""}
	words := []string{"alpha", "beta", "gamma", "delta", "alps", "Beta", "ÉPSILON"}
	for i := range n {
		var fs []string
		fs = append(fs, fmt.Sprintf("name: %q, email: \"e%d\"", fmt.Sprintf("n%d", rng.IntN(n/3+1)), i))
		if rng.IntN(5) > 0 {
			fs = append(fs, fmt.Sprintf("age: %d", rng.IntN(60)))
		}
		if c := cities[rng.IntN(len(cities))]; c != "" {
			fs = append(fs, fmt.Sprintf("city: %q", c))
		}
		if rng.IntN(3) > 0 {
			fs = append(fs, fmt.Sprintf("score: %d.5", rng.IntN(10)))
		}
		if rng.IntN(4) > 0 {
			var ws []string
			for range 1 + rng.IntN(4) {
				ws = append(ws, words[rng.IntN(len(words))])
			}
			fs = append(fs, fmt.Sprintf("bio: %q", strings.Join(ws, " ")))
		}
		switch rng.IntN(4) {
		case 0:
			fs = append(fs, fmt.Sprintf(`cfg: "{\"port\": %d}"`, 80+rng.IntN(5)))
		case 1:
			fs = append(fs, `cfg: "{\"port\": \"x\"}"`)
		case 2:
			fs = append(fs, `cfg: "{\"host\": 1}"`)
		}
		switch rng.IntN(5) {
		case 0:
			fs = append(fs, fmt.Sprintf("misc: %d", rng.IntN(5)))
		case 1:
			fs = append(fs, fmt.Sprintf("misc: \"%d\"", rng.IntN(5)))
		case 2:
			fs = append(fs, fmt.Sprintf("misc: [%d]", rng.IntN(3)))
		case 3:
			fs = append(fs, fmt.Sprintf("misc: %d.0", rng.IntN(5)))
		}
		if i > 0 && rng.IntN(2) == 0 {
			fs = append(fs, fmt.Sprintf("friend: @p%d", rng.IntN(i)))
		}
		s.run(fmt.Sprintf("create Person @p%d { %s };", i, strings.Join(fs, ", ")))
		if rng.IntN(3) == 0 {
			s.run(fmt.Sprintf("create Pet { name: \"pet%d\", owner: @p%d };", i, rng.IntN(i+1)))
		}
	}
	// Some changes after indexing.
	for range n / 10 {
		i := rng.IntN(n)
		switch rng.IntN(3) {
		case 0:
			s.run(fmt.Sprintf("update @p%d set age = %d;", i, rng.IntN(60)))
		case 1:
			s.run(fmt.Sprintf("update @p%d unset age, city;", i))
		case 2:
			s.run(fmt.Sprintf("update @p%d set bio = \"gamma ray\";", i))
		}
	}
	if o.late {
		s.run(planIndexes)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.db.WaitIndexes(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// planOf plans a query's source without running it.
func (s *session) planOf(src string) string {
	s.t.Helper()
	st, err := lang.ParseStatement(src)
	if err != nil {
		s.t.Fatalf("%s: %v", src, err)
	}
	tx := s.db.Begin(engine.TxOptions{User: "tester"})
	defer tx.Rollback()
	ctx := &Context{Tx: tx, Handles: s.handles, Params: s.params}
	info, err := lang.Check(ctx.env(), st)
	if err != nil {
		return "" // the query doesn't check
	}
	p, err := newExecutor(ctx, info).planSource(st.(*lang.Pipeline))
	if err != nil {
		s.t.Fatalf("%s: %v", src, err)
	}
	return p.String()
}

func TestPlanChoice(t *testing.T) {
	s := planData(t, 400, 1)
	for _, c := range []struct{ q, want string }{
		{`find Person p | show p.name`, "p: scan"},
		{`find Person p | where p.name = "n3"`, "p: scan"},
		{`find Person p | where p.age = 30`, "p: index person_age"},
		{`find Person p | where 30 = p.age and p.name != "x"`, "p: index person_age"},
		{`find Person p | where p.age in [1, 2]`, "p: index person_age"},
		{`find Person p | where p.email = "e7"`, "p: index email"},
		{`find Person p | where p.email starts with "e39"`, "p: index email"},
		{`find Person p | where p.age > 57`, "p: index person_age"},
		{`find Person p | where p.age between 3 and 4`, "p: index person_age"},
		{`find Person p | where p.age = null`, "p: scan"},
		{`find Person p | where p.city = "Lima" and p.age < 5`, "p: index city_age"},
		// age is optional, so city_age misses people in Lima without one.
		{`find Person p | where p.city = "Lima"`, "p: scan"},
		{`find Person p | where p.cfg->port = 81`, "p: index cfg_port"},
		{`find Person p | where search(p.bio, "delta")`, "p: text person_bio"},
		{`find Person p | where p = @p5`, "p: id"},
		{`find Person p | where p in [@p5, @p6]`, "p: id"},
		{`find Person p | where p.age = 30 or p.age = 31`, "p: scan"},
		{`find Person p | where p.friend = @p0`, "p: index person_friend"},
		{`find Person p, Pet x | where x.owner = p and p.email = "e1"`, "p: index email; x: index pet_owner per row"},
		{`find Pet x, Person p | where x.owner = p and x.name = "pet3"`, "x: scan; p: id per row"},
		{`find Person p | sort p.age desc | limit 3`, "p: scan; top 3 by person_age desc"},
		{`find Person p | where p.age > 2 | sort p.age | skip 2 | limit 3`, "p: index person_age"},
		{`find Person p | where p.age = 30 | sort p.name | limit 2`, "p: index person_age"},
		// age is optional: ascending, people without one come first.
		{`find Person p | sort p.age | limit 3`, "p: scan"},
		{`find Person p | sort p.email | limit 3`, "p: scan; top 3 by email asc"},
		{`find Person p | sort p.age desc | show p.name`, "p: scan"},
	} {
		if got := s.planOf(c.q); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.q, got, c.want)
		}
	}
	// With a large range, reading in index order beats sorting it.
	defer func(n int) { probeCap = n }(probeCap)
	probeCap = 50
	for _, c := range []struct{ q, want string }{
		{`find Person p | where p.email > "e2" | sort p.email | skip 2 | limit 3`, "p: index email; top 5 by email asc"},
		{`find Person p | where p.age > 2 | sort p.age desc | limit 3`, "p: index person_age; top 3 by person_age desc"},
		{`find Person p | where p.email > "e2" | sort p.email | limit 3`, "p: index email; top 3 by email asc"},
		{`find Person p | where p.email = "e2" | sort p.email | limit 3`, "p: index email"},
	} {
		if got := s.planOf(c.q); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.q, got, c.want)
		}
	}
}

// TestPlannerMatchesScan runs random queries with and without the planner:
// whenever the plain plan succeeds, the planned one must give the same
// rows in the same order.
// POLYDB_PLAN_ROUNDS runs more rounds, each with its own data.
func TestPlannerMatchesScan(t *testing.T) {
	rounds, _ := strconv.Atoi(os.Getenv("POLYDB_PLAN_ROUNDS"))
	for r := range max(rounds, 1) {
		t.Run(fmt.Sprint("round", r), func(t *testing.T) {
			plannerMatchesScan(t, planData(t, 150, 2+uint64(r)*10), 150, uint64(r), 1500, true)
		})
	}
}

// The same on disk, with indexes built in the background after the data
// (more than syncBuildLimit people). Joins pair people only with pets, to
// keep the plain plan's cross products small.
func TestPlannerMatchesScanOnDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	n := 1100
	s := planData(t, n, 5, dataOpts{disk: true, late: true})
	plannerMatchesScan(t, s, n, 5, 400, false)
}

func plannerMatchesScan(t *testing.T, s *session, n int, seed uint64, queries int, selfJoins bool) {
	rng := rand.New(rand.NewPCG(3, 4+seed))
	pick := func(xs ...string) string { return xs[rng.IntN(len(xs))] }
	num := func() string { return fmt.Sprint(rng.IntN(62) - 1) }
	cond := func(p, q string) string {
		forms := []func() string{
			func() string { return fmt.Sprintf("%s.age %s %s", p, pick("=", "<", "<=", ">", ">=", "!="), num()) },
			func() string { return fmt.Sprintf("%s %s %s.age", num(), pick("=", "<", ">="), p) },
			func() string { return fmt.Sprintf("%s.age between %s and %s", p, num(), num()) },
			func() string { return fmt.Sprintf("%s.age in [%s, %s%s]", p, num(), num(), pick("", ", null")) },
			func() string { return fmt.Sprintf("%s.age = null", p) },
			func() string { return fmt.Sprintf("%s.age > %s.5", p, num()) },
			func() string { return fmt.Sprintf("%s.age > \"x\"", p) },
			func() string { return fmt.Sprintf("%s.city = %q", p, pick("Oslo", "Lima", "Osaka", "x")) },
			func() string { return fmt.Sprintf("%s.city starts with %q", p, pick("O", "Os", "L", "", "Lyon")) },
			func() string { return fmt.Sprintf("%s.name = \"n%d\"", p, rng.IntN(n/3)) },
			func() string { return fmt.Sprintf("%s.email %s \"e%d\"", p, pick("=", "<", ">"), rng.IntN(n)) },
			func() string {
				return fmt.Sprintf("search(%s.bio, %q)", p, pick("alpha", "beta -gamma", "al*", "\"beta delta\"", "epsilon OR delta"))
			},
			func() string { return fmt.Sprintf("%s.cfg->port %s %d", p, pick("=", ">", "<"), 79+rng.IntN(7)) },
			func() string { return fmt.Sprintf("%s.misc = %s", p, pick("1", "1.0", "\"1\"", "[1]", "null")) },
			func() string { return fmt.Sprintf("%s.misc > %s", p, pick("1", "\"1\"")) },
			func() string { return fmt.Sprintf("%s.score >= %s", p, num()) },
			func() string { return fmt.Sprintf("%s = @p%d", p, rng.IntN(n)) },
			func() string { return fmt.Sprintf("%s in [@p%d, @p%d]", p, rng.IntN(n), rng.IntN(n)) },
			func() string { return fmt.Sprintf("%s.friend = @p%d", p, rng.IntN(n)) },
			func() string { return fmt.Sprintf("%s.age / 0 > 1", p) },
			func() string { return fmt.Sprintf("not %s.age = %s", p, num()) },
			func() string { return fmt.Sprintf("(%s.age = %s or %s.city = \"Oslo\")", p, num(), p) },
		}
		if q != "" {
			forms = append(forms,
				func() string { return fmt.Sprintf("%s.friend = %s", p, q) },
				func() string { return fmt.Sprintf("%s = %s.friend", p, q) },
				func() string { return fmt.Sprintf("%s.age = %s.age", p, q) },
				func() string { return fmt.Sprintf("%s.age < %s.age", q, p) },
				func() string { return fmt.Sprintf("%s.city = %s.city", p, q) },
				func() string { return fmt.Sprintf("%s.owner = %s", q, p) },
			)
		}
		return forms[rng.IntN(len(forms))]()
	}
	where := func(p, q string) string {
		var b strings.Builder
		for range rng.IntN(3) {
			b.WriteString(" | where " + cond(p, q))
			for range rng.IntN(3) {
				b.WriteString(" and " + cond(p, q))
			}
		}
		return b.String()
	}
	matched, failed := 0, 0
	used := map[string]int{}
	defer func(n int) { probeCap = n }(probeCap)
	for i := range queries {
		probeCap = []int{1000, 20, 1}[i%3]
		var q string
		switch i % 4 {
		case 0, 1:
			q = "find Person p" + where("p", "") + " | show p.email, p.age"
		case 2:
			key := pick("p.age", "p.age desc", "p.email", "p.email desc", "p.name", "p.cfg->port desc", "p.misc")
			q = "find Person p" + where("p", "") + " | sort " + key + pick("", ", p.name") +
				pick("", " | skip 3") + fmt.Sprintf(" | limit %d", rng.IntN(8)) + " | show p.email, p.age"
		case 3:
			other := pick("Person q", "Pet q")
			if !selfJoins {
				other = "Pet q"
			}
			q = "find Person p, " + other + where("p", "q")
			q = strings.ReplaceAll(q, "q.age", pick("q.age", "q.name"))
			q += " | where " + cond("p", "q") + " | show p.email, q.name"
			if strings.Contains(q, "Pet q") {
				q = strings.NewReplacer("q.friend", "q.owner", "q.age", "q.name", "q.city", "q.name").Replace(q)
			}
		}
		st, err := lang.ParseStatement(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		run := func(noPlanner bool) (string, error) {
			tx := s.db.Begin(engine.TxOptions{User: "tester"})
			defer tx.Rollback()
			res, err := Exec(&Context{Tx: tx, Handles: s.handles, NoPlanner: noPlanner}, st)
			if err != nil {
				return "", err
			}
			return render(res), nil
		}
		for _, part := range strings.Split(s.planOf(q), "; ") {
			if part == "" {
				continue
			}
			kind := "top"
			if v, rest, ok := strings.Cut(part, ": "); ok && v != "" {
				kind = strings.Fields(rest)[0]
			}
			if strings.HasSuffix(part, "per row") {
				kind += " per row"
			}
			used[kind]++
		}
		want, werr := run(true)
		got, gerr := run(false)
		switch {
		case werr != nil:
			failed++ // the planner may skip rows that raise the error
		case gerr != nil:
			t.Errorf("%s:\n planned: %v\n plain gives %s", q, gerr, want)
		case got != want:
			t.Errorf("%s:\n planned %s\n   plain %s", q, got, want)
		default:
			matched++
		}
	}
	t.Logf("%d queries matched, %d failed without the planner; paths %v", matched, failed, used)
	for _, k := range []string{"index", "text", "id", "top", "index per row", "id per row"} {
		if used[k] == 0 {
			t.Errorf("no query used %s", k)
		}
	}
}

// denyPets is an Access that hides kind Pet.
type denyPets struct{}

func (denyPets) Can(_ security.Perm, k *model.Kind) bool { return k == nil || k.Name != "Pet" }

// An ID lookup sees only what a scan would: entities of kinds the user
// can't read stay hidden from kindless variables, and naming the kind is
// a permission error either way.
func TestPlanPermissions(t *testing.T) {
	s := newSession(t)
	s.run(`define kind Pet { name: text }; create Pet @rex { name: "Rex" }; create @n { name: "Rex" };`)
	for _, noPlanner := range []bool{false, true} {
		run := func(q string) (string, error) {
			st, err := lang.ParseStatement(q)
			if err != nil {
				t.Fatal(err)
			}
			tx := s.db.Begin(engine.TxOptions{User: "tester"})
			defer tx.Rollback()
			res, err := Exec(&Context{Tx: tx, Handles: s.handles, Access: denyPets{}, NoPlanner: noPlanner}, st)
			if err != nil {
				return "", err
			}
			return render(res), nil
		}
		if got, err := run(`find x | where x = @rex or x = @n | show x.name`); err != nil || got != `"Rex"` {
			t.Errorf("noPlanner %v: kindless: %s %v", noPlanner, got, err)
		}
		if got, err := run(`find x | where x in [@rex] | show x.name`); err != nil || got != `` {
			t.Errorf("noPlanner %v: kindless lookup: %q %v", noPlanner, got, err)
		}
		if _, err := run(`find Pet p | where p = @rex | show p.name`); err == nil || !strings.Contains(err.Error(), "permission") {
			t.Errorf("noPlanner %v: kind: %v", noPlanner, err)
		}
	}
}

// FuzzSearch checks search() end to end: through the text index, by
// matching each text (no planner), and on a copy of the field without an
// index, a query finds the same notes or fails the same way.
func FuzzSearch(f *testing.F) {
	for _, q := range []string{"alpha", `"beta gamma" -delta`, "al* OR 東京", "résumé", "-x", `"open`, "a OR", "e-mail", "ÉPSILON beta*"} {
		f.Add(q)
	}
	s := newSession(f)
	s.run(`define kind Note { title: text, body: text?, copy: text? }; define text index note_body on Note(body);`)
	rng := rand.New(rand.NewPCG(11, 12))
	words := []string{"alpha", "Beta", "gamma", "delta", "alps", "résumé", "resume", "東京", "e-mail", "ÉPSILON", "x"}
	for i := range 300 {
		var ws []string
		for range rng.IntN(7) {
			ws = append(ws, words[rng.IntN(len(words))])
		}
		text := strings.Join(ws, pick(rng, " ", ", ", "-", " "))
		if rng.IntN(8) == 0 {
			s.run(fmt.Sprintf("create Note { title: \"t%d\" };", i))
			continue
		}
		s.run(fmt.Sprintf("create Note { title: \"t%d\", body: %q, copy: %q };", i, text, text))
	}
	f.Fuzz(func(t *testing.T, q string) {
		if !utf8.ValidString(q) {
			return // texts are valid UTF-8
		}
		run := func(field string, noPlanner bool) string {
			st, err := lang.ParseStatement("find Note n | where search(n." + field + ", $q) | show n.title")
			if err != nil {
				t.Fatal(err)
			}
			tx := s.db.Begin(engine.TxOptions{User: "tester"})
			defer tx.Rollback()
			res, err := Exec(&Context{Tx: tx, Params: map[string]value.Value{"q": value.MustText(q)}, NoPlanner: noPlanner}, st)
			if err != nil {
				var le *lang.Error
				if errors.As(err, &le) {
					return "error " + le.Code
				}
				return "error " + err.Error()
			}
			return render(res)
		}
		plain := run("body", true)
		if got := run("body", false); got != plain {
			t.Errorf("%q: index gives %s, matching gives %s", q, got, plain)
		}
		if got := run("copy", false); got != plain {
			t.Errorf("%q: unindexed copy gives %s, matching gives %s", q, got, plain)
		}
	})
}

func pick[T any](rng *rand.Rand, xs ...T) T { return xs[rng.IntN(len(xs))] }

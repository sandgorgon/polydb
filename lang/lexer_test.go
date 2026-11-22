package lang

import (
	"strings"
	"testing"
)

// lexString describes the tokens of src compactly for comparison.
func lexString(t *testing.T, src string) string {
	t.Helper()
	toks, err := Lex(src)
	if err != nil {
		t.Fatalf("Lex(%q): %v", src, err)
	}
	var out []string
	for _, tk := range toks[:len(toks)-1] {
		switch tk.Kind {
		case Literal:
			out = append(out, tk.Val.Code().String()+":"+tk.Val.String())
		case Keyword:
			out = append(out, "kw:"+tk.Text)
		default:
			out = append(out, tk.String())
		}
	}
	return strings.Join(out, " ")
}

func TestLexTokens(t *testing.T) {
	cases := map[string]string{
		`find Person p | where p.age > 30;`:     "kw:find Person p | kw:where p . age > int:30 ;",
		`a ++= b --= c ++ d -- e`:               "a ++= b --= c ++ d -- e",
		`p ~knows*1..3~> f <~ x ~ y`:            "p ~ knows * int:1 .. int:3 ~> f <~ x ~ y",
		`p.cfg->server->@port`:                  "p . cfg -> server -> @port",
		`p::created x::y`:                       "p :: created x :: y",
		"`order` `first name` @alice $pw":       "`order` `first name` @alice $pw",
		`42 1_000_000 0x1F 0b1010`:              "int:42 int:1000000 int:31 int:10",
		`123n 3.14 1e-9 2.0 1E3`:                "bigint:123n float:3.14 float:1e-09 float:2.0 float:1000.0",
		`19.99d 0.10d 5d`:                       "decimal:19.99d decimal:0.10d decimal:5d",
		`250ms 1h30m 1.5s 5ns 2µs 3us`:          "duration:250ms duration:1h30m0s duration:1.5s duration:5ns duration:2µs duration:3µs",
		`"a\n\"b\"\u{1F600}" r` + "`C:\\x`":     `text:"a\n\"b\"😀" text:"C:\\x"`,
		`x"dead" b64"3q0="`:                     `bytes:x"dead" bytes:x"dead"`,
		`t"2026-11-21T14:30:00Z" d"2026-11-21"`: `time:t"2026-11-21T14:30:00Z" date:d"2026-11-21"`,
		`#01JD8X3K7M0000Q2W5ZB9N4T6R`:           "id:#01JD8X3K7M0000Q2W5ZB9N4T6R",
		`d "x" x y"z"`:                          `d text:"x" x y text:"z"`,
		"// line\nfoo /* block\n */ bar":        "foo bar",
		`true null nan inf rule`:                "kw:true kw:null kw:nan kw:inf kw:rule",
		`名前 café _x`:                            "名前 café _x",
		`a=>b != c <= d >= e`:                   "a => b != c <= d >= e",
		`list<list<int>>?`:                      "list < list < int > > ?",
	}
	for src, want := range cases {
		if got := lexString(t, src); got != want {
			t.Errorf("Lex(%q)\n got %s\nwant %s", src, got, want)
		}
	}
}

func TestLexNFCAndPositions(t *testing.T) {
	toks, err := Lex("a\n  cafe\u0301 $x")
	if err != nil {
		t.Fatal(err)
	}
	if toks[1].Text != "café" || toks[1].Pos.Line != 2 || toks[1].Pos.Col != 3 {
		t.Errorf("token %+v", toks[1])
	}
	if toks[2].Pos.Col != 8 {
		t.Errorf("$x at col %d, want 8 (columns count code points)", toks[2].Pos.Col)
	}
}

func TestLexErrors(t *testing.T) {
	cases := map[string]string{
		`"unterminated`:        "unterminated text",
		`"bad \q"`:             `unknown escape \q`,
		`"\u{110000}"`:         "invalid code point",
		`/* open`:              "unterminated /*",
		"`open":                "unterminated `quoted",
		"``":                   "empty",
		`@@x`:                  "reserved",
		`@ x`:                  "expected a name",
		`5min`:                 "invalid number suffix",
		`99999999999999999999`: "out of range",
		`1.5n`:                 "fraction",
		`x"abc"`:               "invalid hex",
		`t"yesterday"`:         "invalid time",
		`d"2026-02-30"`:        "invalid date",
		`#nope`:                "invalid id",
		`a ^ b`:                "unexpected character",
		"\xff":                 "not valid UTF-8",
		`999999999999h`:        "out of range",
	}
	for src, want := range cases {
		_, err := Lex(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Lex(%q) error = %v, want containing %q", src, err, want)
		}
	}
}

func FuzzLex(f *testing.F) {
	for _, s := range []string{
		`find Person p | where p.age > 30 and "eng" in p.tags | show p.name;`,
		"create Person @a { name: \"A\", t: t\"2026-01-01T00:00:00Z\", d: 1h30m, x: 0x1F, y: 19.99d };",
		"p ~knows*1..3~> f /* c */ // x\n `odd name` r`raw` b64\"AA==\" #01JD8X3K7M0000Q2W5ZB9N4T6R",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		toks, err := Lex(src)
		if err != nil {
			return
		}
		// Token positions must increase and end at EOF.
		for i := 1; i < len(toks); i++ {
			if toks[i].Pos.Offset < toks[i-1].End {
				t.Fatalf("token %d overlaps the previous one", i)
			}
		}
		if toks[len(toks)-1].Kind != EOF {
			t.Fatal("no EOF token")
		}
	})
}

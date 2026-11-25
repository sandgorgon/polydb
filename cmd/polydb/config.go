package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// A configuration file sets the same things as the flags, one per line:
//
//	# comments and blank lines are ignored
//	listen = :7377
//	data-dir = /var/lib/polydb
//	max-memory = 2GiB
//	compression = true
//
// A key is a flag's name without the dashes; a value may be quoted with
// "…". Flags on the command line override the file.

// configArg finds --config file (or -config, =file) among args.
func configArg(args []string) (string, bool) {
	for i, a := range args {
		if a == "--" {
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "config" {
			continue
		}
		if hasVal {
			return val, true
		}
		if i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// loadConfig sets fs's flags from a configuration file.
func loadConfig(fs *flag.FlagSet, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected key = value", path, n)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			uq, err := strconv.Unquote(val)
			if err != nil {
				return fmt.Errorf("%s:%d: %v", path, n, err)
			}
			val = uq
		}
		if key == "config" || fs.Lookup(key) == nil {
			return fmt.Errorf("%s:%d: unknown setting %q (polydb serve -h lists them)", path, n, key)
		}
		if err := fs.Set(key, val); err != nil {
			return fmt.Errorf("%s:%d: %s: %v", path, n, key, err)
		}
	}
	return sc.Err()
}

// sizeFlag is a byte count given as a number or with a unit: KiB, MiB,
// GiB and TiB (powers of 1024) or KB, MB, GB and TB (of 1000). -1
// means no limit where a flag allows it.
type sizeFlag struct{ p *int64 }

func (f sizeFlag) String() string {
	if f.p == nil {
		return "0"
	}
	return strconv.FormatInt(*f.p, 10)
}

func (f sizeFlag) Set(s string) error {
	n, err := parseSize(s)
	if err != nil {
		return err
	}
	*f.p = n
	return nil
}

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	for _, u := range sizeUnits {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, errors.New("a size is a number of bytes, or one with a unit like 512MiB or 2GiB")
	}
	v := n * float64(mult)
	if v > math.MaxInt64 || v < math.MinInt64 || v != math.Trunc(v) {
		return 0, errors.New("size out of range or not a whole number of bytes")
	}
	return int64(v), nil
}

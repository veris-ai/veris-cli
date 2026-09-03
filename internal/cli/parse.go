package cli

import (
	"flag"
	"strings"
)

// ParseInterspersed parses args on fs with flags and positionals in any order,
// so `veris env create staging --ttl 20` works as well as the other way round.
// The standard parser stops at the first positional; this one takes that word,
// parses the remainder, and repeats. A bare `--` ends the parse and everything
// after it is positional, untouched.
func ParseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	// fs.Parse swallows the terminator, and a `--` in value position
	// (`--name --`) is a value rather than the end, so the terminator is
	// found first by walking the tokens the way the parser will: a flag that
	// takes a value owns the token after it, whatever that token looks like.
	var tail []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--":
			args, tail = args[:i], args[i+1:]
			i = len(args)
		case takesValue(fs, args[i]):
			i++
		}
	}
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return append(positional, tail...), nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// takesValue reports whether arg is a flag on fs that consumes the argument
// after it: a non-boolean flag written without `=value`. A lone "-" is a
// positional, as it is to the flag package.
func takesValue(fs *flag.FlagSet, arg string) bool {
	if len(arg) < 2 || !strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
		return false
	}
	name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return false
	}
	return true
}

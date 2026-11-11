/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selector

import (
	"strings"
	"unicode"
)

// Glob reports whether s matches the pattern as a whole: * is any run of
// characters, ? one character, and \ makes the next character literal. A
// pattern without wildcards is an exact comparison; the comparison is
// case-sensitive.
func Glob(pattern, s string) bool {
	return glob([]rune(pattern), []rune(s))
}

func glob(p, s []rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for len(p) > 0 && p[0] == '*' {
				p = p[1:]
			}
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if glob(p, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			lit := p[0]
			if lit == '\\' && len(p) > 1 {
				p = p[1:]
				lit = p[0]
			}
			if len(s) == 0 || s[0] != lit {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}

// HasWildcard reports whether the pattern has an unescaped * or ?.
func HasWildcard(pattern string) bool {
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\\':
			i++
		case '*', '?':
			return true
		}
	}
	return false
}

// LongestLiteral returns the longest run of literal characters of the pattern
// (escapes resolved), for vendors that can only match a substring.
func LongestLiteral(pattern string) string {
	var best, cur strings.Builder
	rs := []rune(pattern)
	flush := func() {
		if cur.Len() > best.Len() {
			best.Reset()
			best.WriteString(cur.String())
		}
		cur.Reset()
	}
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\\':
			if i+1 < len(rs) {
				i++
			}
			cur.WriteRune(rs[i])
		case '*', '?':
			flush()
		default:
			cur.WriteRune(rs[i])
		}
	}
	flush()
	return best.String()
}

// NaturalCompare orders a and b with runs of digits compared as numbers, so
// that 4081.3.1 sorts before 4081.10.0. It returns -1, 0 or 1.
func NaturalCompare(a, b string) int {
	for a != "" && b != "" {
		da, db := unicode.IsDigit(rune(a[0])), unicode.IsDigit(rune(b[0]))
		if da && db {
			na, ra := digits(a)
			nb, rb := digits(b)
			na, nb = strings.TrimLeft(na, "0"), strings.TrimLeft(nb, "0")
			switch {
			case len(na) != len(nb):
				if len(na) < len(nb) {
					return -1
				}
				return 1
			case na != nb:
				return strings.Compare(na, nb)
			}
			a, b = ra, rb
			continue
		}
		if a[0] != b[0] {
			if a[0] < b[0] {
				return -1
			}
			return 1
		}
		a, b = a[1:], b[1:]
	}
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return -1
	}
	return 1
}

func digits(s string) (run, rest string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i], s[i:]
}

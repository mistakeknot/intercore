// Package coordination provides unified coordination lock management.
//
// Glob overlap algorithm copied from core/intermute/internal/glob/overlap.go
// to avoid L1↔L1 coupling. Do NOT import from intermute.
package coordination

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxTokens    = 50
	maxWildcards = 10
)

type tokenKind int

const (
	tokenLiteral tokenKind = iota
	tokenAny
	tokenStar
	tokenClass
)

type runeRange struct {
	lo rune
	hi rune
}

type token struct {
	kind   tokenKind
	lit    rune
	ranges []runeRange
}

const maxRune = rune(0x10FFFF)

var nonSeparatorRanges = []runeRange{
	{lo: 0, hi: '/' - 1},
	{lo: '/' + 1, hi: maxRune},
}

// ValidateComplexity checks that a glob pattern doesn't exceed token/wildcard limits.
// Must be called before PatternsOverlap to prevent NFA DoS.
func ValidateComplexity(pattern string) error {
	segments := strings.Split(filepath.ToSlash(pattern), "/")
	totalTokens := 0
	totalWildcards := 0
	for _, seg := range segments {
		tokens, err := parseSegment(seg)
		if err != nil {
			return err
		}
		totalTokens += len(tokens)
		for _, t := range tokens {
			if t.kind == tokenStar || t.kind == tokenAny {
				totalWildcards++
			}
		}
	}
	if totalTokens > maxTokens {
		return fmt.Errorf("pattern too complex: %d tokens exceeds limit of %d", totalTokens, maxTokens)
	}
	if totalWildcards > maxWildcards {
		return fmt.Errorf("pattern too complex: %d wildcards exceeds limit of %d", totalWildcards, maxWildcards)
	}
	return nil
}

// PatternsOverlap returns true if two glob patterns can match the same path.
func PatternsOverlap(a, b string) (bool, error) {
	a = filepath.ToSlash(a)
	b = filepath.ToSlash(b)

	segmentsA := strings.Split(a, "/")
	segmentsB := strings.Split(b, "/")
	if len(segmentsA) != len(segmentsB) {
		return false, nil
	}

	for i := range segmentsA {
		overlap, err := segmentPatternsOverlap(segmentsA[i], segmentsB[i])
		if err != nil {
			return false, err
		}
		if !overlap {
			return false, nil
		}
	}

	return true, nil
}

func segmentPatternsOverlap(a, b string) (bool, error) {
	tokensA, err := parseSegment(a)
	if err != nil {
		return false, err
	}
	tokensB, err := parseSegment(b)
	if err != nil {
		return false, err
	}

	type state struct {
		i int
		j int
	}

	addClosure := func(initial state, seen map[state]struct{}, queue *[]state) {
		stack := []state{initial}
		for len(stack) > 0 {
			curr := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if _, ok := seen[curr]; ok {
				continue
			}
			seen[curr] = struct{}{}
			*queue = append(*queue, curr)
			if curr.i < len(tokensA) && tokensA[curr.i].kind == tokenStar {
				stack = append(stack, state{i: curr.i + 1, j: curr.j})
			}
			if curr.j < len(tokensB) && tokensB[curr.j].kind == tokenStar {
				stack = append(stack, state{i: curr.i, j: curr.j + 1})
			}
		}
	}

	seen := make(map[state]struct{})
	queue := make([]state, 0, (len(tokensA)+1)*(len(tokensB)+1))
	addClosure(state{i: 0, j: 0}, seen, &queue)

	for idx := 0; idx < len(queue); idx++ {
		curr := queue[idx]
		if curr.i == len(tokensA) && curr.j == len(tokensB) {
			return true, nil
		}
		if curr.i == len(tokensA) || curr.j == len(tokensB) {
			continue
		}

		aNext, aRanges := tokenConsume(tokensA, curr.i)
		bNext, bRanges := tokenConsume(tokensB, curr.j)
		if !rangesOverlap(aRanges, bRanges) {
			continue
		}

		addClosure(state{i: aNext, j: bNext}, seen, &queue)
	}

	return false, nil
}

func tokenConsume(tokens []token, idx int) (next int, ranges []runeRange) {
	tok := tokens[idx]
	if tok.kind == tokenStar {
		return idx, nonSeparatorRanges
	}
	if tok.kind == tokenLiteral {
		return idx + 1, []runeRange{{lo: tok.lit, hi: tok.lit}}
	}
	return idx + 1, tok.ranges
}

func parseSegment(segment string) ([]token, error) {
	runes := []rune(segment)
	tokens := make([]token, 0, len(runes))

	for i := 0; i < len(runes); {
		ch := runes[i]
		switch ch {
		case '*':
			tokens = append(tokens, token{kind: tokenStar})
			i++
		case '?':
			tokens = append(tokens, token{kind: tokenAny, ranges: nonSeparatorRanges})
			i++
		case '[':
			tok, next, err := parseClass(runes, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, tok)
			i = next
		case '\\':
			if i+1 >= len(runes) {
				return nil, fmt.Errorf("bad pattern")
			}
			tokens = append(tokens, token{kind: tokenLiteral, lit: runes[i+1]})
			i += 2
		default:
			tokens = append(tokens, token{kind: tokenLiteral, lit: ch})
			i++
		}
	}

	return tokens, nil
}

func parseClass(runes []rune, start int) (token, int, error) {
	i := start + 1
	if i >= len(runes) {
		return token{}, 0, fmt.Errorf("bad pattern")
	}
	negated := false
	if runes[i] == '^' {
		negated = true
		i++
	}

	var ranges []runeRange
	hadItem := false
	closed := false

	for i < len(runes) {
		if runes[i] == ']' && hadItem {
			i++
			closed = true
			break
		}

		lo, next, err := readClassRune(runes, i)
		if err != nil {
			return token{}, 0, err
		}
		i = next

		if i+1 < len(runes) && runes[i] == '-' && runes[i+1] != ']' {
			hi, nextHi, err := readClassRune(runes, i+1)
			if err != nil {
				return token{}, 0, err
			}
			if hi < lo {
				return token{}, 0, fmt.Errorf("bad pattern")
			}
			ranges = append(ranges, runeRange{lo: lo, hi: hi})
			i = nextHi
			hadItem = true
			continue
		}

		ranges = append(ranges, runeRange{lo: lo, hi: lo})
		hadItem = true
	}

	if !closed {
		return token{}, 0, fmt.Errorf("bad pattern")
	}

	ranges = normalizeRanges(ranges)
	if negated {
		ranges = subtractRanges(nonSeparatorRanges, ranges)
	} else {
		ranges = intersectRanges(ranges, nonSeparatorRanges)
	}

	return token{kind: tokenClass, ranges: ranges}, i, nil
}

func readClassRune(runes []rune, idx int) (rune, int, error) {
	if idx >= len(runes) {
		return 0, 0, fmt.Errorf("bad pattern")
	}
	if runes[idx] != '\\' {
		return runes[idx], idx + 1, nil
	}
	if idx+1 >= len(runes) {
		return 0, 0, fmt.Errorf("bad pattern")
	}
	return runes[idx+1], idx + 2, nil
}

func rangesOverlap(a, b []runeRange) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].hi < b[j].lo {
			i++
			continue
		}
		if b[j].hi < a[i].lo {
			j++
			continue
		}
		return true
	}
	return false
}

func intersectRanges(a, b []runeRange) []runeRange {
	a = normalizeRanges(a)
	b = normalizeRanges(b)
	out := make([]runeRange, 0)
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		lo := maxIntRune(a[i].lo, b[j].lo)
		hi := minIntRune(a[i].hi, b[j].hi)
		if lo <= hi {
			out = append(out, runeRange{lo: lo, hi: hi})
		}
		if a[i].hi < b[j].hi {
			i++
		} else {
			j++
		}
	}
	return out
}

func subtractRanges(base, subtract []runeRange) []runeRange {
	base = normalizeRanges(base)
	subtract = normalizeRanges(subtract)

	out := make([]runeRange, 0, len(base))
	for _, b := range base {
		current := []runeRange{b}
		for _, s := range subtract {
			next := make([]runeRange, 0, len(current)+1)
			for _, c := range current {
				if s.hi < c.lo || s.lo > c.hi {
					next = append(next, c)
					continue
				}
				if s.lo > c.lo {
					next = append(next, runeRange{lo: c.lo, hi: s.lo - 1})
				}
				if s.hi < c.hi {
					next = append(next, runeRange{lo: s.hi + 1, hi: c.hi})
				}
			}
			current = next
			if len(current) == 0 {
				break
			}
		}
		out = append(out, current...)
	}
	return out
}

func normalizeRanges(ranges []runeRange) []runeRange {
	if len(ranges) <= 1 {
		return ranges
	}

	cp := append([]runeRange(nil), ranges...)
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].lo == cp[j].lo {
			return cp[i].hi < cp[j].hi
		}
		return cp[i].lo < cp[j].lo
	})

	out := make([]runeRange, 0, len(cp))
	current := cp[0]
	for _, rr := range cp[1:] {
		if rr.lo <= current.hi+1 {
			if rr.hi > current.hi {
				current.hi = rr.hi
			}
			continue
		}
		out = append(out, current)
		current = rr
	}
	out = append(out, current)
	return out
}

func maxIntRune(a, b rune) rune {
	if a > b {
		return a
	}
	return b
}

func minIntRune(a, b rune) rune {
	if a < b {
		return a
	}
	return b
}

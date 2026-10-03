// Package normalize splits text into the tokens the index stores and the
// queries match. testdata/normalize_golden.cb records its output over the
// catalogue, so that any change in behaviour shows up as a reviewed diff
// (go test ./internal/normalize -update).
//
// A token is either a word (letters only) or a number (digits only): a change
// between letters and digits ends the token, so "50ml" is "50" "ml".
package normalize

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strings"
	"unicode"
)

// Version is bumped whenever the output of Tokens changes for any input. It is
// part of NormID: a collection indexed with another version has to be
// reprocessed from the source system, since the index stores no text.
//
// 2: "ll" is a letter of its own in Spanish (pollo/polo, calle/cale), so a
// double l is kept; only a run of three or more is cut down to two.
// 3: "sin" is a word: it carries the meaning of "sin gluten", "sin lactosa".
// 4: letters and digits never share a token ("50ml" → "50" "ml"), and the two
// configurable extra letters.
const Version uint16 = 4

// Extra letters appear in tokens as these two bytes, which sort right after
// 'z' — the order of codes 30 and 31 in the 5-bit table.
const (
	Extra1 byte = '{'
	Extra2 byte = '|'
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Normalizer is the normalizer of one collection: the fixed rules plus the
// two extra letters its owner configured.
type Normalizer struct {
	extra [2]rune
}

// Default has no extra letters.
var Default = &Normalizer{}

// New validates the extra letters: at most two (0 is unused), distinct, in
// lowercase, and none of a-z, digits, whitespace or '*', which already mean
// something.
func New(extra [2]rune) (*Normalizer, error) {
	for i, r := range extra {
		if r == 0 {
			continue
		}

		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return nil, fmt.Errorf("normalize: extra letter %q already has a code", r)
		case r == '*' || unicode.IsSpace(r) || unicode.IsControl(r):
			return nil, fmt.Errorf("normalize: extra letter %q is not allowed", r)
		case unicode.ToLower(r) != r:
			return nil, fmt.Errorf("normalize: extra letter %q must be lowercase", r)
		case i == 1 && r == extra[0]:
			return nil, fmt.Errorf("normalize: extra letter %q given twice", r)
		}
	}

	return &Normalizer{extra: extra}, nil
}

// Extra returns the configured extra letters.
func (n *Normalizer) Extra() [2]rune { return n.extra }

// ID is NormID: CRC32C of the version and the extra letters. Every file
// carries it, and a reader rejects a file with another one.
func (n *Normalizer) ID() uint32 {
	var buf [10]byte
	binary.LittleEndian.PutUint16(buf[0:], Version)
	binary.LittleEndian.PutUint32(buf[2:], uint32(n.extra[0]))
	binary.LittleEndian.PutUint32(buf[6:], uint32(n.extra[1]))

	return crc32.Checksum(buf[:], castagnoli)
}

// maxRun is how many times in a row a letter is kept; a longer run is cut.
// Repeats are typos or emphasis ("buenooo"), except where Spanish spells a
// different word with the double letter.
func maxRun(letter byte) int {
	if letter == 'l' {
		return 2
	}

	return 1
}

// Normalize lowercases, folds Spanish accents and ñ (unless ñ is an extra
// letter), keeps letters and digits, treats everything else as a separator,
// splits letters from digits, and cuts a run of the same letter to maxRun
// (digits are never cut).
func (n *Normalizer) Normalize(input string) []string {
	var tokens []string
	var current []byte
	var lastLetter byte
	digits := false
	run := 0

	flush := func() {
		if len(current) > 0 {
			tokens = append(tokens, string(current))
			current = current[:0]
		}

		lastLetter = 0
	}

	for _, source := range input {
		c := n.normalizeChar(source)

		switch {
		case isLetter(c):
			if digits {
				flush()
				digits = false
			}

			if c == lastLetter {
				run++
			} else {
				lastLetter, run = c, 1
			}

			if run <= maxRun(c) {
				current = append(current, c)
			}
		case c >= '0' && c <= '9':
			if !digits {
				flush()
				digits = true
			}

			current = append(current, c)
		default:
			flush()
		}
	}

	flush()

	return tokens
}

// Tokens is Normalize without Spanish stopwords: what gets indexed and scored.
func (n *Normalizer) Tokens(input string) []string {
	tokens := n.Normalize(input)
	kept := tokens[:0]

	for _, token := range tokens {
		if !IsStopword(token) {
			kept = append(kept, token)
		}
	}

	return kept
}

// Normalize is Default.Normalize.
func Normalize(input string) []string { return Default.Normalize(input) }

// Tokens is Default.Tokens.
func Tokens(input string) []string { return Default.Tokens(input) }

// Text is the tokens joined by spaces, for display and tests.
func Text(input string) string {
	return strings.Join(Tokens(input), " ")
}

func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c == Extra1 || c == Extra2
}

// IsNumber reports a token of digits.
func IsNumber(token string) bool {
	return token != "" && token[0] >= '0' && token[0] <= '9'
}

// normalizeChar returns 0 for a separator.
func (n *Normalizer) normalizeChar(source rune) byte {
	switch {
	case source >= 'a' && source <= 'z', source >= '0' && source <= '9':
		return byte(source)
	case source >= 'A' && source <= 'Z':
		return byte(source) + ('a' - 'A')
	}

	if source >= 0x80 || n.extra != [2]rune{} {
		lower := unicode.ToLower(source)
		if lower == n.extra[0] && lower != 0 {
			return Extra1
		}

		if lower == n.extra[1] && lower != 0 {
			return Extra2
		}
	}

	switch source {
	case 'á', 'à', 'ä', 'â', 'Á', 'À', 'Ä', 'Â':
		return 'a'
	case 'é', 'è', 'ë', 'ê', 'É', 'È', 'Ë', 'Ê':
		return 'e'
	case 'í', 'ì', 'ï', 'î', 'Í', 'Ì', 'Ï', 'Î':
		return 'i'
	case 'ó', 'ò', 'ö', 'ô', 'Ó', 'Ò', 'Ö', 'Ô':
		return 'o'
	case 'ú', 'ù', 'ü', 'û', 'Ú', 'Ù', 'Ü', 'Û':
		return 'u'
	case 'ñ', 'Ñ':
		return 'n'
	}

	return 0
}

// IsStopword reports the Spanish function words that are not indexed. "sin"
// is not one: in a catalogue it is the difference between two products.
func IsStopword(token string) bool {
	switch token {
	case "a", "al", "con", "de", "del", "desde", "el", "en", "es", "esa", "ese",
		"eso", "esos", "esta", "esto", "fue", "ha", "la", "las", "le", "les",
		"lo", "los", "me", "mi", "mis", "ni", "no", "nos", "o", "para", "por",
		"que", "se", "ser", "si", "son", "soy", "su", "sus", "tu", "y":
		return true
	}

	return false
}

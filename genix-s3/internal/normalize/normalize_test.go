package normalize

import (
	"flag"
	"os"
	"reflect"
	"testing"

	"github.com/ivanjoz/colbin"
)

var update = flag.Bool("update", false, "rewrite testdata/normalize_golden.cb with the current output")

const goldenPath = "../../testdata/normalize_golden.cb"

type normCase struct {
	Input  string   `cb:"1"`
	Tokens []string `cb:"2"`
	Kept   []string `cb:"3"`
}

type normFixture struct {
	Cases []normCase `cb:"1"`
}

// TestGolden replays the recorded output over the whole catalogue and a set
// of edge cases. A failure is a change in behaviour: if it is intended, bump
// Version, run with -update and review the diff it logs.
func TestGolden(t *testing.T) {
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}

	var fixture normFixture
	if err := colbin.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}

	if len(fixture.Cases) < 1000 {
		t.Fatalf("fixture has only %d cases", len(fixture.Cases))
	}

	changed := 0

	for i, c := range fixture.Cases {
		tokens, kept := Normalize(c.Input), Tokens(c.Input)
		if equalTokens(tokens, c.Tokens) && equalTokens(kept, c.Kept) {
			continue
		}

		changed++

		if *update {
			if changed <= 20 {
				t.Logf("%q: %q → %q", c.Input, c.Tokens, tokens)
			}

			fixture.Cases[i].Tokens, fixture.Cases[i].Kept = tokens, kept

			continue
		}

		t.Errorf("Normalize(%q) = %q, recorded %q", c.Input, tokens, c.Tokens)
	}

	if *update && changed > 0 {
		out, err := colbin.Marshal(&fixture)
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(goldenPath, out, 0o644); err != nil {
			t.Fatal(err)
		}

		t.Logf("%d of %d cases changed; fixture rewritten", changed, len(fixture.Cases))
	}
}

func TestRules(t *testing.T) {
	cases := map[string][]string{
		"CAÑA 100LTS":     {"cana", "100", "lts"},
		"q10 50ml 0,5kg":  {"q", "10", "50", "ml", "0", "5", "kg"},
		"007":             {"007"}, // the value is taken by key5, not here
		"Arroz--Extra":    {"aroz", "extra"},
		"Pollo asado":     {"pollo", "asado"},
		"Polo amarillo":   {"polo", "amarillo"},
		"POLLLO":          {"pollo"}, // three or more l: a typo of ll
		"calllllle":       {"calle"},
		"Talla L":         {"talla", "l"},
		"buenooo cafeeee": {"bueno", "cafe"},
		"1000 ml":         {"1000", "ml"}, // digits are never cut
		"Ñandú Ü":         {"nandu", "u"},
	}

	for input, want := range cases {
		if got := Normalize(input); !reflect.DeepEqual(got, want) {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestTokensDropsStopwords(t *testing.T) {
	got := Tokens("Sal y pimienta de cocina sin gluten")
	want := []string{"sal", "pimienta", "cocina", "sin", "gluten"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tokens = %q, want %q", got, want)
	}
}

func TestExtraLetters(t *testing.T) {
	n, err := New([2]rune{'ñ', '&'})
	if err != nil {
		t.Fatal(err)
	}

	got := n.Normalize("Piña PIÑA campaña M&M 1&2")
	want := []string{"pi{a", "pi{a", "campa{a", "m|m", "1", "|", "2"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Normalize = %q, want %q", got, want)
	}

	if n.ID() == Default.ID() {
		t.Error("extra letters must change NormID")
	}

	for _, bad := range [][2]rune{{'a', 0}, {'5', 0}, {'Ñ', 0}, {'ñ', 'ñ'}, {'*', 0}, {' ', 0}} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%q) accepted", bad)
		}
	}
}

// colbin decodes an empty slice as nil.
func equalTokens(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}

	return reflect.DeepEqual(a, b)
}

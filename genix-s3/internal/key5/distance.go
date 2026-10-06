package key5

// Distance is the optimal-string-alignment distance (Damerau-Levenshtein
// with adjacent transpositions, each substring edited once) between two unit
// strings. It gives up as soon as the distance must exceed budget.
func Distance(a, b []uint8, budget int) (int, bool) {
	if diff := len(a) - len(b); diff > budget || -diff > budget {
		return 0, false
	}

	// Three rows: the previous two for the transposition, and the current one.
	var store [3 * (2*MaxUnits + 8)]int
	width := len(b) + 1

	var rows []int
	if 3*width <= len(store) {
		rows = store[:3*width]
	} else {
		rows = make([]int, 3*width)
	}

	prev2, prev, cur := rows[:width], rows[width:2*width], rows[2*width:]

	for j := range width {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		cur[0] = i
		rowMin := cur[0]

		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}

			best := min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				best = min(best, prev2[j-2]+1)
			}

			cur[j] = best
			rowMin = min(rowMin, best)
		}

		if rowMin > budget {
			return 0, false
		}

		prev2, prev, cur = prev, cur, prev2
	}

	if d := prev[len(b)]; d <= budget {
		return d, true
	}

	return 0, false
}

// KeyDistance compares a dictionary key with the whole query word (all its
// letters, not cut at 8). A key of 8 letters may be a longer word cut short,
// so it is compared with the query's prefixes of 8−budget to 8+budget
// letters and the best is kept: "chocoate" is one edit
// from the key "chocolat".
func KeyDistance(key, query []uint8, budget int) (int, bool) {
	if len(key) < MaxUnits {
		return Distance(key, query, budget)
	}

	best, found := budget+1, false

	for n := MaxUnits - budget; n <= min(len(query), MaxUnits+budget); n++ {
		if n < 0 {
			continue
		}

		if d, ok := Distance(key, query[:n], min(budget, best)); ok && d < best {
			best, found = d, true
		}
	}

	return best, found
}

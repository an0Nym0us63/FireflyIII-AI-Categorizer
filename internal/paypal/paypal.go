// Package paypal parses PayPal activity CSV exports so the categorizer can
// recover the *real* merchant (and item details) behind an opaque "PAYPAL *…"
// bank transaction, matching by amount and date.
package paypal

import (
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type record struct {
	date     time.Time
	name     string   // "Nom" — the counterparty (merchant / payer)
	amount   float64  // abs(Net)
	inflow   bool     // Net > 0 (money in)
	items    []string // item title / subject / note
	currency string
}

// Index holds all parsed PayPal records.
type Index struct {
	records []record
}

// Loaded reports whether any records were parsed.
func (i *Index) Loaded() bool { return i != nil && len(i.records) > 0 }

// Load parses a PayPal CSV file, or every *.csv/*.CSV in a directory.
func Load(path string) *Index {
	idx := &Index{}
	if path == "" {
		return idx
	}
	info, err := os.Stat(path)
	if err != nil {
		return idx
	}
	var files []string
	if info.IsDir() {
		entries, _ := os.ReadDir(path)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if ext := strings.ToLower(filepath.Ext(e.Name())); ext == ".csv" {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
	} else {
		files = []string{path}
	}
	for _, f := range files {
		idx.loadFile(f)
	}
	return idx
}

func (i *Index) loadFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := strings.TrimPrefix(string(data), "\ufeff") // drop UTF-8 BOM
	r := csv.NewReader(strings.NewReader(content))
	r.FieldsPerRecord = -1 // tolerate ragged rows
	r.LazyQuotes = true    // tolerate stray quotes inside fields
	rows, err := r.ReadAll()
	if err != nil || len(rows) < 2 {
		return
	}
	// Build header -> column index map (strip UTF-8 BOM on the first cell).
	head := rows[0]
	if len(head) > 0 {
		head[0] = strings.TrimPrefix(head[0], "\ufeff")
	}
	col := map[string]int{}
	for idx, h := range head {
		col[strings.TrimSpace(strings.ToLower(h))] = idx
	}
	get := func(row []string, name string) string {
		if c, ok := col[name]; ok && c < len(row) {
			return strings.TrimSpace(row[c])
		}
		return ""
	}
	for _, row := range rows[1:] {
		name := get(row, "nom")
		net := parseAmount(get(row, "net"))
		if name == "" || net == 0 {
			continue
		}
		d := parseDate(get(row, "date"))
		var items []string
		for _, k := range []string{"titre de l'objet", "objet", "remarque"} {
			if v := get(row, k); v != "" {
				items = append(items, v)
			}
		}
		i.records = append(i.records, record{
			date:     d,
			name:     name,
			amount:   round2(math.Abs(net)),
			inflow:   net > 0,
			items:    items,
			currency: get(row, "devise"),
		})
	}
}

// Lookup finds the PayPal record matching the given (absolute) amount around the
// given date. Returns the merchant name, item details, and whether the match is
// unambiguous. `certain` is false when several distinct merchants match.
func (i *Index) Lookup(amount float64, date time.Time) (merchant string, content []string, certain bool, ok bool) {
	if !i.Loaded() {
		return "", nil, false, false
	}
	target := round2(math.Abs(amount))
	const windowDays = 7
	type cand struct {
		rec  record
		dist int
	}
	var cands []cand
	for _, rec := range i.records {
		if rec.amount != target {
			continue
		}
		dist := 0
		if !date.IsZero() && !rec.date.IsZero() {
			dist = dayDist(rec.date, date)
			if dist > windowDays {
				continue
			}
		}
		cands = append(cands, cand{rec, dist})
	}
	if len(cands) == 0 {
		return "", nil, false, false
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].dist < cands[b].dist })
	// Distinct merchants among candidates?
	names := map[string]bool{}
	for _, c := range cands {
		names[strings.ToLower(c.rec.name)] = true
	}
	best := cands[0].rec
	return best.name, best.items, len(names) == 1, true
}

func parseAmount(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// French format: "1 234,56" -> remove spaces/nbsp, comma -> dot.
	s = strings.NewReplacer(" ", "", "\u00a0", "", ".", "").Replace(s)
	s = strings.Replace(s, ",", ".", 1)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"02/01/2006", "2006-01-02", "01/02/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func dayDist(a, b time.Time) int {
	d := int(a.Sub(b).Hours() / 24)
	if d < 0 {
		d = -d
	}
	return d
}

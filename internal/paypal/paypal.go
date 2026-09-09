// Package paypal parses PayPal activity CSV exports so the categorizer can
// recover the *real* merchant (and item details) behind an opaque "PAYPAL *…"
// bank transaction, matching by amount and date.
//
// It also handles the "balance top-up" case: a bank debit that funds the PayPal
// balance ("Virement bancaire sur le compte PayPal", positive, no merchant)
// which paid an order at the same timestamp — in that case the real merchant is
// taken from the linked payment.
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
	when   time.Time // date + time
	date   time.Time // date only
	name   string    // "Nom" — the counterparty (merchant / payer)
	typ    string    // "Type"
	amount float64   // abs(Net)
	net    float64   // signed Net
	items  []string  // item title / subject / note
}

func (r record) topup() bool {
	if strings.Contains(strings.ToLower(r.typ), "virement bancaire") {
		return true
	}
	return r.name == ""
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
			if strings.ToLower(filepath.Ext(e.Name())) == ".csv" {
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
		net := parseAmount(get(row, "net"))
		if net == 0 {
			continue
		}
		d := parseDate(get(row, "date"))
		when := d
		if hh := parseClock(get(row, "heure")); !hh.IsZero() && !d.IsZero() {
			when = time.Date(d.Year(), d.Month(), d.Day(), hh.Hour(), hh.Minute(), hh.Second(), 0, time.UTC)
		}
		var items []string
		for _, k := range []string{"titre de l'objet", "objet", "remarque"} {
			if v := get(row, k); v != "" {
				items = append(items, v)
			}
		}
		i.records = append(i.records, record{
			when:   when,
			date:   d,
			name:   get(row, "nom"),
			typ:    get(row, "type"),
			amount: round2(math.Abs(net)),
			net:    net,
			items:  items,
		})
	}
}

// Lookup finds the PayPal record matching the given (absolute) amount around the
// given date, and returns the real merchant + item details.
//
// Two-stage: (1) prefer an amount-matching row that has a real merchant;
// (2) otherwise, if the amount matches a balance top-up ("Virement bancaire…"),
// follow it to the payment made at the same timestamp and use that merchant.
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
	var real, topups []cand
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
		if rec.topup() {
			topups = append(topups, cand{rec, dist})
		} else {
			real = append(real, cand{rec, dist})
		}
	}

	// Stage 1: a direct payment with a real merchant.
	if len(real) > 0 {
		sort.SliceStable(real, func(a, b int) bool { return real[a].dist < real[b].dist })
		names := map[string]bool{}
		for _, c := range real {
			names[strings.ToLower(c.rec.name)] = true
		}
		return real[0].rec.name, real[0].rec.items, len(names) == 1, true
	}

	// Stage 2: the amount matches a balance top-up → find the payment it funded,
	// i.e. the closest payment (negative net, real merchant) at ~the same time.
	if len(topups) > 0 {
		sort.SliceStable(topups, func(a, b int) bool { return topups[a].dist < topups[b].dist })
		tu := topups[0].rec
		if pay, found := i.linkedPayment(tu); found {
			return pay.name, pay.items, true, true
		}
	}
	return "", nil, false, false
}

// linkedPayment returns the payment (negative net, real merchant) closest in
// time to a balance top-up — typically the order that consumed the topped-up
// balance at the same timestamp.
func (i *Index) linkedPayment(tu record) (record, bool) {
	var best record
	found := false
	bestDelta := time.Duration(1<<62 - 1)
	const maxDelta = 10 * time.Minute
	for _, rec := range i.records {
		if rec.net >= 0 || rec.name == "" || rec.topup() {
			continue
		}
		if tu.when.IsZero() || rec.when.IsZero() {
			continue
		}
		delta := rec.when.Sub(tu.when)
		if delta < 0 {
			delta = -delta
		}
		if delta <= maxDelta && delta < bestDelta {
			bestDelta = delta
			best = rec
			found = true
		}
	}
	return best, found
}

func parseAmount(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
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

func parseClock(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"15:04:05", "15:04"} {
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

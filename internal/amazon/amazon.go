// Package amazon loads an Amazon "Order History" CSV export and matches bank
// transactions to their order contents, so an Amazon withdrawal can be
// categorized from what was actually bought rather than just "Amazon".
package amazon

import (
	"encoding/csv"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const matchNearDays = 4 // widen window when no order sits exactly on the label date

// shipment is one dispatch within an order (a plausible individual bank charge).
type shipment struct {
	total    float64
	products []string
	shipDate time.Time
}

// order groups all items of one Amazon order.
type order struct {
	orderDate time.Time
	card      string
	total     float64 // whole-order total (a plausible single bank charge)
	products  []string
	shipments []shipment
}

// Index holds the parsed orders for lookup.
type Index struct {
	orders []order
}

var (
	reLast4     = regexp.MustCompile(`(\d{4})\s*$`)
	reLabelDate = regexp.MustCompile(`\b(\d{2})/(\d{2})\b`)
)

// Load parses the CSV at path. A missing/unreadable/unexpected file yields an
// empty (inert) index so the feature is simply disabled.
func Load(path string) *Index {
	if strings.TrimSpace(path) == "" {
		return &Index{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return &Index{}
	}
	text := strings.TrimPrefix(string(raw), "\ufeff") // strip UTF-8 BOM

	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil || len(records) < 2 {
		return &Index{}
	}

	col := map[string]int{}
	for i, name := range records[0] {
		col[strings.TrimSpace(name)] = i
	}
	for _, n := range []string{"Order ID", "Order Date", "Ship Date", "Total Amount", "Product Name", "Payment Method Type"} {
		if _, ok := col[n]; !ok {
			return &Index{}
		}
	}
	get := func(row []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}

	type shipAcc struct {
		total    float64
		products []string
		date     time.Time
	}
	type orderAcc struct {
		orderDate time.Time
		card      string
		total     float64
		products  []string
		ships     map[string]*shipAcc
		shipOrder []string
	}
	orders := map[string]*orderAcc{}
	var orderKeys []string

	for _, row := range records[1:] {
		oid := get(row, "Order ID")
		if oid == "" {
			continue
		}
		o, ok := orders[oid]
		if !ok {
			o = &orderAcc{
				orderDate: parseDay(get(row, "Order Date")),
				card:      last4(get(row, "Payment Method Type")),
				ships:     map[string]*shipAcc{},
			}
			orders[oid] = o
			orderKeys = append(orderKeys, oid)
		}
		amt := 0.0
		if v, err := strconv.ParseFloat(get(row, "Total Amount"), 64); err == nil {
			amt = v
		}
		name := get(row, "Product Name")

		o.total += amt
		if name != "" {
			o.products = append(o.products, name)
		}

		sd := get(row, "Ship Date")[:min(10, len(get(row, "Ship Date")))]
		s, ok := o.ships[sd]
		if !ok {
			s = &shipAcc{date: parseDay(get(row, "Ship Date"))}
			o.ships[sd] = s
			o.shipOrder = append(o.shipOrder, sd)
		}
		s.total += amt
		if name != "" {
			s.products = append(s.products, name)
		}
	}

	idx := &Index{}
	for _, oid := range orderKeys {
		o := orders[oid]
		ord := order{
			orderDate: o.orderDate,
			card:      o.card,
			total:     round2(o.total),
			products:  o.products,
		}
		for _, sd := range o.shipOrder {
			s := o.ships[sd]
			ord.shipments = append(ord.shipments, shipment{
				total:    round2(s.total),
				products: s.products,
				shipDate: s.date,
			})
		}
		idx.orders = append(idx.orders, ord)
	}
	return idx
}

// Loaded reports whether any orders are available.
func (i *Index) Loaded() bool { return i != nil && len(i.orders) > 0 }

// ParseLabelDate extracts a dd/mm date from a bank description and resolves its
// year from the Firefly transaction date, handling the year boundary (a label
// "25/12" with a Firefly date of 03/01/2026 resolves to 2025). Returns the zero
// time when no dd/mm is present or ref is zero.
func ParseLabelDate(description string, ref time.Time) time.Time {
	if ref.IsZero() {
		return time.Time{}
	}
	m := reLabelDate.FindStringSubmatch(description)
	if m == nil {
		return time.Time{}
	}
	day, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	if day < 1 || day > 31 || month < 1 || month > 12 {
		return time.Time{}
	}
	d := time.Date(ref.Year(), time.Month(month), day, 0, 0, 0, 0, time.UTC)
	// The order/payment date can't sensibly be well after the booking date; if it
	// is, the label belongs to the previous year (Dec label, Jan booking).
	if d.Sub(ref) > 45*24*time.Hour {
		d = d.AddDate(-1, 0, 0)
	} else if ref.Sub(d) > 320*24*time.Hour {
		d = d.AddDate(1, 0, 0)
	}
	return d
}

// Lookup returns the products of the order matching a bank transaction, using
// the label date as the primary key:
//   - exactly one order on that date -> take it, regardless of amount (certain);
//   - several orders on that date -> pick the most coherent amount (uncertain);
//   - none -> widen to nearby dates and apply the same rule.
//
// descDate is the date parsed from the label (preferred); fireflyDate is the
// fallback when the label has no date. certain is false when the match relied on
// amount disambiguation, so the caller can flag it for review.
func (i *Index) Lookup(amount float64, descDate, fireflyDate time.Time, card string) (products []string, certain bool, ok bool) {
	target := descDate
	if target.IsZero() {
		target = fireflyDate
	}
	if !i.Loaded() || target.IsZero() {
		return nil, false, false
	}

	// Exact-date orders.
	var exact []order
	for _, o := range i.orders {
		if sameDay(o.orderDate, target) {
			exact = append(exact, o)
		}
	}
	if p, c, ok := resolve(exact, amount, card); ok {
		return p, c, true
	}

	// Widen: orders whose order date or any ship date is near the target.
	var near []order
	for _, o := range i.orders {
		if dayDist(o.orderDate, target) <= matchNearDays {
			near = append(near, o)
			continue
		}
		for _, s := range o.shipments {
			if dayDist(s.shipDate, target) <= matchNearDays {
				near = append(near, o)
				break
			}
		}
	}
	return resolve(near, amount, card)
}

// resolve applies the unique / most-coherent-amount rule to a candidate set.
func resolve(cands []order, amount float64, card string) (products []string, certain bool, ok bool) {
	switch len(cands) {
	case 0:
		return nil, false, false
	case 1:
		// One order on this date — take it whatever the amount.
		return cands[0].products, true, true
	}

	// Several orders — pick the sub-entity (whole order or a shipment) whose
	// total is closest to the charged amount. Needs a known amount.
	if amount <= 0 {
		return nil, false, false
	}
	type opt struct {
		products []string
		dist     float64
		card     bool
	}
	best := opt{dist: math.MaxFloat64}
	for _, o := range cands {
		cm := card != "" && o.card == card
		consider := func(total float64, prods []string) {
			d := math.Abs(total - amount)
			if d < best.dist || (d == best.dist && cm && !best.card) {
				best = opt{products: prods, dist: d, card: cm}
			}
		}
		consider(o.total, o.products)
		for _, s := range o.shipments {
			consider(s.total, s.products)
		}
	}
	if best.products == nil {
		return nil, false, false
	}
	return best.products, false, true // amount-disambiguated → not certain
}

func last4(paymentMethod string) string {
	m := reLast4.FindStringSubmatch(paymentMethod)
	if m == nil {
		return ""
	}
	return m[1]
}

// parseDay reads the leading YYYY-MM-DD of an ISO timestamp into a date.
func parseDay(s string) time.Time {
	if len(s) < 10 {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", s[:10])
	if err != nil {
		return time.Time{}
	}
	return t
}

func sameDay(a, b time.Time) bool {
	return !a.IsZero() && !b.IsZero() && a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

func dayDist(a, b time.Time) int {
	if a.IsZero() || b.IsZero() {
		return 1 << 30
	}
	return int(math.Abs(a.Sub(b).Hours()) / 24)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Package amazon loads an Amazon "Order History" CSV export and matches bank
// transactions to their order contents, so an Amazon withdrawal can be
// categorized from what was actually bought rather than just "Amazon".
package amazon

import (
	"encoding/csv"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	matchAmountTolerance = 0.02 // euros
	matchMaxDays         = 7    // bank charge vs ship/order date
)

// Shipment is one Amazon shipment (grouped rows sharing an order + ship date).
// The bank is charged the sum of the items in a shipment.
type Shipment struct {
	OrderID   string
	OrderDate time.Time
	ShipDate  time.Time
	Total     float64
	CardLast4 string
	Products  []string
}

// Index holds the parsed shipments for lookup.
type Index struct {
	shipments []Shipment
}

var reLast4 = regexp.MustCompile(`(\d{4})\s*$`)

// Load parses the CSV at path. A missing or unreadable file yields an empty
// (inert) index and a nil error, so the feature is simply disabled when no
// file is present.
func Load(path string) *Index {
	if strings.TrimSpace(path) == "" {
		return &Index{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return &Index{}
	}
	// Strip a UTF-8 BOM if present so the first header name matches.
	text := strings.TrimPrefix(string(raw), "\ufeff")

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
	need := []string{"Order ID", "Order Date", "Ship Date", "Total Amount", "Product Name", "Payment Method Type"}
	for _, n := range need {
		if _, ok := col[n]; !ok {
			return &Index{} // unexpected format
		}
	}
	get := func(row []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}

	type acc struct {
		orderID   string
		orderDate time.Time
		shipDate  time.Time
		total     float64
		card      string
		products  []string
	}
	groups := map[string]*acc{}
	var order []string

	for _, row := range records[1:] {
		oid := get(row, "Order ID")
		ship := parseDay(get(row, "Ship Date"))
		key := oid + "|" + ship.Format("2006-01-02")
		g, ok := groups[key]
		if !ok {
			g = &acc{
				orderID:   oid,
				orderDate: parseDay(get(row, "Order Date")),
				shipDate:  ship,
				card:      last4(get(row, "Payment Method Type")),
			}
			groups[key] = g
			order = append(order, key)
		}
		if v, err := strconv.ParseFloat(get(row, "Total Amount"), 64); err == nil {
			g.total += v
		}
		if name := get(row, "Product Name"); name != "" {
			g.products = append(g.products, name)
		}
	}

	idx := &Index{}
	for _, key := range order {
		g := groups[key]
		idx.shipments = append(idx.shipments, Shipment{
			OrderID:   g.orderID,
			OrderDate: g.orderDate,
			ShipDate:  g.shipDate,
			Total:     round2(g.total),
			CardLast4: g.card,
			Products:  g.products,
		})
	}
	return idx
}

// Loaded reports whether any shipments are available.
func (i *Index) Loaded() bool { return i != nil && len(i.shipments) > 0 }

// Lookup finds the products of the shipment best matching a bank transaction by
// amount and date (optionally refined by the card's last 4 digits). It returns
// nothing when there is no confident, unambiguous match.
func (i *Index) Lookup(amount float64, date time.Time, cardLast4 string) ([]string, bool) {
	if !i.Loaded() || amount <= 0 {
		return nil, false
	}

	type cand struct {
		s         Shipment
		dayDiff   int
		cardMatch bool
	}
	var cands []cand
	for _, s := range i.shipments {
		if math.Abs(s.Total-amount) > matchAmountTolerance {
			continue
		}
		best := 1 << 30
		for _, d := range []time.Time{s.ShipDate, s.OrderDate} {
			if d.IsZero() || date.IsZero() {
				continue
			}
			diff := int(math.Abs(date.Sub(d).Hours()) / 24)
			if diff < best {
				best = diff
			}
		}
		if best > matchMaxDays {
			continue
		}
		cands = append(cands, cand{s, best, cardLast4 != "" && s.CardLast4 == cardLast4})
	}
	if len(cands) == 0 {
		return nil, false
	}

	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].cardMatch != cands[b].cardMatch {
			return cands[a].cardMatch
		}
		return cands[a].dayDiff < cands[b].dayDiff
	})

	best := cands[0]
	// Reject ambiguity: another equally-good candidate with different contents.
	if len(cands) > 1 {
		c2 := cands[1]
		if c2.cardMatch == best.cardMatch && c2.dayDiff == best.dayDiff && !sameProducts(best.s.Products, c2.s.Products) {
			return nil, false
		}
	}
	return best.s.Products, true
}

func sameProducts(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

func round2(v float64) float64 { return math.Round(v*100) / 100 }

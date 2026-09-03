// Package mailorder connects to an IMAP mailbox to find the order-confirmation
// email matching a bank transaction, so opaque merchants (PayPal, Amazon,
// AliExpress) can be categorized from the real order contents.
package mailorder

import (
	"crypto/tls"
	"fmt"
	stdhtml "html"
	"io"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
)

// Account holds IMAP connection settings for one mailbox.
type Account struct {
	Host     string
	Port     int
	User     string
	Password string
}

func (a Account) addr() string {
	port := a.Port
	if port == 0 {
		port = 993
	}
	return fmt.Sprintf("%s:%d", a.Host, port)
}

// dial connects and authenticates to the IMAP server.
func dial(a Account) (*client.Client, error) {
	c, err := client.DialTLS(a.addr(), &tls.Config{ServerName: a.Host})
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := c.Login(a.User, a.Password); err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("login: %w", err)
	}
	return c, nil
}

// reMerchant captures the recipient/merchant from PayPal-style wording:
// "Vous avez payé 12,95 € à MesBilles." / "Vous avez envoyé 2,00 € à tommy guilbert."
var reMerchant = regexp.MustCompile(`(?i)vous avez (?:pay[eé]|envoy[eé]).*?\sà\s+([^\n.]+?)(?:\s+avec\b|[.\n]|$)`)
var reMerchant2 = regexp.MustCompile(`(?i)en faveur de\s+([^\n.]+?)(?:[.\n]|$)`)

// ExtractMerchant tries to pull the real merchant/recipient name out of an order
// email body (currently PayPal patterns). Returns "" when nothing reliable found.
func ExtractMerchant(body string) string {
	for _, re := range []*regexp.Regexp{reMerchant, reMerchant2} {
		m := re.FindStringSubmatch(body)
		if len(m) < 2 {
			continue
		}
		name := strings.TrimSpace(m[1])
		if name == "" || len(name) > 60 || strings.Count(name, " ") > 6 {
			continue
		}
		return name
	}
	return ""
}

// Test verifies that the mailbox is reachable and the credentials work.
func Test(a Account) error {
	if a.Host == "" || a.User == "" {
		return fmt.Errorf("host and user are required")
	}
	c, err := dial(a)
	if err != nil {
		return err
	}
	return c.Logout()
}

// FindOrderEmail searches the INBOX (never Spam/other folders) for an
// order-confirmation email in [date-backDays, date+fwdDays] (order emails
// usually precede the bank charge by a few days), restricted to the given
// senders. Among candidates it prefers the email whose body contains the
// transaction amount, then the closest date. Returns the text.
// SearchResult is the outcome of an order-email search.
type SearchResult struct {
	Text        string
	Found       bool
	Installment bool
	Candidates  int // emails matching sender+date, before the amount filter
}

func FindOrderEmail(a Account, senders []string, date time.Time, amount float64, backDays, fwdDays int) (SearchResult, error) {
	if a.Host == "" || a.User == "" || date.IsZero() {
		return SearchResult{}, nil
	}
	c, err := dial(a)
	if err != nil {
		return SearchResult{}, err
	}
	defer c.Logout()

	// Only the INBOX — never Bulk/Spam or other folders.
	if _, err := c.Select("INBOX", true); err != nil {
		return SearchResult{}, fmt.Errorf("select inbox: %w", err)
	}

	since := date.AddDate(0, 0, -backDays)
	before := date.AddDate(0, 0, fwdDays+1)
	section := &imap.BodySectionName{}

	searchUIDs := func(froms []string) ([]uint32, error) {
		crit := imap.NewSearchCriteria()
		crit.Since = since
		crit.Before = before
		if len(froms) > 0 {
			crit.Header = textproto.MIMEHeader{}
			for _, f := range froms {
				crit.Header.Add("From", f)
			}
		}
		return c.Search(crit)
	}

	type cand struct {
		text        string
		dayDiff     time.Duration
		amt         bool
		installment bool
	}
	fetchCands := func(uids []uint32) ([]cand, error) {
		if len(uids) == 0 {
			return nil, nil
		}
		seqset := new(imap.SeqSet)
		for _, id := range uids {
			seqset.AddNum(id)
		}
		items := []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}
		messages := make(chan *imap.Message, 64)
		done := make(chan error, 1)
		go func() { done <- c.Fetch(seqset, items, messages) }()
		var out []cand
		for msg := range messages {
			if msg == nil {
				continue
			}
			var diff time.Duration
			if msg.Envelope != nil && !msg.Envelope.Date.IsZero() {
				d := msg.Envelope.Date.Sub(date)
				if d < 0 {
					d = -d
				}
				diff = d
			}
			r := msg.GetBody(section)
			if r == nil {
				continue
			}
			body := extractText(r)
			if body == "" {
				continue
			}
			ok, inst := amountMatch(body, amount)
			out = append(out, cand{text: body, dayDiff: diff, amt: ok, installment: inst})
		}
		if err := <-done; err != nil {
			return nil, err
		}
		return out, nil
	}
	hasAmt := func(cs []cand) bool {
		for _, c := range cs {
			if c.amt {
				return true
			}
		}
		return false
	}

	// Pass 1: filter by the configured sender(s) — fast and precise.
	var froms []string
	for _, s := range senders {
		if t := strings.TrimSpace(s); t != "" {
			froms = append(froms, t)
		}
	}
	uids, err := searchUIDs(froms)
	if err != nil {
		return SearchResult{}, fmt.Errorf("search: %w", err)
	}
	cands, err := fetchCands(uids)
	if err != nil {
		return SearchResult{}, fmt.Errorf("fetch: %w", err)
	}

	// Pass 2: senders were set but no candidate matches the amount — the
	// merchant's sender address may have changed over time. Retry ignoring the
	// sender and let the amount identify the right email.
	if len(froms) > 0 && !hasAmt(cands) {
		if uids2, serr := searchUIDs(nil); serr == nil {
			if c2, ferr := fetchCands(uids2); ferr == nil && len(c2) > 0 {
				cands = c2
			}
		}
	}

	if len(cands) == 0 {
		return SearchResult{}, nil
	}

	pick := func(list []cand) cand {
		best := list[0]
		for _, c := range list[1:] {
			if c.dayDiff < best.dayDiff {
				best = c
			}
		}
		return best
	}
	var withAmt []cand
	for _, c := range cands {
		if c.amt {
			withAmt = append(withAmt, c)
		}
	}
	var chosen cand
	switch {
	case len(withAmt) > 0:
		chosen = pick(withAmt)
	case amount <= 0 && len(cands) == 1:
		chosen = cands[0]
	default:
		// Amount known but no email matches it → don't guess.
		return SearchResult{Candidates: len(cands)}, nil
	}
	out := chosen.text
	if len(out) > 5000 {
		out = out[:5000]
	}
	return SearchResult{Text: out, Found: true, Installment: chosen.installment, Candidates: len(cands)}, nil
}

// amountMatch reports whether text contains the debit amount (±2%), and whether
// the match was via the full total of a 4-installment payment (amount*4 ±2%).
// Only numbers adjacent to a currency symbol/code are considered, so order
// numbers, phone numbers, dates, etc. never produce a false match.
func amountMatch(text string, amount float64) (matched bool, installment bool) {
	if amount < 0 {
		amount = -amount
	}
	if amount == 0 {
		return false, false
	}
	within := func(v, target float64) bool {
		if target <= 0 {
			return false
		}
		diff := v - target
		if diff < 0 {
			diff = -diff
		}
		return diff <= 0.02*target
	}
	exact, four := false, false
	for _, m := range reMoney.FindAllStringSubmatch(text, -1) {
		tok := m[1]
		if tok == "" {
			tok = m[2]
		}
		v, ok := parseAmountToken(tok)
		if !ok {
			continue
		}
		if within(v, amount) {
			exact = true
		}
		if within(v, amount*4) {
			four = true
		}
	}
	if exact {
		return true, false
	}
	if four {
		return true, true
	}
	return false, false
}

// reMoney captures a number immediately adjacent to a currency symbol/code,
// e.g. "12,95 €", "€12.95", "EUR 12.95", "12.95 EUR", "$1,234.56".
var reMoney = regexp.MustCompile(`(?i)(?:€|eur|\$|usd|£|gbp|chf)[ \x{00a0}\x{202f}\x{2009}]?([0-9][0-9 \x{00a0}\x{202f}\x{2009}.,]*[0-9])|([0-9][0-9 \x{00a0}\x{202f}\x{2009}.,]*[0-9])[ \x{00a0}\x{202f}\x{2009}]?(?:€|eur|\$|usd|£|gbp|chf)`)

func parseAmountToken(s string) (float64, bool) {
	for _, sp := range []string{"\u00a0", "\u202f", "\u2009", " "} {
		s = strings.ReplaceAll(s, sp, "")
	}
	lastDot := strings.LastIndex(s, ".")
	lastComma := strings.LastIndex(s, ",")
	dec := lastDot
	if lastComma > lastDot {
		dec = lastComma
	}
	var intPart, frac string
	if dec >= 0 {
		intPart, frac = s[:dec], s[dec+1:]
		if len(frac) != 2 { // only 2-decimal money tokens
			return 0, false
		}
	} else {
		intPart, frac = s, "00"
	}
	intPart = strings.ReplaceAll(strings.ReplaceAll(intPart, ".", ""), ",", "")
	if intPart == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(intPart+"."+frac, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

var (
	reHTMLTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	reStyleScript = regexp.MustCompile(`(?is)<(style|script|head)[^>]*>.*?</\s*(style|script|head)\s*>`)
	reWhitespace  = regexp.MustCompile(`[ \t]{2,}`)
	reBlankLines  = regexp.MustCompile(`\n{3,}`)
)

// stripHTML turns an HTML body into readable plain text.
func stripHTML(html string) string {
	s := reStyleScript.ReplaceAllString(html, " ")
	s = reHTMLTag.ReplaceAllString(s, " ")
	s = stdhtml.UnescapeString(s)
	s = strings.NewReplacer("\u00a0", " ", "\u202f", " ", "\u2009", " ", "\r", "").Replace(s)
	s = reWhitespace.ReplaceAllString(s, " ")
	// Trim each line, drop empty runs.
	lines := strings.Split(s, "\n")
	var out []string
	for _, l := range lines {
		out = append(out, strings.TrimSpace(l))
	}
	s = strings.Join(out, "\n")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// extractText reads an email body and returns its text (preferring text/plain,
// falling back to stripped HTML).
func extractText(r io.Reader) string {
	mr, err := mail.CreateReader(r)
	if err != nil {
		b, _ := io.ReadAll(r)
		return stripHTML(string(b))
	}
	var plain, html string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if _, ok := p.Header.(*mail.InlineHeader); !ok {
			continue
		}
		ct, _, _ := p.Header.(*mail.InlineHeader).ContentType()
		b, _ := io.ReadAll(p.Body)
		if strings.HasPrefix(ct, "text/plain") && strings.TrimSpace(plain) == "" {
			plain = string(b)
		} else if strings.HasPrefix(ct, "text/html") && html == "" {
			html = string(b)
		}
	}
	if strings.TrimSpace(plain) != "" {
		return strings.TrimSpace(plain)
	}
	if html != "" {
		return stripHTML(html)
	}
	return ""
}

// Package mailorder connects to an IMAP mailbox to find the order-confirmation
// email matching a bank transaction, so opaque merchants (PayPal, Amazon,
// AliExpress) can be categorized from the real order contents.
package mailorder

import (
	"crypto/tls"
	"fmt"
	stdhtml "html"
	"io"
	"math"
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
func FindOrderEmail(a Account, senders []string, date time.Time, amount float64, backDays, fwdDays int) (string, bool, error) {
	if a.Host == "" || a.User == "" || date.IsZero() {
		return "", false, nil
	}
	c, err := dial(a)
	if err != nil {
		return "", false, err
	}
	defer c.Logout()

	// Only the INBOX — never Bulk/Spam or other folders.
	if _, err := c.Select("INBOX", true); err != nil {
		return "", false, fmt.Errorf("select inbox: %w", err)
	}

	since := date.AddDate(0, 0, -backDays)
	before := date.AddDate(0, 0, fwdDays+1)

	// Collect matching UIDs. When senders are given, search each (OR); otherwise
	// fall back to a date-only search.
	idset := map[uint32]bool{}
	var searches [][]string // each is a list of From values for one criteria
	for _, s := range senders {
		s = strings.TrimSpace(s)
		if s != "" {
			searches = append(searches, []string{s})
		}
	}
	if len(searches) == 0 {
		searches = append(searches, nil)
	}
	for _, froms := range searches {
		crit := imap.NewSearchCriteria()
		crit.Since = since
		crit.Before = before
		if len(froms) > 0 {
			crit.Header = textproto.MIMEHeader{}
			for _, f := range froms {
				crit.Header.Add("From", f)
			}
		}
		ids, err := c.Search(crit)
		if err != nil {
			return "", false, fmt.Errorf("search: %w", err)
		}
		for _, id := range ids {
			idset[id] = true
		}
	}
	if len(idset) == 0 {
		return "", false, nil
	}

	seqset := new(imap.SeqSet)
	for id := range idset {
		seqset.AddNum(id)
	}
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}
	messages := make(chan *imap.Message, 30)
	done := make(chan error, 1)
	go func() { done <- c.Fetch(seqset, items, messages) }()

	type cand struct {
		text    string
		dayDiff time.Duration
		amt     bool
	}
	var cands []cand
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
		text := extractText(r)
		if text == "" {
			continue
		}
		cands = append(cands, cand{text: text, dayDiff: diff, amt: bodyHasAmount(text, amount)})
	}
	if err := <-done; err != nil {
		return "", false, fmt.Errorf("fetch: %w", err)
	}
	if len(cands) == 0 {
		return "", false, nil
	}

	// Selection: the amount is the key. Prefer emails whose body contains the
	// exact amount; among those, the closest date. If several candidates and
	// NONE matches the amount, refuse to guess (avoid the wrong email).
	pick := func(list []cand) string {
		best := ""
		bestDiff := time.Duration(1<<62 - 1)
		for _, c := range list {
			if best == "" || c.dayDiff < bestDiff {
				best = c.text
				bestDiff = c.dayDiff
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
	var chosen string
	switch {
	case len(withAmt) > 0:
		// One or more emails contain the exact amount → decisive.
		chosen = pick(withAmt)
	case amount <= 0 && len(cands) == 1:
		// Amount unknown and a single candidate → accept.
		chosen = cands[0].text
	default:
		// Amount known but no email matches it → don't guess (avoids attaching
		// an unrelated order, e.g. a PayPal "4X" installment vs a different total).
		return "", false, nil
	}
	if chosen == "" {
		return "", false, nil
	}
	if len(chosen) > 5000 {
		chosen = chosen[:5000]
	}
	return chosen, true, nil
}

// bodyHasAmount reports whether text contains a monetary value equal to amount
// (to the cent), tolerant of formats: 12,95 / 12.95 / 1 234,56 / EUR 12.95 / 12.95€.
func bodyHasAmount(text string, amount float64) bool {
	if amount <= 0 {
		return false
	}
	target := int64(math.Round(amount * 100))
	for _, tok := range reMoney.FindAllString(text, -1) {
		if v, ok := parseAmountToken(tok); ok && int64(math.Round(v*100)) == target {
			return true
		}
	}
	return false
}

var reMoney = regexp.MustCompile(`\d[\d \x{00a0}.,]*\d`)

func parseAmountToken(s string) (float64, bool) {
	s = strings.ReplaceAll(s, "\u00a0", "")
	s = strings.ReplaceAll(s, " ", "")
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
	s = strings.ReplaceAll(s, "\u00a0", " ") // non-breaking space
	s = strings.ReplaceAll(s, "\r", "")
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

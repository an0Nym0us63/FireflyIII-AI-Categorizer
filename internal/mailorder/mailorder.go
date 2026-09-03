// Package mailorder connects to an IMAP mailbox to find the order-confirmation
// email matching a bank transaction, so opaque merchants (PayPal, Amazon,
// AliExpress) can be categorized from the real order contents.
package mailorder

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/textproto"
	"regexp"
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

// FindOrderEmail searches the mailbox for an order-confirmation email near the
// given date (± windowDays), optionally restricted to a recipient alias, and
// returns the text content of the closest match (truncated).
func FindOrderEmail(a Account, recipient string, date time.Time, windowDays int) (string, bool, error) {
	if a.Host == "" || a.User == "" || date.IsZero() {
		return "", false, nil
	}
	c, err := dial(a)
	if err != nil {
		return "", false, err
	}
	defer c.Logout()

	if _, err := c.Select("INBOX", true); err != nil {
		return "", false, fmt.Errorf("select inbox: %w", err)
	}

	crit := imap.NewSearchCriteria()
	crit.Since = date.AddDate(0, 0, -windowDays)
	crit.Before = date.AddDate(0, 0, windowDays+1)
	if recipient != "" {
		crit.Header = textproto.MIMEHeader{}
		crit.Header.Add("To", recipient)
	}
	ids, err := c.Search(crit)
	if err != nil {
		return "", false, fmt.Errorf("search: %w", err)
	}
	if len(ids) == 0 {
		return "", false, nil
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(ids...)
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}
	messages := make(chan *imap.Message, 30)
	done := make(chan error, 1)
	go func() { done <- c.Fetch(seqset, items, messages) }()

	bestText := ""
	bestDiff := time.Duration(1<<62 - 1)
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
		if text != "" && diff < bestDiff {
			bestDiff = diff
			bestText = text
		}
	}
	if err := <-done; err != nil {
		return "", false, fmt.Errorf("fetch: %w", err)
	}
	if bestText == "" {
		return "", false, nil
	}
	if len(bestText) > 5000 {
		bestText = bestText[:5000]
	}
	return bestText, true, nil
}

var reHTMLTag = regexp.MustCompile(`(?s)<[^>]*>`)

// extractText reads an email body and returns its text (preferring text/plain,
// falling back to stripped HTML).
func extractText(r io.Reader) string {
	mr, err := mail.CreateReader(r)
	if err != nil {
		// Not MIME multipart — read raw.
		b, _ := io.ReadAll(r)
		return strings.TrimSpace(string(b))
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
		if strings.HasPrefix(ct, "text/plain") && plain == "" {
			plain = string(b)
		} else if strings.HasPrefix(ct, "text/html") && html == "" {
			html = string(b)
		}
	}
	if strings.TrimSpace(plain) != "" {
		return strings.TrimSpace(plain)
	}
	if html != "" {
		return strings.TrimSpace(reHTMLTag.ReplaceAllString(html, " "))
	}
	return ""
}

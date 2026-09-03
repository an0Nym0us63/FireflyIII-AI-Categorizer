// Package mailorder connects to an IMAP mailbox to find the order-confirmation
// email matching a bank transaction, so opaque merchants (PayPal, Amazon,
// AliExpress) can be categorized from the real order contents.
package mailorder

import (
	"crypto/tls"
	"fmt"

	"github.com/emersion/go-imap/client"
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

package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// Alerts leave through channels. Each one is a small adapter: a subject and
// a body go in, a provider's HTTP call or an SMTP session comes out. Adding
// a provider means adding one case here, not touching the alerting logic.
//
// Chat and SMS get a shortened message: nobody reads a traceroute on a
// phone. The full body always goes by email and webhook.

type ChannelKind string

const (
	ChanSMTP     ChannelKind = "smtp"
	ChanSendmail ChannelKind = "sendmail"
	ChanWebhook  ChannelKind = "webhook"
	ChanSlack    ChannelKind = "slack"
	ChanTeams    ChannelKind = "teams"
	ChanTelegram ChannelKind = "telegram"
	ChanTwilio   ChannelKind = "twilio" // SMS, or WhatsApp with a whatsapp: number
	ChanOVHSMS   ChannelKind = "ovh_sms"
	ChanGateway  ChannelKind = "gatewayapi"
)

type Channel struct {
	ID      string      `json:"id"`
	Kind    ChannelKind `json:"kind"`
	Name    string      `json:"name"`
	Enabled bool        `json:"enabled"`

	// SMTP / sendmail
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user,omitempty"`
	Pass     string `json:"pass,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`       // recipients, comma separated
	Security string `json:"security,omitempty"` // starttls | tls | none
	Command  string `json:"command,omitempty"`  // sendmail binary

	// HTTP providers
	URL   string `json:"url,omitempty"`   // webhook, Slack, Teams
	Token string `json:"token,omitempty"` // Telegram bot token, GatewayAPI token
	Chat  string `json:"chat,omitempty"`  // Telegram chat id

	// Twilio, OVH
	AccountSID  string `json:"account_sid,omitempty"`
	AuthToken   string `json:"auth_token,omitempty"`
	FromNumber  string `json:"from_number,omitempty"`
	ToNumbers   string `json:"to_numbers,omitempty"`
	AppKey      string `json:"app_key,omitempty"`
	AppSecret   string `json:"app_secret,omitempty"`
	ConsumerKey string `json:"consumer_key,omitempty"`
	ServiceName string `json:"service_name,omitempty"`
	Sender      string `json:"sender,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"` // OVH API root, for other regions
}

type ChannelSet struct {
	Channels []Channel `json:"channels"`
}

func (s *Store) Channels() ChannelSet {
	var c ChannelSet
	if raw := s.Setting("channels", ""); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	return c
}

func (s *Store) SetChannels(c ChannelSet) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.SetSetting("channels", string(b))
}

// checkChannel refuses a channel that could not possibly deliver, rather
// than letting it fail silently the night it matters.
func checkChannel(c *Channel) error {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		c.Name = string(c.Kind)
	}
	switch c.Kind {
	case ChanSMTP:
		if c.Host == "" || c.From == "" || strings.TrimSpace(c.To) == "" {
			return fmt.Errorf("SMTP needs a server, a sender and at least one recipient")
		}
		if c.Port == 0 {
			c.Port = 587
		}
		if c.Security == "" {
			c.Security = "starttls"
		}
		switch c.Security {
		case "starttls", "tls", "none":
		default:
			return fmt.Errorf("security must be starttls, tls or none")
		}
	case ChanSendmail:
		if c.From == "" || strings.TrimSpace(c.To) == "" {
			return fmt.Errorf("sendmail needs a sender and at least one recipient")
		}
		if c.Command == "" {
			c.Command = "/usr/sbin/sendmail"
		}
	case ChanWebhook, ChanSlack, ChanTeams:
		if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://") {
			return fmt.Errorf("this channel needs an https URL")
		}
	case ChanTelegram:
		if c.Token == "" || c.Chat == "" {
			return fmt.Errorf("Telegram needs a bot token and a chat id")
		}
	case ChanTwilio:
		if c.AccountSID == "" || c.AuthToken == "" || c.FromNumber == "" ||
			strings.TrimSpace(c.ToNumbers) == "" {
			return fmt.Errorf("Twilio needs an account SID, an auth token, a sender and a recipient")
		}
	case ChanOVHSMS:
		if c.AppKey == "" || c.AppSecret == "" || c.ConsumerKey == "" ||
			c.ServiceName == "" || strings.TrimSpace(c.ToNumbers) == "" {
			return fmt.Errorf("OVH SMS needs the three keys, the service name and a recipient")
		}
		if c.Endpoint == "" {
			c.Endpoint = "https://eu.api.ovh.com/1.0"
		}
	case ChanGateway:
		if c.Token == "" || strings.TrimSpace(c.ToNumbers) == "" {
			return fmt.Errorf("GatewayAPI needs a token and a recipient")
		}
	default:
		return fmt.Errorf("unknown channel kind %q", c.Kind)
	}
	return nil
}

// shortText is what goes to a phone or a chat room: the subject, the first
// meaningful lines, and the link. A traceroute is unreadable there.
func shortText(subject, body string, max int) string {
	var keep []string
	for _, line := range strings.Split(body, "\n") {
		// Indented lines are traceroute hops and the like: unreadable on a
		// phone, and they would eat the whole message.
		if line != strings.TrimLeft(line, " \t") {
			continue
		}
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		// Everything from the traceroute onwards is detail for the email.
		if strings.HasPrefix(l, "Traceroute") || strings.HasPrefix(l, "No traceroute") ||
			strings.HasPrefix(l, "Sent by smokestack") {
			break
		}
		keep = append(keep, l)
		if len(keep) >= 4 {
			break
		}
	}
	out := subject
	if len(keep) > 0 {
		out += " — " + strings.Join(keep, " ")
	}
	out = strings.Join(strings.Fields(out), " ")
	if len(out) > max {
		out = out[:max-1] + "…"
	}
	return out
}

var notifyClient = &http.Client{Timeout: 15 * time.Second}

func postJSON(u string, payload any, headers map[string]string) error {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doAndCheck(req)
}

func doAndCheck(req *http.Request) error {
	resp, err := notifyClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	return nil
}

// Send delivers one message through one channel.
func (c Channel) Send(subject, body string) error {
	switch c.Kind {
	case ChanSMTP:
		return c.sendSMTP(subject, body)
	case ChanSendmail:
		return c.sendSendmail(subject, body)
	case ChanWebhook:
		return postJSON(c.URL, map[string]any{"subject": subject, "body": body}, nil)
	case ChanSlack:
		return postJSON(c.URL, map[string]any{"text": subject + "\n```\n" + body + "```"}, nil)
	case ChanTeams:
		// Works both with the old connector payload and with a Workflows
		// webhook, which accepts the same "text" field.
		return postJSON(c.URL, map[string]any{
			"@type": "MessageCard", "@context": "https://schema.org/extensions",
			"summary": subject, "title": subject, "text": strings.ReplaceAll(body, "\n", "\n\n"),
		}, nil)
	case ChanTelegram:
		return postJSON("https://api.telegram.org/bot"+c.Token+"/sendMessage",
			map[string]any{"chat_id": c.Chat, "text": subject + "\n\n" + body,
				"disable_web_page_preview": true}, nil)
	case ChanTwilio:
		return c.sendTwilio(shortText(subject, body, 1400))
	case ChanOVHSMS:
		return c.sendOVH(shortText(subject, body, 480))
	case ChanGateway:
		return c.sendGatewayAPI(shortText(subject, body, 480))
	}
	return fmt.Errorf("unknown channel kind %q", c.Kind)
}

func (c Channel) sendSMTP(subject, body string) error {
	to := splitList(c.To)
	msg := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		c.From, strings.Join(to, ", "), subject, body))
	addr := net.JoinHostPort(c.Host, fmt.Sprint(c.Port))

	var conn net.Conn
	var err error
	if c.Security == "tls" { // implicit TLS, usually port 465
		conn, err = tls.Dial("tcp", addr, &tls.Config{ServerName: c.Host})
	} else {
		conn, err = net.DialTimeout("tcp", addr, 15*time.Second)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return err
	}
	defer cl.Close()
	if c.Security == "starttls" {
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
				return err
			}
		}
	}
	if c.User != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.User, c.Pass, c.Host)); err != nil {
			return err
		}
	}
	if err := cl.Mail(c.From); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := cl.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}

// sendSendmail hands the message to the local mail system, for a server
// that already relays mail and where no SMTP credentials are wanted.
func (c Channel) sendSendmail(subject, body string) error {
	to := splitList(c.To)
	msg := fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\n"+
		"MIME-Version: 1.0\nContent-Type: text/plain; charset=utf-8\n\n%s",
		c.From, strings.Join(to, ", "), subject, body)
	args := append([]string{"-f", c.From}, to...)
	cmd := exec.Command(c.Command, args...)
	cmd.Stdin = strings.NewReader(msg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v %s", c.Command, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c Channel) sendTwilio(text string) error {
	base := c.Endpoint
	if base == "" {
		base = "https://api.twilio.com"
	}
	for _, to := range splitList(c.ToNumbers) {
		form := url.Values{"From": {c.FromNumber}, "To": {to}, "Body": {text}}
		req, err := http.NewRequest("POST",
			base+"/2010-04-01/Accounts/"+c.AccountSID+"/Messages.json",
			strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(c.AccountSID, c.AuthToken)
		if err := doAndCheck(req); err != nil {
			return err
		}
	}
	return nil
}

// sendOVH signs the request the way the OVH API expects: SHA-1 over the
// application secret, the consumer key, the method, the URL, the body and
// the timestamp.
func (c Channel) sendOVH(text string) error {
	u := fmt.Sprintf("%s/sms/%s/jobs", strings.TrimSuffix(c.Endpoint, "/"), c.ServiceName)
	sender := c.Sender
	payload := map[string]any{
		"message": text, "receivers": splitList(c.ToNumbers),
		"charset": "UTF-8", "noStopClause": true, "priority": "high",
	}
	if sender != "" {
		payload["sender"] = sender
	} else {
		payload["senderForResponse"] = true
	}
	b, _ := json.Marshal(payload)
	ts := fmt.Sprint(time.Now().Unix())
	sum := sha1.Sum([]byte(strings.Join([]string{c.AppSecret, c.ConsumerKey, "POST", u, string(b), ts}, "+")))
	req, err := http.NewRequest("POST", u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ovh-Application", c.AppKey)
	req.Header.Set("X-Ovh-Consumer", c.ConsumerKey)
	req.Header.Set("X-Ovh-Timestamp", ts)
	req.Header.Set("X-Ovh-Signature", fmt.Sprintf("$1$%x", sum))
	return doAndCheck(req)
}

func (c Channel) sendGatewayAPI(text string) error {
	base := c.Endpoint
	if base == "" {
		base = "https://gatewayapi.com"
	}
	var recipients []map[string]any
	for _, n := range splitList(c.ToNumbers) {
		recipients = append(recipients, map[string]any{"msisdn": strings.TrimLeft(n, "+")})
	}
	sender := c.Sender
	if sender == "" {
		sender = "smokestack"
	}
	req, err := http.NewRequest("POST", base+"/rest/mtsms", nil)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{"sender": sender, "message": text, "recipients": recipients})
	req.Body = io.NopCloser(bytes.NewReader(b))
	req.ContentLength = int64(len(b))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.Token, "")
	return doAndCheck(req)
}

// SendAll delivers through every enabled channel and reports what failed,
// without letting one broken provider stop the others.
func SendAll(set ChannelSet, subject, body string) error {
	var failures []string
	sent := 0
	for _, c := range set.Channels {
		if !c.Enabled {
			continue
		}
		if err := c.Send(subject, body); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", c.Name, err))
			continue
		}
		sent++
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d channel(s) delivered, %s", sent, strings.Join(failures, "; "))
	}
	return nil
}

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io"
	"log"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/valyala/fasthttp"
)

type emailCampaignConfig struct {
	CampaignID  string                    `json:"campaign_id"`
	Description string                    `json:"description"`
	FromEmail   string                    `json:"from_email"`
	FromName    string                    `json:"from_name"`
	ReplyTo     string                    `json:"reply_to"`
	Emails      []emailCampaignDefinition `json:"emails"`
}

type emailCampaignDefinition struct {
	ID        string `json:"id"`
	DelayDays int    `json:"delay_days"`
	Subject   string `json:"subject"`
	Preheader string `json:"preheader"`
	Template  string `json:"template"`
}

type compiledCampaignEmail struct {
	emailCampaignDefinition
	html *htmltemplate.Template
	text *texttemplate.Template
}

type emailCampaign struct {
	cfg      Config
	db       *sql.DB
	meta     emailCampaignConfig
	emails   []compiledCampaignEmail
	sendMail func(to, subject, textBody, htmlBody, unsubscribeURL string) (string, error)
}

type campaignTemplateData struct {
	SiteBaseURL, CampaignID, UnsubscribeURL, PostalAddress string
}

func newEmailCampaign(cfg Config, db *sql.DB) (*emailCampaign, error) {
	body, err := os.ReadFile(cfg.EmailCampaignPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", cfg.EmailCampaignPath, err)
	}
	var meta emailCampaignConfig
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("parse %s: %w", cfg.EmailCampaignPath, err)
	}
	if !safeCampaignID(meta.CampaignID) || len(meta.Emails) == 0 {
		return nil, errors.New("campaign_id and at least one email are required")
	}

	dir := filepath.Dir(cfg.EmailCampaignPath)
	seen := make(map[string]bool, len(meta.Emails))
	compiled := make([]compiledCampaignEmail, 0, len(meta.Emails))
	previousDelay := -1
	for _, item := range meta.Emails {
		if !safeCampaignID(item.ID) || !safeCampaignID(item.Template) || seen[item.ID] {
			return nil, fmt.Errorf("invalid or duplicate campaign email %q", item.ID)
		}
		if item.DelayDays < previousDelay || strings.TrimSpace(item.Subject) == "" {
			return nil, fmt.Errorf("email %q has an invalid delay or subject", item.ID)
		}
		seen[item.ID] = true
		previousDelay = item.DelayDays

		htmlPath := filepath.Join(dir, item.Template+".html")
		textPath := filepath.Join(dir, item.Template+".txt")
		htmlBody, err := os.ReadFile(htmlPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", htmlPath, err)
		}
		textBody, err := os.ReadFile(textPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", textPath, err)
		}
		ht, err := htmltemplate.New(item.ID).Option("missingkey=error").Parse(string(htmlBody))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", htmlPath, err)
		}
		tt, err := texttemplate.New(item.ID).Option("missingkey=error").Parse(string(textBody))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", textPath, err)
		}
		compiled = append(compiled, compiledCampaignEmail{emailCampaignDefinition: item, html: ht, text: tt})
	}

	c := &emailCampaign{cfg: cfg, db: db, meta: meta, emails: compiled}
	c.sendMail = c.sendSES
	return c, nil
}

func safeCampaignID(v string) bool {
	if v == "" || len(v) > 80 {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (c *emailCampaign) validateDelivery() error {
	if c == nil {
		return errors.New("campaign is not configured")
	}
	missing := make([]string, 0, 5)
	if c.cfg.AWSSMTPUsername == "" {
		missing = append(missing, "AWS_SMTP_USERNAME")
	}
	if c.cfg.AWSSMTPPassword == "" {
		missing = append(missing, "AWS_SMTP_PASSWORD")
	}
	if len(c.cfg.EmailUnsubscribeSecret) < 32 {
		missing = append(missing, "EMAIL_UNSUBSCRIBE_SECRET (at least 32 characters)")
	}
	postal := strings.TrimSpace(c.cfg.EmailPostalAddress)
	if postal == "" || strings.Contains(strings.ToLower(postal), "replace") {
		missing = append(missing, "EMAIL_POSTAL_ADDRESS")
	}
	if len(missing) > 0 {
		return fmt.Errorf("EMAIL_DRIP_ENABLED requires %s", strings.Join(missing, ", "))
	}
	if err := exactMailbox(c.cfg.EmailFromEmail); err != nil {
		return fmt.Errorf("SES_FROM_EMAIL: %w", err)
	}
	if err := exactMailbox(c.cfg.EmailReplyTo); err != nil {
		return fmt.Errorf("SES_REPLY_TO: %w", err)
	}
	if c.cfg.AWSRegion == "" {
		return errors.New("AWS_REGION is required")
	}
	return nil
}

func exactMailbox(value string) error {
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value {
		return errors.New("must be one plain email address")
	}
	return nil
}

func (s *Server) startEmailDrip() error {
	if s.email == nil || !s.cfg.EmailDripEnabled {
		log.Printf("email campaign: staged, delivery disabled")
		return nil
	}
	if err := s.email.validateDelivery(); err != nil {
		return err
	}
	go func() {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		<-timer.C
		s.runEmailDripTick()
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.runEmailDripTick()
		}
	}()
	log.Printf("email campaign: %s enabled via SES %s", s.email.meta.CampaignID, s.cfg.AWSRegion)
	return nil
}

func (s *Server) runEmailDripTick() {
	sent, failed, err := s.email.sendDue(50, time.Now().UTC())
	if err != nil {
		log.Printf("email campaign tick: %v", err)
		return
	}
	if sent > 0 || failed > 0 {
		log.Printf("email campaign tick: sent=%d failed=%d", sent, failed)
	}
}

func (c *emailCampaign) setPreference(userID string, enabled bool) error {
	if c == nil || c.db == nil || userID == "" {
		return errors.New("email preferences unavailable")
	}
	now := time.Now().UTC()
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previouslyEnabled bool
	err = tx.QueryRow(`SELECT product_updates FROM email_preferences WHERE user_id=$1 FOR UPDATE`, userID).Scan(&previouslyEnabled)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err = tx.Exec(`
		INSERT INTO email_preferences (user_id,product_updates,opted_in_at,unsubscribed_at,updated_at)
		VALUES ($1,$2,CASE WHEN $2 THEN $3::timestamptz ELSE NULL END,CASE WHEN $2 THEN NULL ELSE $3::timestamptz END,$3)
		ON CONFLICT (user_id) DO UPDATE SET
			product_updates=EXCLUDED.product_updates,
			opted_in_at=CASE WHEN EXCLUDED.product_updates AND NOT email_preferences.product_updates THEN EXCLUDED.updated_at ELSE email_preferences.opted_in_at END,
			unsubscribed_at=CASE WHEN EXCLUDED.product_updates THEN NULL ELSE EXCLUDED.updated_at END,
			updated_at=EXCLUDED.updated_at`, userID, enabled, now); err != nil {
		return err
	}
	if enabled && !previouslyEnabled {
		if _, err = tx.Exec(`INSERT INTO email_campaign_enrollments (campaign_id,user_id,enrolled_at)
			VALUES ($1,$2,$3) ON CONFLICT (campaign_id,user_id) DO NOTHING`, c.meta.CampaignID, userID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (c *emailCampaign) preference(userID string) (bool, error) {
	var enabled bool
	err := c.db.QueryRow(`SELECT product_updates FROM email_preferences WHERE user_id=$1`, userID).Scan(&enabled)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return enabled, err
}

func (c *emailCampaign) unsubscribeToken(userID string) string {
	if c == nil || userID == "" || len(c.cfg.EmailUnsubscribeSecret) < 32 {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(c.cfg.EmailUnsubscribeSecret))
	mac.Write([]byte("mojojojo-email-unsubscribe-v1\x00" + userID))
	return userID + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (c *emailCampaign) verifyUnsubscribeToken(token string) (string, bool) {
	cut := strings.LastIndexByte(token, '.')
	if cut <= 0 || len(c.cfg.EmailUnsubscribeSecret) < 32 {
		return "", false
	}
	userID := token[:cut]
	want := c.unsubscribeToken(userID)
	return userID, hmac.Equal([]byte(want), []byte(token))
}

func (c *emailCampaign) unsubscribeURL(userID string) string {
	token := c.unsubscribeToken(userID)
	if token == "" {
		return ""
	}
	return c.cfg.SiteBaseURL + "/email/unsubscribe?token=" + url.QueryEscape(token)
}

func (c *emailCampaign) render(item compiledCampaignEmail, userID string) (string, string, error) {
	data := campaignTemplateData{
		SiteBaseURL: c.cfg.SiteBaseURL, CampaignID: c.meta.CampaignID,
		UnsubscribeURL: c.unsubscribeURL(userID), PostalAddress: c.cfg.EmailPostalAddress,
	}
	if data.UnsubscribeURL == "" {
		return "", "", errors.New("unsubscribe signing secret is not configured")
	}
	var fragment, textBody bytes.Buffer
	if err := item.html.Execute(&fragment, data); err != nil {
		return "", "", err
	}
	if err := item.text.Execute(&textBody, data); err != nil {
		return "", "", err
	}
	htmlBody, err := renderEmailShell(item, data, htmltemplate.HTML(fragment.String()))
	if err != nil {
		return "", "", err
	}
	plain := strings.TrimSpace(textBody.String()) + "\n\nYou opted in to the mojojojo product and open-source introduction.\nUnsubscribe: " + data.UnsubscribeURL + "\n" + data.PostalAddress + "\n"
	return plain, htmlBody, nil
}

func renderEmailShell(item compiledCampaignEmail, data campaignTemplateData, body htmltemplate.HTML) (string, error) {
	const shell = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>{{.Subject}}</title></head>
<body style="margin:0;background:#282c34;color:#d9dee7;font:16px/1.6 Arial,sans-serif">
<div style="display:none;max-height:0;overflow:hidden;opacity:0">{{.Preheader}}</div>
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="background:#282c34"><tr><td align="center" style="padding:28px 14px">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="max-width:620px;background:#323842;border:1px solid #4b5361;border-radius:16px"><tr><td style="padding:28px">
<p style="margin:0 0 24px;font-size:20px;font-weight:800;color:#d9dee7">mojo<span style="color:#98c379">jojo</span></p>
<h1 style="margin:0 0 18px;font-size:28px;line-height:1.15;color:#d9dee7">{{.Subject}}</h1>
<div style="color:#d9dee7">{{.Body}}</div>
<hr style="border:0;border-top:1px solid #4b5361;margin:28px 0 18px">
<p style="margin:0 0 8px;color:#abb2bf;font-size:12px">You opted in to the six-part mojojojo product and open-source introduction.</p>
<p style="margin:0;color:#abb2bf;font-size:12px"><a href="{{.UnsubscribeURL}}" style="color:#98c379">Unsubscribe</a> · {{.PostalAddress}}</p>
</td></tr></table></td></tr></table></body></html>`
	t, err := htmltemplate.New("shell").Parse(shell)
	if err != nil {
		return "", err
	}
	view := struct {
		Subject, Preheader, UnsubscribeURL, PostalAddress string
		Body                                              htmltemplate.HTML
	}{item.Subject, item.Preheader, data.UnsubscribeURL, data.PostalAddress, body}
	var out bytes.Buffer
	if err := t.Execute(&out, view); err != nil {
		return "", err
	}
	// Class styling in fragments is promoted to inline style because several
	// inboxes remove style blocks.
	html := strings.ReplaceAll(out.String(), `class="button"`, `style="display:inline-block;background:#98c379;color:#20252c;text-decoration:none;font-weight:700;padding:11px 18px;border-radius:10px"`)
	html = strings.ReplaceAll(html, `<a href=`, `<a style="color:#98c379" href=`)
	return html, nil
}

func (c *emailCampaign) sendDue(limit int, now time.Time) (sent, failed int, err error) {
	if err := c.validateDelivery(); err != nil {
		return 0, 0, err
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	for i, item := range c.emails {
		previousID := ""
		if i > 0 {
			previousID = c.emails[i-1].ID
		}
		rows, queryErr := c.db.Query(`
			SELECT e.user_id,u.email
			FROM email_campaign_enrollments e
			JOIN email_preferences p ON p.user_id=e.user_id
			JOIN users u ON u.id=e.user_id
			WHERE e.campaign_id=$1 AND e.enrolled_at <= $2
			  AND p.product_updates=TRUE AND p.unsubscribed_at IS NULL AND u.disabled_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM email_campaign_sends s
			    WHERE s.campaign_id=e.campaign_id AND s.email_id=$3 AND s.user_id=e.user_id AND s.status IN ('sending','sent'))
			  AND ($4='' OR EXISTS (SELECT 1 FROM email_campaign_sends prior
			    WHERE prior.campaign_id=e.campaign_id AND prior.email_id=$4 AND prior.user_id=e.user_id AND prior.status='sent'))
			ORDER BY e.enrolled_at LIMIT $5`, c.meta.CampaignID, now.Add(-time.Duration(item.DelayDays)*24*time.Hour), item.ID, previousID, limit)
		if queryErr != nil {
			return sent, failed, queryErr
		}
		type candidate struct{ userID, email string }
		candidates := make([]candidate, 0)
		for rows.Next() {
			var candidate candidate
			if scanErr := rows.Scan(&candidate.userID, &candidate.email); scanErr != nil {
				rows.Close()
				return sent, failed, scanErr
			}
			candidates = append(candidates, candidate)
		}
		if rowsErr := rows.Close(); rowsErr != nil {
			return sent, failed, rowsErr
		}
		for _, candidate := range candidates {
			reserved, reserveErr := c.reserveSend(item.ID, candidate.userID, now)
			if reserveErr != nil {
				return sent, failed, reserveErr
			}
			if !reserved {
				continue
			}
			textBody, htmlBody, renderErr := c.render(item, candidate.userID)
			if renderErr != nil {
				c.failSend(item.ID, candidate.userID, renderErr)
				failed++
				continue
			}
			messageID, sendErr := c.sendMail(candidate.email, item.Subject, textBody, htmlBody, c.unsubscribeURL(candidate.userID))
			if sendErr != nil {
				c.failSend(item.ID, candidate.userID, sendErr)
				failed++
				continue
			}
			if _, updateErr := c.db.Exec(`UPDATE email_campaign_sends SET status='sent',sent_at=$4,message_id=$5,error=''
				WHERE campaign_id=$1 AND email_id=$2 AND user_id=$3`, c.meta.CampaignID, item.ID, candidate.userID, time.Now().UTC(), messageID); updateErr != nil {
				return sent, failed, updateErr
			}
			sent++
		}
	}
	return sent, failed, nil
}

func (c *emailCampaign) reserveSend(emailID, userID string, now time.Time) (bool, error) {
	var attempts int
	err := c.db.QueryRow(`
		INSERT INTO email_campaign_sends (campaign_id,email_id,user_id,status,attempt_count,attempted_at)
		VALUES ($1,$2,$3,'sending',1,$4)
		ON CONFLICT (campaign_id,email_id,user_id) DO UPDATE SET
			status='sending',attempt_count=email_campaign_sends.attempt_count+1,attempted_at=EXCLUDED.attempted_at,error=''
		WHERE email_campaign_sends.status IN ('failed','sending')
		  AND email_campaign_sends.attempt_count < 3
		  AND email_campaign_sends.attempted_at < EXCLUDED.attempted_at - INTERVAL '1 hour'
		RETURNING attempt_count`, c.meta.CampaignID, emailID, userID, now).Scan(&attempts)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (c *emailCampaign) failSend(emailID, userID string, sendErr error) {
	message := sendErr.Error()
	if len(message) > 500 {
		message = message[:500]
	}
	_, _ = c.db.Exec(`UPDATE email_campaign_sends SET status='failed',error=$4
		WHERE campaign_id=$1 AND email_id=$2 AND user_id=$3`, c.meta.CampaignID, emailID, userID, message)
}

func (c *emailCampaign) sendSES(to, subject, textBody, htmlBody, unsubscribeURL string) (string, error) {
	if err := exactMailbox(to); err != nil {
		return "", fmt.Errorf("recipient: %w", err)
	}
	if strings.ContainsAny(subject, "\r\n") || strings.ContainsAny(unsubscribeURL, "\r\n") {
		return "", errors.New("invalid mail header")
	}
	host := "email-smtp." + c.cfg.AWSRegion + ".amazonaws.com"
	address := net.JoinHostPort(host, "465")
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", address, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host})
	if err != nil {
		return "", fmt.Errorf("connect SES: %w", err)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return "", err
	}
	defer client.Close()
	if err := client.Auth(smtp.PlainAuth("", c.cfg.AWSSMTPUsername, c.cfg.AWSSMTPPassword, host)); err != nil {
		return "", fmt.Errorf("authenticate SES: %w", err)
	}
	if err := client.Mail(c.cfg.EmailFromEmail); err != nil {
		return "", err
	}
	if err := client.Rcpt(to); err != nil {
		return "", err
	}
	w, err := client.Data()
	if err != nil {
		return "", err
	}
	messageID := randomToken(18) + "@mojojojo.cc"
	raw, err := c.rawMessage(to, subject, textBody, htmlBody, unsubscribeURL, messageID)
	if err == nil {
		_, err = io.WriteString(w, raw)
	}
	closeErr := w.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := client.Quit(); err != nil {
		return "", err
	}
	return messageID, nil
}

func (c *emailCampaign) rawMessage(to, subject, textBody, htmlBody, unsubscribeURL, messageID string) (string, error) {
	boundary := "mjj-" + randomToken(18)
	from := (&mail.Address{Name: c.cfg.EmailFromName, Address: c.cfg.EmailFromEmail}).String()
	reply := (&mail.Address{Address: c.cfg.EmailReplyTo}).String()
	var out strings.Builder
	headers := []string{
		"From: " + from, "To: " + to, "Reply-To: " + reply,
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"Date: " + time.Now().UTC().Format(time.RFC1123Z), "Message-ID: <" + messageID + ">",
		"MIME-Version: 1.0", `Content-Type: multipart/alternative; boundary="` + boundary + `"`,
		"List-Unsubscribe: <" + unsubscribeURL + ">", "List-Unsubscribe-Post: List-Unsubscribe=One-Click",
		"X-Auto-Response-Suppress: All",
	}
	for _, header := range headers {
		if strings.ContainsAny(header, "\r\n") {
			return "", errors.New("invalid mail header")
		}
		out.WriteString(header + "\r\n")
	}
	out.WriteString("\r\n")
	writePart := func(contentType, body string) error {
		out.WriteString("--" + boundary + "\r\nContent-Type: " + contentType + "; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		var encoded bytes.Buffer
		writer := quotedprintable.NewWriter(&encoded)
		if _, err := writer.Write([]byte(body)); err != nil {
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
		out.Write(encoded.Bytes())
		out.WriteString("\r\n")
		return nil
	}
	if err := writePart("text/plain", textBody); err != nil {
		return "", err
	}
	if err := writePart("text/html", htmlBody); err != nil {
		return "", err
	}
	out.WriteString("--" + boundary + "--\r\n")
	return out.String(), nil
}

func (s *Server) handleEmailPreferences(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromSession(ctx)
	if user == nil {
		redirectLocal(ctx, "/auth/login?next=%2Femail%2Fpreferences")
		return
	}
	message := ""
	if string(ctx.Method()) == "POST" {
		if !s.validAuthForm(ctx) {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			message = "The form expired. Please try again."
		} else if err := s.email.setPreference(user.ID, string(ctx.FormValue("product_updates")) == "on"); err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			message = "Could not save your email preference."
		} else {
			message = "Email preference saved."
		}
	}
	enabled, _ := s.email.preference(user.ID)
	csrf := authFormToken(ctx)
	if csrf == "" {
		csrf = randomToken(24)
	}
	s.setCookie(ctx, authFormCookie, csrf, authFormTTL)
	s.render(ctx, "email_preferences.html", map[string]any{
		"Meta": s.meta("Email preferences — mojojojo", ""), "User": user,
		"Enabled": enabled, "Message": message, "CSRF": csrf,
	})
}

func (s *Server) handleEmailUnsubscribe(ctx *fasthttp.RequestCtx) {
	token := strings.TrimSpace(string(ctx.QueryArgs().Peek("token")))
	if token == "" {
		token = strings.TrimSpace(string(ctx.FormValue("token")))
	}
	userID, valid := s.email.verifyUnsubscribeToken(token)
	if !valid {
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		s.render(ctx, "email_unsubscribe.html", map[string]any{
			"Meta": s.meta("Email preferences — mojojojo", ""), "Invalid": true,
		})
		return
	}
	if string(ctx.Method()) == "POST" {
		if err := s.email.setPreference(userID, false); err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			ctx.SetContentType("text/plain; charset=utf-8")
			ctx.WriteString("Could not update email preference.")
			return
		}
		if strings.EqualFold(string(ctx.Request.Header.Peek("List-Unsubscribe")), "One-Click") ||
			string(ctx.FormValue("List-Unsubscribe")) == "One-Click" {
			ctx.SetContentType("text/plain; charset=utf-8")
			ctx.WriteString("Unsubscribed")
			return
		}
		s.render(ctx, "email_unsubscribe.html", map[string]any{
			"Meta": s.meta("Email preferences — mojojojo", ""), "Saved": true,
		})
		return
	}
	s.render(ctx, "email_unsubscribe.html", map[string]any{
		"Meta": s.meta("Email preferences — mojojojo", ""), "Token": token,
	})
}

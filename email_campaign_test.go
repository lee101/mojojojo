package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func campaignTestConfig() Config {
	cfg := loadConfig()
	cfg.SiteBaseURL = "https://mojojojo.cc"
	cfg.EmailCampaignPath = "emails/drip_config.json"
	cfg.EmailFromEmail = "lee.penkman@netwrck.com"
	cfg.EmailFromName = "Lee at mojojojo"
	cfg.EmailReplyTo = "lee.penkman@netwrck.com"
	cfg.EmailPostalAddress = "1 Test Street, Test City, New Zealand"
	cfg.EmailUnsubscribeSecret = strings.Repeat("s", 32)
	cfg.AWSSMTPUsername = "smtp-user"
	cfg.AWSSMTPPassword = "smtp-password"
	cfg.AWSRegion = "us-east-1"
	return cfg
}

func TestCampaignTemplatesRenderWithRequiredFooter(t *testing.T) {
	c, err := newEmailCampaign(campaignTestConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.emails) != 6 {
		t.Fatalf("campaign has %d emails, want 6", len(c.emails))
	}
	for _, item := range c.emails {
		plain, html, err := c.render(item, "user-token-safe")
		if err != nil {
			t.Fatalf("%s: %v", item.ID, err)
		}
		for _, body := range []string{plain, html} {
			for _, want := range []string{"email/unsubscribe?token=", "mojojojo", c.cfg.EmailPostalAddress} {
				if !strings.Contains(body, want) {
					t.Errorf("%s body missing %q", item.ID, want)
				}
			}
		}
		if strings.Contains(html, "<no value>") {
			t.Errorf("%s rendered a missing value", item.ID)
		}
	}
}

func TestUnsubscribeTokenRejectsTampering(t *testing.T) {
	c, err := newEmailCampaign(campaignTestConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	token := c.unsubscribeToken("u1")
	if userID, ok := c.verifyUnsubscribeToken(token); !ok || userID != "u1" {
		t.Fatalf("valid token rejected: user=%q valid=%v", userID, ok)
	}
	if _, ok := c.verifyUnsubscribeToken(token + "x"); ok {
		t.Fatal("tampered token accepted")
	}
}

func TestCampaignRequiresComplianceAndSESConfig(t *testing.T) {
	cfg := campaignTestConfig()
	cfg.EmailPostalAddress = ""
	cfg.AWSSMTPPassword = ""
	c, err := newEmailCampaign(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = c.validateDelivery()
	if err == nil || !strings.Contains(err.Error(), "EMAIL_POSTAL_ADDRESS") || !strings.Contains(err.Error(), "AWS_SMTP_PASSWORD") {
		t.Fatalf("incomplete delivery configuration accepted: %v", err)
	}
}

func TestOptInSendsSequenceOnceAndOptOutStopsIt(t *testing.T) {
	db := testPostgres(t)
	created := time.Now().UTC().Add(-40 * 24 * time.Hour)
	if _, err := db.Exec(`INSERT INTO users (id,email,password_hash,created_at) VALUES ('mail-user','mail@example.com','x',$1)`, created); err != nil {
		t.Fatal(err)
	}
	c, err := newEmailCampaign(campaignTestConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	var delivered []string
	c.sendMail = func(to, subject, textBody, htmlBody, unsubscribeURL string) (string, error) {
		if to != "mail@example.com" || unsubscribeURL == "" || textBody == "" || htmlBody == "" {
			t.Fatalf("invalid delivery payload to=%q unsubscribe=%q", to, unsubscribeURL)
		}
		delivered = append(delivered, subject)
		return fmt.Sprintf("message-%d@mojojojo.cc", len(delivered)), nil
	}
	if err := c.setPreference("mail-user", true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE email_campaign_enrollments SET enrolled_at=$1 WHERE user_id='mail-user'`, created); err != nil {
		t.Fatal(err)
	}
	sent, failed, err := c.sendDue(50, time.Now().UTC())
	if err != nil || failed != 0 || sent != len(c.emails) {
		t.Fatalf("first tick sent=%d failed=%d err=%v", sent, failed, err)
	}
	sent, failed, err = c.sendDue(50, time.Now().UTC())
	if err != nil || sent != 0 || failed != 0 || len(delivered) != len(c.emails) {
		t.Fatalf("duplicate tick sent=%d failed=%d deliveries=%d err=%v", sent, failed, len(delivered), err)
	}
	if err := c.setPreference("mail-user", false); err != nil {
		t.Fatal(err)
	}
	if enabled, err := c.preference("mail-user"); err != nil || enabled {
		t.Fatalf("opt out not recorded: enabled=%v err=%v", enabled, err)
	}
}

func TestRawCampaignMessageHasOneClickHeadersAndBothBodies(t *testing.T) {
	c, err := newEmailCampaign(campaignTestConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.rawMessage("mail@example.com", "A subject", "plain", "<b>html</b>", "https://mojojojo.cc/email/unsubscribe?token=x", "id@mojojojo.cc")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"List-Unsubscribe: <https://mojojojo.cc/email/unsubscribe?token=x>",
		"List-Unsubscribe-Post: List-Unsubscribe=One-Click",
		"Content-Type: text/plain", "Content-Type: text/html", "Message-ID: <id@mojojojo.cc>",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw message missing %q", want)
		}
	}
}

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                 string
	Debug                bool
	SiteBaseURL          string
	RunnerURL            string
	VectorURL            string
	VectorToken          string
	AgentURL             string
	AgentToken           string
	MaxAgentRuns         int
	AgentTokensPerCredit float64
	DatabaseURL          string
	// EdgeURL is the Cloudflare Worker in wasmworker/, if one is deployed.
	// EdgeToken is the secret it checks; without it, anybody who finds the URL
	// is spending our Cloudflare budget.
	EdgeURL    string
	EdgeToken  string
	LoginURL   string
	AccountURL string
	SignupURL  string
	TopUpURL   string

	// Stripe is optional at process startup so a developer can still run the
	// execution service without payment credentials. When configured, Checkout
	// Sessions are embedded on /credits and webhooks move paid value into the
	// local credit ledger. Price IDs are public identifiers; keys and the
	// webhook signing secret belong in the deployment's secrets vault.
	StripeSecretKey               string
	StripePublishableKey          string
	StripeWebhookSecret           string
	StripeCreditsPriceID          string
	StripeDeveloperMonthlyPriceID string
	StripeDeveloperAnnualPriceID  string
	StripeDedicatedMonthlyPriceID string
	StripeDedicatedAnnualPriceID  string
	StripeAutomaticTax            bool

	// Product introduction email. Delivery is opt-in and disabled unless the
	// complete SES SMTP and compliance configuration is present.
	EmailDripEnabled       bool
	EmailCampaignPath      string
	EmailFromEmail         string
	EmailFromName          string
	EmailReplyTo           string
	EmailPostalAddress     string
	EmailUnsubscribeSecret string
	AWSSMTPUsername        string
	AWSSMTPPassword        string
	AWSRegion              string

	// Price of compute, in milliseconds per mojojojo credit. One credit is
	// $0.001, so 10000 ms/credit is $0.36 per core-hour and 2000 ms/credit on
	// a GPU is $1.80 per GPU-hour.
	MsPerCredit    float64
	MsPerCreditGPU float64

	// Object storage. Provider-specific credentials stay internal; every URL
	// returned to a customer uses the mojojojo static domain.
	R2Endpoint        string
	R2AccessKey       string
	R2SecretKey       string
	R2Bucket          string
	R2Prefix          string
	CDNBase           string
	StorageSigningKey string
	StorageFreeBytes  int64
	StorageMaxBytes   int64
	StorageGBMonthUSD float64
	StorageMaxUpload  int64

	// Shared secret a rented worker must present. Generated per deploy if
	// unset, which is fine: workers are launched with the value in hand.
	RunnerToken string

	// Credits handed to an account the first time it runs anything here.
	// 1000 credits is $1 of compute — about 2.8 core-hours.
	WelcomeCredits int

	// Anonymous *server* runs are off by default. The browser runtime is free
	// and unlimited without an account; our machines are not, because an
	// unauthenticated compute endpoint is a free botnet with a nice API.
	AllowAnonymous   bool
	FreeMsPerDay     float64
	FreeTimeoutMs    int
	MaxTimeoutMs     int
	DefaultTimeoutMs int

	// Managed vector search. One credit is $0.001; fractional costs accrue
	// until a whole credit can move through the existing account ledger.
	VectorMaxDocuments     int
	VectorMaxDocumentBytes int
	VectorCPUQueryCredits  float64
	VectorGPUQueryCredits  float64
	VectorUpsertCredits    float64
	VectorConcurrency      int
	ReservedVectorCredits  int
	ReservedVectorSlots    int
}

func loadConfig() Config {
	_ = godotenv.Load()
	siteBaseURL := strings.TrimRight(env("SITE_BASE_URL", "https://mojojojo.cc"), "/")
	runnerToken := env("RUNNER_TOKEN", randomToken(24))
	return Config{
		Port:                 env("PORT", "4341"),
		Debug:                env("DEV", "false") == "true",
		SiteBaseURL:          siteBaseURL,
		RunnerURL:            strings.TrimRight(env("RUNNER_URL", "http://127.0.0.1:4342"), "/"),
		VectorURL:            strings.TrimRight(env("VECTOR_URL", "http://127.0.0.1:4344"), "/"),
		VectorToken:          env("VECTOR_WORKER_TOKEN", runnerToken),
		AgentURL:             strings.TrimRight(env("MJJ_AGENT_URL", "http://127.0.0.1:4343"), "/"),
		AgentToken:           env("MJJ_AGENT_TOKEN", ""),
		MaxAgentRuns:         envInt("MJJ_MAX_RUNS_PER_USER", 2),
		AgentTokensPerCredit: envFloat("MJJ_TOKENS_PER_CREDIT", 1000),
		DatabaseURL:          env("MOJOJOJO_DATABASE_URL", env("DATABASE_URL", "postgres:///mojojojo?host=/var/run/postgresql&sslmode=disable")),
		// The edge tier (wasmworker/). Empty means there isn't one, and every
		// placement decision quietly stops considering it.
		EdgeURL:                       env("EDGE_URL", ""),
		EdgeToken:                     env("EDGE_TOKEN", ""),
		LoginURL:                      siteBaseURL + "/auth/login",
		AccountURL:                    siteBaseURL + "/credits",
		SignupURL:                     siteBaseURL + "/auth/signup",
		TopUpURL:                      siteBaseURL + "/credits?buy=credits",
		StripeSecretKey:               env("STRIPE_SECRET_KEY", ""),
		StripePublishableKey:          env("STRIPE_PUBLISHABLE_KEY", ""),
		StripeWebhookSecret:           env("STRIPE_WEBHOOK_SECRET", ""),
		StripeCreditsPriceID:          env("STRIPE_CREDITS_PRICE_ID", ""),
		StripeDeveloperMonthlyPriceID: env("STRIPE_DEVELOPER_MONTHLY_PRICE_ID", ""),
		StripeDeveloperAnnualPriceID:  env("STRIPE_DEVELOPER_ANNUAL_PRICE_ID", ""),
		StripeDedicatedMonthlyPriceID: env("STRIPE_DEDICATED_MONTHLY_PRICE_ID", ""),
		StripeDedicatedAnnualPriceID:  env("STRIPE_DEDICATED_ANNUAL_PRICE_ID", ""),
		StripeAutomaticTax:            env("STRIPE_AUTOMATIC_TAX", "false") == "true",
		EmailDripEnabled:              env("EMAIL_DRIP_ENABLED", "false") == "true",
		EmailCampaignPath:             env("EMAIL_CAMPAIGN_PATH", "emails/drip_config.json"),
		EmailFromEmail:                env("SES_FROM_EMAIL", "lee.penkman@netwrck.com"),
		EmailFromName:                 env("SES_FROM_NAME", "Lee at mojojojo"),
		EmailReplyTo:                  env("SES_REPLY_TO", "lee.penkman@netwrck.com"),
		EmailPostalAddress:            env("EMAIL_POSTAL_ADDRESS", ""),
		EmailUnsubscribeSecret:        env("EMAIL_UNSUBSCRIBE_SECRET", ""),
		AWSSMTPUsername:               env("AWS_SMTP_USERNAME", env("AWS_SMTP_IAM_USERNAME", "")),
		AWSSMTPPassword:               env("AWS_SMTP_PASSWORD", ""),
		AWSRegion:                     env("AWS_REGION", "us-east-1"),
		R2Endpoint:                    r2Endpoint(),
		R2AccessKey:                   env("MOJOJOJO_OBJECT_ACCESS_KEY", env("CLOUDFLARE_R2_ACCESS_KEY_ID", "")),
		R2SecretKey:                   env("MOJOJOJO_OBJECT_SECRET_KEY", env("CLOUDFLARE_R2_SECRET_ACCESS_KEY", "")),
		R2Bucket:                      env("MOJOJOJO_OBJECT_BUCKET", env("MOJOJOJO_R2_BUCKET", "mojojojostatic")),
		R2Prefix:                      objectPrefix(env("MOJOJOJO_OBJECT_PREFIX", env("MOJOJOJO_R2_PREFIX", ""))),
		CDNBase:                       strings.TrimRight(env("MOJOJOJO_STATIC_BASE", env("MOJOJOJO_CDN_BASE", "https://mojojojostatic.mojojojo.com")), "/"),
		StorageSigningKey:             storageSigningKey(),
		StorageFreeBytes:              envInt64("MOJOJOJO_STORAGE_FREE_BYTES", 1<<30),
		StorageMaxBytes:               envInt64("MOJOJOJO_STORAGE_MAX_BYTES", 100<<30),
		StorageGBMonthUSD:             envFloat("MOJOJOJO_STORAGE_GB_MONTH_USD", 0.05),
		StorageMaxUpload:              envInt64("MOJOJOJO_STORAGE_MAX_UPLOAD", 64<<20),
		RunnerToken:                   runnerToken,
		WelcomeCredits:                envInt("WELCOME_CREDITS", 1000),
		AllowAnonymous:                env("ALLOW_ANONYMOUS", "false") == "true",
		MsPerCredit:                   envFloat("MS_PER_CREDIT", 10000),
		MsPerCreditGPU:                envFloat("MS_PER_CREDIT_GPU", 2000),
		FreeMsPerDay:                  envFloat("FREE_MS_PER_DAY", 30000),
		FreeTimeoutMs:                 envInt("FREE_TIMEOUT_MS", 5000),
		MaxTimeoutMs:                  envInt("MAX_TIMEOUT_MS", 300000),
		DefaultTimeoutMs:              envInt("DEFAULT_TIMEOUT_MS", 10000),
		VectorMaxDocuments:            envInt("VECTOR_MAX_DOCUMENTS", 250000),
		VectorMaxDocumentBytes:        envInt("VECTOR_MAX_DOCUMENT_BYTES", 32768),
		VectorCPUQueryCredits:         envFloat("VECTOR_CPU_QUERY_CREDITS", 0.5),
		VectorGPUQueryCredits:         envFloat("VECTOR_GPU_QUERY_CREDITS", 1.5),
		VectorUpsertCredits:           envFloat("VECTOR_UPSERT_CREDITS", 0.02),
		VectorConcurrency:             envInt("VECTOR_CONCURRENCY", 8),
		ReservedVectorCredits:         envInt("VECTOR_RESERVED_CREDITS_PER_HOUR", 1500),
		ReservedVectorSlots:           envInt("VECTOR_RESERVED_SLOTS", 1),
	}
}

// A production host should set a dedicated signing key. Existing installs
// already have a stable object-store secret, so deriving a domain-separated
// fallback from it avoids invalidating every outstanding 15-minute ticket on
// restart without adding another mandatory secret to bootstrap.
func storageSigningKey() string {
	if key := env("MOJOJOJO_STORAGE_SIGNING_KEY", ""); key != "" {
		return key
	}
	if secret := env("MOJOJOJO_OBJECT_SECRET_KEY", env("CLOUDFLARE_R2_SECRET_ACCESS_KEY", "")); secret != "" {
		sum := sha256.Sum256([]byte("mojojojo-upload-tickets-v1\x00" + secret))
		return base64.RawURLEncoding.EncodeToString(sum[:])
	}
	return randomToken(32)
}

// r2Endpoint derives the S3 endpoint from the account id when it is not set
// explicitly, which is how app.nz's environment actually stores it.
func r2Endpoint() string {
	if v := env("MOJOJOJO_OBJECT_ENDPOINT", ""); v != "" {
		return v
	}
	if v := env("R2_ENDPOINT", ""); v != "" {
		return v
	}
	if account := env("R2_ACCOUNT_ID", ""); account != "" {
		return "https://" + account + ".r2.cloudflarestorage.com"
	}
	return ""
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v, err := strconv.ParseInt(env(key, ""), 10, 64); err == nil {
		return v
	}
	return fallback
}

func objectPrefix(v string) string {
	v = strings.Trim(v, "/")
	if v == "" {
		return ""
	}
	return v + "/"
}

func envFloat(key string, fallback float64) float64 {
	if v, err := strconv.ParseFloat(env(key, ""), 64); err == nil {
		return v
	}
	return fallback
}

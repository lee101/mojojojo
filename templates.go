package main

import (
	"bytes"
	"html/template"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/valyala/fasthttp"
)

type templates struct{ t *template.Template }

func loadTemplates(cfg Config) (*templates, error) {
	t, err := template.New("").Funcs(template.FuncMap{
		"seconds": func(ms float64) string {
			return strings.TrimRight(strings.TrimRight(
				strings.TrimSpace(formatFloat(ms/1000, 2)), "0"), ".")
		},
		"ms": func(v float64) string { return formatFloat(v, 1) },
		// What a credit balance is worth, in the two units anyone actually
		// thinks in: dollars, and time on a machine.
		"dollars":   func(credits int) string { return "$" + formatFloat(float64(credits)*usdPerCredit, 2) },
		"creditsMs": func(credits int) float64 { return float64(credits) * cfg.MsPerCredit },
		"cpuHours": func(credits int) string {
			return formatFloat(float64(credits)*cfg.MsPerCredit/3600000, 1)
		},
		"gpuMinutes": func(credits int) string {
			return formatFloat(float64(credits)*cfg.MsPerCreditGPU/60000, 0)
		},
		"usd": func(v float64) string {
			if v >= 0.01 {
				return "$" + formatFloat(v, 2)
			}
			return "$" + strings.TrimRight(strings.TrimRight(formatFloat(v, 7), "0"), ".")
		},
		"usdM": func(v float64) string {
			return "$" + strings.TrimRight(strings.TrimRight(formatFloat(v*1e6, 3), "0"), ".")
		},
		"gib": func(b int64) string {
			return strings.TrimRight(strings.TrimRight(formatFloat(float64(b)/(1<<30), 2), "0"), ".")
		},
		"num": func(v float64) string { return strings.TrimRight(strings.TrimRight(formatFloat(v, 2), "0"), ".") },
		"pct": func(a, b float64) string {
			if b <= 0 {
				return "0"
			}
			return formatFloat(math.Min(100, a/b*100), 1)
		},
	}).ParseGlob("templates/*.html")
	if err != nil {
		return nil, err
	}
	// Research posts are one template each, under their own directory. They are
	// parsed by hand rather than with ParseGlob because ParseGlob names a
	// template after its *base* filename — so templates/research/docs.html
	// would silently replace the docs page. Naming them "research/<slug>.html"
	// keeps the route, the file and the template name identical.
	files, err := filepath.Glob("templates/research/*.html")
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		name := "research/" + filepath.Base(path)
		if _, err := t.New(name).Parse(string(body)); err != nil {
			return nil, err
		}
	}
	return &templates{t: t}, nil
}

// One mojojojo credit is a tenth of a cent.
const usdPerCredit = 0.001

func formatFloat(v float64, digits int) string {
	return strconv.FormatFloat(v, 'f', digits, 64)
}

type Meta struct {
	Title, Desc, Canonical, Image string
}

func (s *Server) meta(title, desc string) Meta {
	if desc == "" {
		desc = "Per-millisecond Python execution. Native-compiled hot loops via mojosub, real CPython fallback, free in your browser."
	}
	return Meta{
		Title:     title,
		Desc:      desc,
		Canonical: s.cfg.SiteBaseURL,
		Image:     s.cfg.SiteBaseURL + "/static/logo.webp",
	}
}

func (s *Server) metaAt(title, desc, path string) Meta {
	meta := s.meta(title, desc)
	meta.Canonical = strings.TrimRight(s.cfg.SiteBaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	return meta
}

// pageData fills in what every page needs whether or not its handler thought
// about it: the config the header and footer link to, and a credit balance.
// Credits is never allowed to be missing — a nil there is a template error on
// a page that only wanted to say hello.
func (s *Server) pageData(data map[string]any) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	data["Cfg"] = s.cfg
	data["AgentSourceURL"] = agentSourceURL
	data["AgentDocsURL"] = agentDocsURL
	data["AgentReleasesURL"] = agentReleasesURL
	data["AgentIssuesURL"] = agentIssuesURL
	data["AgentLicenseURL"] = agentLicenseURL
	data["AgentReleaseBase"] = agentReleaseDownloadBase
	if _, ok := data["Credits"]; !ok {
		credits := 0
		if u, _ := data["User"].(*User); u != nil {
			credits = u.Credits()
		}
		data["Credits"] = credits
	}
	return data
}

func (s *Server) render(ctx *fasthttp.RequestCtx, name string, data map[string]any) {
	data = s.pageData(data)
	var buf bytes.Buffer
	if err := s.tmpl.t.ExecuteTemplate(&buf, name, data); err != nil {
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		ctx.SetContentType("text/plain; charset=utf-8")
		ctx.WriteString("template error: " + err.Error())
		return
	}
	ctx.SetContentType("text/html; charset=utf-8")
	ctx.Write(buf.Bytes())
}

func (s *Server) handleHome(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromSession(ctx)
	data := map[string]any{
		"Meta":        s.meta("mojojojo — Python by the millisecond", ""),
		"User":        user,
		"Example":     exampleProgram,
		"MojoExample": mojoExampleProgram,
	}
	if user != nil {
		data["Credits"] = user.Credits()
	}
	s.render(ctx, "home.html", data)
}

func (s *Server) handleDocs(ctx *fasthttp.RequestCtx) {
	s.render(ctx, "docs.html", map[string]any{
		"Meta": s.meta("mojojojo docs — API, pricing, the browser runtime", ""),
		"User": s.auth.userFromSession(ctx),
	})
}

func (s *Server) handleAgentPage(ctx *fasthttp.RequestCtx) {
	s.render(ctx, "agent.html", map[string]any{
		"Meta": s.metaAt("mjj — the token-efficient mojojojo coding agent",
			"Install the mojojojo coding agent for Linux, macOS, or Windows, or try it in the browser.",
			"/agent"),
		"User": s.auth.userFromSession(ctx),
	})
}

func (s *Server) handleEditor(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromSession(ctx)
	data := map[string]any{
		"Meta":      s.meta("mojojojo editor — write Python, run it, see the cost", ""),
		"User":      user,
		"Example":   editorExample,
		"FullBleed": true,
	}
	if user != nil {
		data["Credits"] = user.Credits()
	}
	s.render(ctx, "editor.html", data)
}

func (s *Server) handleStudio(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromSession(ctx)
	data := map[string]any{
		"Meta": s.meta("mojojojo studio — build and host apps with Python or Mojo",
			"A browser IDE for static frontends and metered Python or Mojo serverless functions."),
		"User": user, "FullBleed": true,
	}
	if user != nil {
		data["Credits"] = user.Credits()
	}
	s.render(ctx, "studio.html", data)
}

func (s *Server) handleShowcasePage(ctx *fasthttp.RequestCtx, category string) {
	title, desc := "Apps built with Mojo", "Explore hosted applications built with Mojo, Python, and mojojojo serverless functions."
	if category == "tool" {
		title = "Tools built with Mojo"
		desc = "Useful converters and developer tools, with source you can open in mojojojo studio."
	}
	s.render(ctx, "showcase.html", map[string]any{
		"Meta": s.meta(title+" — mojojojo", desc), "User": s.auth.userFromSession(ctx),
		"Category": category, "Title": title, "Description": desc,
	})
}

// handlePricing2 is the human pricing page; /v1/pricing is the machine one.
func (s *Server) handlePricing2(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromSession(ctx)
	data := map[string]any{
		"Meta": s.meta("mojojojo pricing — $0.36 a core-hour, billed per millisecond", ""),
		"User": user,
	}
	if user != nil {
		data["Credits"] = user.Credits()
	}
	s.render(ctx, "pricing.html", data)
}

func (s *Server) handleCloudPage(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromSession(ctx)
	data := map[string]any{
		"Meta": s.meta("mojojojo cloud — deploy Mojo projects and static apps",
			"Deploy ZIP bundles, multi-file Mojo projects, and globally delivered assets."),
		"User": user,
	}
	if user != nil {
		data["Credits"] = user.Credits()
	}
	s.render(ctx, "cloud.html", data)
}

func (s *Server) handleDataPage(ctx *fasthttp.RequestCtx) {
	s.render(ctx, "data.html", map[string]any{
		"Meta": s.meta("mojojojo data — hosted JSON records with vector search",
			"A scale-to-zero managed document database backed by Postgres, with optional mojo-embed vector indexing."),
		"User": s.auth.userFromSession(ctx),
	})
}

func (s *Server) handleVectorPage(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromSession(ctx)
	data := map[string]any{
		"Meta": s.meta("mojojojo vector search — managed folded embeddings",
			"Create a vector project, index text with a static 512-dimensional model, and query it through one CPU/GPU API."),
		"User": user,
	}
	if user != nil {
		data["Credits"] = user.Credits()
	}
	s.render(ctx, "vector.html", data)
}

func (s *Server) handleExamplesPage(ctx *fasthttp.RequestCtx) {
	s.render(ctx, "examples.html", map[string]any{
		"Meta": s.meta("mojojojo examples — useful apps with one-click deploys",
			"Open-source QR, cron, phone, image, Python, and Mojo apps ready to customize and deploy."),
		"User": s.auth.userFromSession(ctx),
	})
}

func (s *Server) handleKeysPage(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromSession(ctx)
	data := map[string]any{"Meta": s.meta("mojojojo API keys", ""), "User": user}
	if user != nil {
		keys, err := s.auth.listKeys(user.ID)
		if err != nil {
			data["Error"] = err.Error()
		}
		data["Keys"] = keys
		data["Credits"] = user.Credits()
	}
	s.render(ctx, "keys.html", data)
}

// Post is one research write-up. Content lives in its own template under
// templates/research/, so a post is a file rather than a database — there are
// going to be a dozen of these, not a thousand, and a template gets the same
// syntax highlighting and the same contrast as the rest of the site for free.
type Post struct {
	Slug, Title, Blurb, Date string
	// Minutes is the reading estimate. Stated because these are long.
	Minutes int
	// Tags group them: "containment" and "speed" are the two threads.
	Tags []string
}

// posts is ordered newest first, which is the order the index renders.
var posts = []Post{
	{
		Slug:  "the-open-source-stack-behind-mojojojo",
		Title: "The open-source stack behind mojojojo",
		Blurb: "mojosub, the compiler, the coding agent, and the Mojo port factory " +
			"are separate projects on purpose. Here is what each one owns, what is " +
			"ready today, and where the hard gates sit.",
		Date: "2026-08-03", Minutes: 7,
		Tags: []string{"open-source", "tooling"},
	},
	{
		Slug:  "notebooks-should-show-the-diff",
		Title: "A notebook agent should show the diff",
		Blurb: "A preview of mojojojo Notebooks: reactive Python and Mojo cells, " +
			"reviewed AI edits, immutable revisions, and one execution and billing path.",
		Date: "2026-08-03", Minutes: 6,
		Tags: []string{"preview", "notebooks"},
	},
	{
		Slug:  "context-is-the-agent-budget",
		Title: "Context is the coding agent's real budget",
		Blurb: "Why mjj searches first, returns grounded line ranges, loads skills only " +
			"when needed, and keeps its tool surface deliberately small.",
		Date: "2026-08-03", Minutes: 6,
		Tags: []string{"open-source", "agent"},
	},
	{
		Slug:  "from-studio-to-a-hosted-app",
		Title: "From Studio to a hosted app",
		Blurb: "The shortest route from a browser project to a published static app " +
			"with metered Python or Mojo functions, version history, and no server to keep idle.",
		Date: "2026-08-03", Minutes: 5,
		Tags: []string{"product", "studio"},
	},
	{
		Slug:  "no-compiler-in-the-sandbox",
		Title: "There is no compiler in the sandbox",
		Blurb: "How a multi-tenant service shares a compile cache without letting " +
			"one tenant put code in another tenant's process — and the four " +
			"isolation bugs we shipped and fixed on the way there.",
		Date: "2026-07-28", Minutes: 11,
		Tags: []string{"containment"},
	},
	{
		Slug:  "how-early-do-we-know",
		Title: "How early do we know it has to be CPython?",
		Blurb: "Most submitted Python cannot be compiled, and finding that out " +
			"late costs five seconds of somebody else's compiler. Measuring the " +
			"pipeline against a labelled corpus, and the static filter that came out of it.",
		Date: "2026-07-30", Minutes: 9,
		Tags: []string{"speed"},
	},
	{
		Slug:  "converting-python-to-mojo",
		Title: "Converting Python to Mojo, with an agent for the rest",
		Blurb: "A transpiler that follows rules can be trusted and refuses most " +
			"code; an agent converts anything and can be trusted about nothing. " +
			"So the agent's output goes through the same gate — compile, compare " +
			"against CPython, measure. Plus what fuzzing the transpiler found.",
		Date: "2026-07-30", Minutes: 10,
		Tags: []string{"speed", "tooling"},
	},
	{
		Slug:  "the-copy-nobody-needed",
		Title: "The copy nobody needed",
		Blurb: "Compiled Mojo was slower than CPython on a million-element list. " +
			"More than half of that was a copy back the kernel had provably not " +
			"earned — plus three name-collision bugs found by reading compiler stderr.",
		Date: "2026-07-30", Minutes: 8,
		Tags: []string{"speed"},
	},
}

func postBySlug(slug string) *Post {
	for i := range posts {
		if posts[i].Slug == slug {
			return &posts[i]
		}
	}
	return nil
}

func (s *Server) handleResearchIndex(ctx *fasthttp.RequestCtx) {
	s.render(ctx, "research.html", map[string]any{
		"Meta": s.meta("mojojojo research — containment and speed",
			"How mojojojo runs untrusted Python fast: the compile trust boundary, "+
				"the fallback heuristics, and the measurements behind both."),
		"User":  s.auth.userFromSession(ctx),
		"Posts": posts,
	})
}

func (s *Server) handleResearchPost(ctx *fasthttp.RequestCtx, slug string) {
	post := postBySlug(slug)
	if post == nil {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		s.render(ctx, "404.html", map[string]any{
			"Meta": s.meta("Not found", ""),
			"User": s.auth.userFromSession(ctx),
		})
		return
	}
	s.render(ctx, "research/"+slug+".html", map[string]any{
		"Meta": s.meta(post.Title+" — mojojojo research", post.Blurb),
		"User": s.auth.userFromSession(ctx),
		"Post": post,
	})
}

// handleCreditsPage is where money is managed: what the balance is, what it is
// worth in machine time, where it went, and how to get more. Signed out it is
// the sign-up page — a visitor who has decided to pay us should never have to
// go hunting for the button.
func (s *Server) handleCreditsPage(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://js.stripe.com https://static.cloudflareinsights.com; style-src 'self' 'unsafe-inline'; frame-src https://js.stripe.com https://hooks.stripe.com https://checkout.stripe.com; connect-src 'self' https://api.stripe.com https://r.stripe.com https://m.stripe.network https://cloudflareinsights.com; img-src 'self' data: https://*.stripe.com; font-src 'self' data:")
	user := s.auth.userFromSession(ctx)
	// Every Add credits link uses this intent URL. A visitor signs up first and
	// returns to the same intent; an existing customer reaches the embedded
	// Checkout page directly.
	if user == nil && string(ctx.QueryArgs().Peek("buy")) == "credits" {
		redirectLocal(ctx, "/auth/signup?next=%2Fcredits%3Fbuy%3Dcredits")
		return
	}
	data := map[string]any{
		"Meta": s.meta("mojojojo credits — balance, spending and top-ups",
			"Manage mojojojo credits and spend them by the millisecond here. "+
				"Add credits to start, with a $1 bonus on your first top-up."),
		"User":       user,
		"StripePage": true,
	}
	if s.stripe != nil {
		offers := s.stripe.offers()
		data["StripeConfigured"] = offers["credits"].PriceID != ""
		data["DeveloperAvailable"] = offers["developer_monthly"].PriceID != "" && offers["developer_annual"].PriceID != ""
		data["DedicatedAvailable"] = offers["dedicated_monthly"].PriceID != "" && offers["dedicated_annual"].PriceID != ""
		data["StripeTaxEnabled"] = s.cfg.StripeAutomaticTax
	}
	if user != nil {
		data["Credits"] = user.Credits()
		data["OwedMs"] = round3(s.bill.owed(user.ID))
		data["TotalMs"] = round3(s.bill.totalMs(user.ID))
		data["Granted"] = s.bill.granted(user.ID)
		ledger, err := s.auth.ledger(user.ID, 25)
		if err != nil {
			log.Printf("credits: ledger %s: %v", user.ID, err)
		}
		data["Ledger"] = ledger
		runs, _ := s.bill.recent(user.ID, 10)
		data["Runs"] = runs
		spent, earned := s.auth.ledgerTotals(user.ID)
		data["Spent"], data["Earned"] = spent, earned
	}
	s.render(ctx, "credits.html", data)
}

func (s *Server) handleUsagePage(ctx *fasthttp.RequestCtx) {
	user := s.auth.userFromSession(ctx)
	data := map[string]any{"Meta": s.meta("mojojojo usage", ""), "User": user}
	if user != nil {
		rows, _ := s.bill.recent(user.ID, 50)
		data["Runs"] = rows
		data["Credits"] = user.Credits()
		data["OwedMs"] = round3(s.bill.owed(user.ID))
		data["TotalMs"] = round3(s.bill.totalMs(user.ID))
	}
	s.render(ctx, "usage.html", data)
}

// serveClientJS serves the browser runtime with a long cache and CORS open, so
// a page anywhere can script-src it.
func (s *Server) serveClientJS(ctx *fasthttp.RequestCtx) {
	s.serveJS(ctx, "web/mojojojo.js")
}

// serveJS hands out one of our own ES modules. Same headers as the runtime:
// the highlighter is embeddable too, and it is the same kind of file.
func (s *Server) serveJS(ctx *fasthttp.RequestCtx, path string) {
	body, err := os.ReadFile(path)
	if err != nil {
		sendError(ctx, 500, "client unavailable")
		return
	}
	ctx.SetContentType("application/javascript; charset=utf-8")
	ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")
	ctx.Response.Header.Set("Cache-Control", "public, max-age=300")
	ctx.Write(body)
}

const editorExample = `# Imports are installed for you — no packages field, no image to build.
import numpy as np

def rolling_sharpe(returns, window: int) -> float:
    best = -1e9
    for i in range(window, len(returns)):
        total = 0.0
        sq = 0.0
        for j in range(i - window, i):
            total += returns[j]
            sq += returns[j] * returns[j]
        mean = total / window
        var = sq / window - mean * mean
        if var > 1e-12:
            s = mean / (var ** 0.5)
            if s > best:
                best = s
    return best

rng = np.random.default_rng(7)
returns = rng.normal(0.0004, 0.01, 60_000)
print("best rolling sharpe:", round(rolling_sharpe(returns, 64), 4))
`

const exampleProgram = `# Numeric loops are transpiled to Mojo, compiled once, and cached by
# content hash. Call a kernel repeatedly and the later calls go native —
# and the compile is on us, not on your bill.
def monte_carlo_pi(n: int) -> float:
    inside = 0
    x = 12345
    for i in range(n):
        x = (1103515245 * x + 12345) % 2147483648
        a = x / 2147483648.0
        x = (1103515245 * x + 12345) % 2147483648
        b = x / 2147483648.0
        if a * a + b * b <= 1.0:
            inside += 1
    return 4.0 * inside / n

total = 0.0
for batch in range(8):
    total += monte_carlo_pi(400_000)
print(total / 8)
`

// mojoExampleProgram is the same estimate as exampleProgram, written in Mojo
// instead of Python, so the two panels on the home page are comparable: same
// arithmetic, same iteration count, one interpreted-then-accelerated and one
// compiled outright. It is deliberately plain Mojo — no imports, no traits —
// because its job is to be read by somebody who has never written any.
const mojoExampleProgram = `# Written in Mojo, compiled on our machines, run with no interpreter in the
# way. The build is on us and cached by content hash; you pay for the run.
def monte_carlo_pi(n: Int) -> Float64:
    var inside: Int = 0
    var x: Int = 12345
    for _ in range(n):
        x = (1103515245 * x + 12345) % 2147483648
        var a = Float64(x) / 2147483648.0
        x = (1103515245 * x + 12345) % 2147483648
        var b = Float64(x) / 2147483648.0
        if a * a + b * b <= 1.0:
            inside += 1
    return 4.0 * Float64(inside) / Float64(n)

def main():
    var total: Float64 = 0.0
    for _ in range(8):
        total += monte_carlo_pi(400000)
    print(total / 8)
`

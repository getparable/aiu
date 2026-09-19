package core

import (
	_ "embed"
	"html"
	"regexp"
	"strings"
)

// The logos are Simple Icons (CC0 artwork; the marks belong to Anthropic and OpenAI).
// AIU.app ships the same files.
var (
	//go:embed assets/claude.svg
	claudeLogo string
	//go:embed assets/openai.svg
	openaiLogo string
)

var svgTitle = regexp.MustCompile(`<title>.*?</title>`)

// inlineLogo returns the provider's mark as inline SVG that takes the page's text colour.
// Inline markup, not an <img>, because the page's CSP forbids loading anything.
func inlineLogo(p Provider) string {
	svg := claudeLogo
	if p == Codex {
		svg = openaiLogo
	}
	svg = svgTitle.ReplaceAllString(svg, "")
	return strings.Replace(svg, "<svg ", `<svg class="logo" aria-hidden="true" fill="currentColor" `, 1)
}

type pageKind int

const (
	pageSuccess pageKind = iota
	pageFailure
	pageNotice
)

// callbackPage renders the browser page a sign-in lands on. Styles are inline (the
// CSP allows inline styles only), monochrome, and follow the system light/dark setting.
func callbackPage(p Provider, kind pageKind, title, detail string) string {
	mark := `<svg class="badge-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12.5l4.2 4.2L19 7" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/></svg>`
	if kind != pageSuccess {
		mark = `<svg class="badge-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 7v6.5M12 17h.01" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"/></svg>`
	}
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>` + html.EscapeString(title) + ` · aiu</title>
<style>
  :root {
    --bg: #f4f4f5; --card: rgba(255,255,255,.72); --edge: rgba(0,0,0,.08);
    --text: #111113; --muted: #6b6b73; --faint: rgba(0,0,0,.06);
    --glow-a: rgba(0,0,0,.05); --glow-b: rgba(0,0,0,.03);
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #0c0c0e; --card: rgba(28,28,32,.66); --edge: rgba(255,255,255,.09);
      --text: #f4f4f5; --muted: #9a9aa3; --faint: rgba(255,255,255,.07);
      --glow-a: rgba(255,255,255,.07); --glow-b: rgba(255,255,255,.035);
    }
  }
  * { box-sizing: border-box; }
  html, body { height: 100%; margin: 0; }
  body {
    display: grid; place-items: center; padding: 24px;
    background:
      radial-gradient(900px 600px at 20% 10%, var(--glow-a), transparent 60%),
      radial-gradient(700px 500px at 85% 90%, var(--glow-b), transparent 60%),
      var(--bg);
    color: var(--text);
    font: 15px/1.5 -apple-system, BlinkMacSystemFont, "SF Pro Text", "Inter", "Segoe UI", system-ui, sans-serif;
    -webkit-font-smoothing: antialiased;
  }
  .card {
    width: min(420px, 100%); padding: 40px 36px 32px; text-align: center;
    background: var(--card); border: 1px solid var(--edge); border-radius: 28px;
    backdrop-filter: blur(40px) saturate(1.4); -webkit-backdrop-filter: blur(40px) saturate(1.4);
    box-shadow: 0 1px 0 rgba(255,255,255,.06) inset, 0 30px 80px -30px rgba(0,0,0,.35);
    animation: rise .5s cubic-bezier(.2,.8,.2,1) both;
  }
  .brand { position: relative; width: 72px; height: 72px; margin: 0 auto 22px; }
  .logo-wrap {
    width: 72px; height: 72px; border-radius: 22px; display: grid; place-items: center;
    background: var(--faint); border: 1px solid var(--edge);
  }
  .logo { width: 34px; height: 34px; }
  .badge {
    position: absolute; right: -6px; bottom: -6px; width: 30px; height: 30px; border-radius: 50%;
    display: grid; place-items: center; background: var(--text); color: var(--bg);
    box-shadow: 0 0 0 4px var(--bg);
  }
  .badge-icon { width: 17px; height: 17px; }
  h1 { margin: 0 0 8px; font-size: 22px; font-weight: 650; letter-spacing: -.01em; }
  p { margin: 0; color: var(--muted); }
  .hint {
    margin-top: 26px; padding-top: 18px; border-top: 1px solid var(--edge);
    font-size: 12.5px; color: var(--muted); letter-spacing: .01em;
  }
  kbd {
    font: 600 11.5px/1 ui-monospace, "SF Mono", Menlo, monospace; padding: 3px 6px;
    border-radius: 6px; border: 1px solid var(--edge); background: var(--faint);
  }
  @keyframes rise { from { transform: translateY(10px) scale(.985); } to { transform: none; } }
  @media (prefers-reduced-motion: reduce) { .card { animation: none; } }
</style>
</head>
<body>
  <main class="card">
    <div class="brand">
      <div class="logo-wrap">` + inlineLogo(p) + `</div>
      <div class="badge">` + mark + `</div>
    </div>
    <h1>` + html.EscapeString(title) + `</h1>
    <p>` + html.EscapeString(detail) + `</p>
    <div class="hint">You can close this tab <kbd>⌘W</kbd></div>
  </main>
</body>
</html>`
}

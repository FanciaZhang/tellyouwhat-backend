package appledelivery

import (
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

func validOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || !hostnamePattern.MatchString(u.Hostname()) || u.Host != u.Hostname() || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("installation origin must be a plain HTTPS origin")
	}
	return nil
}

type Handler struct {
	store  *Store
	origin string
	host   string
	now    func() time.Time
	slots  chan struct{}
}

func NewHandler(s *Store, origin string) (*Handler, error) {
	if err := validOrigin(origin); err != nil {
		return nil, err
	}
	u, _ := url.Parse(origin)
	return &Handler{store: s, origin: origin, host: u.Host, now: time.Now, slots: make(chan struct{}, 4)}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", 405)
		return
	}
	if r.URL.Path == "/readyz" && r.URL.RawQuery == "" {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		}
		return
	}
	if (!strings.EqualFold(r.Host, h.host) && !strings.EqualFold(r.Host, h.host+":443")) || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "d" || !tokenPattern.MatchString(parts[2]) ||
		(parts[3] != "" && parts[3] != "manifest.plist" && parts[3] != "app.ipa") {
		http.NotFound(w, r)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Try again shortly", 503)
		return
	}
	key := sha256.Sum256([]byte(parts[2]))
	g, err := h.store.readGrant(fmt.Sprintf("%x.json", key))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if h.now().Before(g.Created) {
		http.NotFound(w, r)
		return
	}
	if !h.now().Before(g.Expires) {
		http.Error(w, "Installation link expired", http.StatusGone)
		return
	}
	if parts[3] == "app.ipa" {
		if err := h.store.verifyPackage(g.Candidate); err != nil {
			http.Error(w, "Package unavailable", 503)
			return
		}
		if !h.now().Before(g.Expires) {
			http.Error(w, "Installation link expired", 410)
			return
		}
		f, err := h.store.regular("packages/" + g.Candidate.Digest + ".ipa")
		if err != nil {
			http.Error(w, "Package unavailable", 503)
			return
		}
		defer f.Close()
		// Stop an already-started transfer when its capability expires.
		deadline := h.now().Add(15 * time.Minute)
		if g.Expires.Before(deadline) {
			deadline = g.Expires
		}
		if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil {
			http.Error(w, "Transfer unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="development.ipa"`)
		w.Header().Set("ETag", `"`+g.Candidate.Digest+`"`)
		http.ServeContent(w, r, "development.ipa", time.Time{}, f)
		return
	}
	base := h.origin + "/d/" + parts[2] + "/"
	if parts[3] == "manifest.plist" {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		if r.Method == http.MethodGet {
			_, _ = w.Write(manifest(g.Candidate, base+"app.ipa"))
		}
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	_ = installationPage.Execute(w, struct {
		Title, Version, Build, Expires string
		Install                        template.URL
	}{
		appNames[g.Candidate.BundleID], g.Candidate.Version, g.Candidate.Build,
		g.Expires.In(time.FixedZone("UTC+8", 8*3600)).Format("2006-01-02 15:04（北京时间）"),
		template.URL("itms-services://?action=download-manifest&url=" + url.QueryEscape(base+"manifest.plist")),
	})
}

func xmlText(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}
func manifest(c Candidate, ipaURL string) []byte {
	return []byte(xml.Header + `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>items</key><array><dict><key>assets</key><array><dict><key>kind</key><string>software-package</string><key>url</key><string>` +
		xmlText(ipaURL) + `</string></dict></array><key>metadata</key><dict><key>bundle-identifier</key><string>` + xmlText(c.BundleID) + `</string><key>bundle-version</key><string>` + xmlText(c.Build) + `</string><key>kind</key><string>software</string><key>title</key><string>` + xmlText(appNames[c.BundleID]) + `</string></dict></dict></array></dict></plist>`)
}

var installationPage = template.Must(template.New("install").Parse(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}}</title><style>body{font:17px system-ui;max-width:32rem;margin:12vh auto;padding:24px;color:#18212b;background:#f6f7fa}h1{font-size:28px}p{line-height:1.6;color:#526070}a{display:block;margin:32px 0;padding:16px;text-align:center;background:#1468d5;color:white;border-radius:12px;text-decoration:none}small{line-height:1.6;color:#526070}</style><main><h1>{{.Title}}</h1><p>版本 {{.Version}} · 构建 {{.Build}}</p><a href="{{.Install}}">安装开发版</a><p>请使用 iPhone 上的 Safari 打开，仅限已登记设备。</p><small>链接有效至 {{.Expires}}</small></main></html>`))

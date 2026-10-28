// Package fetch reads a web page's title, description and icon for a new
// bookmark. Users choose the URLs, so by default it refuses to connect to
// loopback, private and other non-public addresses (checked on every
// connection, so redirects and DNS tricks can't get around it).
package fetch

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"golang.org/x/net/publicsuffix"
)

const (
	timeout      = 10 * time.Second
	maxPage      = 1 << 20   // only the <head> matters; stop reading after 1 MiB
	maxIcon      = 100 << 10 // 100 KiB
	maxRedirects = 5
	userAgent    = "Mozilla/5.0 (compatible; Anchor/1.0; bookmark details)"

	maxTitle       = 500
	maxDescription = 2000
)

// ErrBlocked means the address is not one Anchor may connect to.
var ErrBlocked = errors.New("address not allowed")

type Client struct {
	http *http.Client
}

// New returns a client. allowPrivate also lets it reach loopback, private
// and link-local addresses (for bookmarks of services on your own network).
func New(allowPrivate bool) *Client { return NewWithTimeout(allowPrivate, timeout) }

// NewWithTimeout is New with another limit on each request (tests use a
// short one).
func NewWithTimeout(allowPrivate bool, timeout time.Duration) *Client {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			if !allowPrivate && !public(ip) {
				return fmt.Errorf("%w: %s", ErrBlocked, ip)
			}
			return nil
		},
	}
	return &Client{http: &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil, // a proxy would make the address check meaningless
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to a non-http URL")
			}
			return nil
		},
	}}
}

// IPv4 ranges that aren't public yet pass IsGlobalUnicast: carrier NAT (and
// Tailscale), IETF protocol assignments, benchmarking (RFC 2544), and the
// reserved 240/4.
var notPublic4 = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// IPv6 addresses that carry an IPv4 one, and reach it: NAT64 (RFC 6052),
// IPv4-compatible and 6to4. Local-use NAT64 and Teredo can't be read back
// reliably, so they aren't public.
var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	ipv4Compat = netip.MustParsePrefix("::/96")
	sixToFour  = netip.MustParsePrefix("2002::/16")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	teredo     = netip.MustParsePrefix("2001::/32")
)

func public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is6() {
		b := ip.As16()
		switch {
		case nat64Local.Contains(ip), teredo.Contains(ip):
			return false
		case nat64.Contains(ip), ipv4Compat.Contains(ip):
			return public(netip.AddrFrom4([4]byte(b[12:])))
		case sixToFour.Contains(ip):
			return public(netip.AddrFrom4([4]byte(b[2:6])))
		}
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate() &&
		!slices.ContainsFunc(notPublic4, func(p netip.Prefix) bool { return p.Contains(ip) }) &&
		!(ip.Is4() && ip.As4()[0] == 0)
}

// Meta is what a page says about itself.
type Meta struct {
	Title       string
	Description string
	Icons       []string // absolute icon URLs the page links, best first
	FinalURL    string   // after redirects
}

// Page fetches rawURL and reads its metadata. A response that isn't HTML
// (a PDF, an image) gives empty Meta and no error.
func (c *Client) Page(ctx context.Context, rawURL string) (Meta, error) {
	res, err := c.get(ctx, rawURL, "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5")
	if err != nil {
		return Meta{}, err
	}
	defer res.Body.Close()
	m := Meta{FinalURL: res.Request.URL.String()}
	if res.StatusCode != http.StatusOK {
		return m, fmt.Errorf("status %d", res.StatusCode)
	}
	ct := res.Header.Get("Content-Type")
	if !strings.Contains(ct, "html") {
		return m, nil
	}
	body, err := charset.NewReader(io.LimitReader(res.Body, maxPage), ct)
	if err != nil {
		body = io.LimitReader(res.Body, maxPage)
	}
	parseHead(body, res.Request.URL, &m)
	return m, nil
}

// parseHead reads <title>, description meta tags and icon links, stopping
// at <body>. og: values are only used when the plain ones are missing.
func parseHead(r io.Reader, base *url.URL, m *Meta) {
	var ogTitle, ogDesc, title string
	var icons, appleIcons []string
	inTitle := false
	z := html.NewTokenizer(r)
loop:
	for {
		switch z.Next() {
		case html.ErrorToken:
			break loop
		case html.TextToken:
			if inTitle {
				title += string(z.Text())
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "title":
				inTitle = false
			case "head":
				break loop
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				attrs[string(k)] = string(v)
			}
			switch string(name) {
			case "body":
				break loop
			case "title":
				inTitle = title == ""
			case "meta":
				key := strings.ToLower(cmpOr(attrs["property"], attrs["name"]))
				switch key {
				case "description":
					m.Description = cmpOr(m.Description, attrs["content"])
				case "og:title":
					ogTitle = cmpOr(ogTitle, attrs["content"])
				case "og:description":
					ogDesc = cmpOr(ogDesc, attrs["content"])
				}
			case "link":
				rels := strings.Fields(strings.ToLower(attrs["rel"]))
				href := resolve(base, attrs["href"])
				if href == "" || strings.Contains(attrs["type"], "svg") || strings.HasSuffix(strings.ToLower(attrs["href"]), ".svg") {
					continue
				}
				for _, rel := range rels {
					switch rel {
					case "icon":
						icons = append(icons, href)
					case "apple-touch-icon":
						appleIcons = append(appleIcons, href)
					}
				}
			}
		}
	}
	m.Title = clean(cmpOr(title, ogTitle), maxTitle)
	m.Description = clean(cmpOr(m.Description, ogDesc), maxDescription)
	m.Icons = append(icons, appleIcons...)
}

// Icon downloads an icon, accepting only common raster formats (sniffed, not
// trusted from the header). SVG is refused: it can carry scripts. Redirects
// are followed anywhere (mail.google.com/favicon.ico lives on gstatic.com):
// whatever comes back must be an image.
func (c *Client) Icon(ctx context.Context, rawURL string) (mime string, data []byte, err error) {
	res, err := c.get(ctx, rawURL, "image/*")
	if err != nil {
		return "", nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("status %d", res.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(res.Body, maxIcon+1))
	if err != nil {
		return "", nil, err
	}
	if len(data) > maxIcon {
		return "", nil, errors.New("icon too large")
	}
	switch mime = http.DetectContentType(data); mime {
	case "image/png", "image/gif", "image/jpeg", "image/webp", "image/x-icon", "image/vnd.microsoft.icon":
	default:
		// DetectContentType doesn't know every ICO variant.
		if !bytes.HasPrefix(data, []byte{0, 0, 1, 0}) {
			return "", nil, fmt.Errorf("not an image (%s)", mime)
		}
		mime = "image/x-icon"
	}
	if mime == "image/png" && blankPNG(data) || mime == "image/x-icon" && blankICO(data) {
		return "", nil, errors.New("blank icon")
	}
	return mime, data, nil
}

// The blank checks below look for an icon with no visible pixel, like the
// all-transparent favicon.ico of online.beobank.be. Only PNGs and ICOs of
// PNGs or 32-bit bitmaps are checked; anything else, or anything that doesn't
// parse, counts as visible.

// maxBlankSide is the largest image the blank checks decode. A small PNG can
// declare a huge image (48 KB for 20000×20000 px, 381 MB to decode); real
// icons are at most 256 px.
const maxBlankSide = 1024

// maxBlankImages is the most images in an ICO file the blank check decodes.
// Real icons hold a handful; a crafted one can point thousands of entries at
// one large image and take minutes to check.
const maxBlankImages = 16

// blankICO reports whether every image in an ICO file is blank.
func blankICO(data []byte) bool {
	if len(data) < 6 {
		return false
	}
	n := int(binary.LittleEndian.Uint16(data[4:]))
	if n == 0 || n > maxBlankImages || len(data) < 6+16*n {
		return false
	}
	for i := range n {
		e := data[6+16*i:]
		size, off := uint64(binary.LittleEndian.Uint32(e[8:])), uint64(binary.LittleEndian.Uint32(e[12:]))
		if off+size > uint64(len(data)) {
			return false
		}
		if img := data[off : off+size]; !(blankPNG(img) || blankBitmap(img)) {
			return false
		}
	}
	return true
}

// blankPNG reports whether data is a PNG whose pixels are all transparent.
func blankPNG(data []byte) bool {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width > maxBlankSide || cfg.Height > maxBlankSide {
		return false
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return false
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0 {
				return false
			}
		}
	}
	return true
}

// blankBitmap reports whether data is an ICO entry's 32-bit bitmap whose
// pixels are all zero: transparent black.
func blankBitmap(data []byte) bool {
	if len(data) < 40 || binary.LittleEndian.Uint32(data) != 40 || binary.LittleEndian.Uint16(data[14:]) != 32 {
		return false
	}
	w, h := int(int32(binary.LittleEndian.Uint32(data[4:]))), int(int32(binary.LittleEndian.Uint32(data[8:])))/2 // h counts the mask too
	if w <= 0 || h <= 0 || w > maxBlankSide || h > maxBlankSide || 40+w*h*4 > len(data) {
		return false
	}
	return !slices.ContainsFunc(data[40:40+w*h*4], func(c byte) bool { return c != 0 })
}

func (c *Client) get(ctx context.Context, rawURL, accept string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("not an http(s) URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", accept)
	return c.http.Do(req)
}

func resolve(base *url.URL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "data:") {
		return ""
	}
	u, err := base.Parse(href)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.String()
}

// clean collapses whitespace and cuts s to at most n runes.
func clean(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n-1]) + "…"
	}
	return s
}

func cmpOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// SameHost reports whether two URLs are on the same host, ignoring a leading
// "www.". A page that redirects elsewhere, even within its site (Keep to
// accounts.google.com, AD to a consent wall), isn't describing itself.
func SameHost(a, b string) bool {
	ha, hb := host(a), host(b)
	return ha != "" && ha == hb
}

// ToSiteHome reports whether to is the main host of from's site, as when
// online.beobank.be sends you to www.beobank.be. That page isn't the one
// asked for, but the site's icon fits it. A sign-in host is not the main one
// (keep.google.com to accounts.google.com).
func ToSiteHome(from, to string) bool {
	h := host(to)
	return h != "" && h == RegistrableDomain(host(from))
}

func host(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// RegistrableDomain is host's site: cronos.sharepoint.com → sharepoint.com,
// www.bbc.co.uk → bbc.co.uk. IP addresses and unknown hosts stay as they are.
func RegistrableDomain(host string) string {
	host = strings.ToLower(host)
	if _, err := netip.ParseAddr(host); err == nil {
		return host
	}
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}

// ServiceIcon is the URL of domain's icon at a public icon service, or ""
// for an unknown service.
func ServiceIcon(service, domain string) string {
	switch service {
	case "duckduckgo":
		return "https://icons.duckduckgo.com/ip3/" + url.PathEscape(domain) + ".ico"
	case "google":
		return "https://www.google.com/s2/favicons?sz=64&domain=" + url.QueryEscape(domain)
	}
	return ""
}

// DefaultIcon is where a site's icon lives when the page doesn't say.
func DefaultIcon(pageURL string) string {
	u, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/favicon.ico"
}

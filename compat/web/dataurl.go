package web

// dataurl.go: fetch() of a data: URL.
//
// A data URL carries its own body, so there is no request to make and no
// permission to check — which is also why it must be handled before the HTTP
// path rather than inside it. Without this, `fetch("data:…")` failed at the
// transport and the whole WPT fetch/data-urls directory scored 2 of 154.

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// defaultDataMIME is what a data URL means when it names no type, per the
// WHATWG data-url processor.
const defaultDataMIME = "text/plain;charset=US-ASCII"

// parseDataURL splits a data: URL into its MIME type and decoded body.
func parseDataURL(raw string) (mime string, body []byte, err error) {
	rest, ok := cutSchemePrefix(raw, "data:")
	if !ok {
		return "", nil, fmt.Errorf("not a data URL")
	}
	// The FIRST comma separates the metadata from the data; a comma inside the
	// data is content, not a separator.
	head, data, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, fmt.Errorf("data URL has no comma")
	}
	head = strings.TrimSpace(head)
	isBase64 := false
	// ";base64" must be the LAST parameter, matched case-insensitively, and its
	// trailing whitespace is ignored.
	if i := strings.LastIndex(strings.ToLower(head), ";base64"); i >= 0 &&
		strings.TrimSpace(head[i+len(";base64"):]) == "" {
		isBase64 = true
		head = head[:i]
	}
	// A type that begins with ";" is a parameter list with no type, and gets
	// "text/plain" — and ONLY that. The charset is not a default: it belongs to
	// the fallback below, for a type that does not parse at all. Prepending the
	// whole default here gave "text/plain;charset=US-ASCII;charset=x" for
	// ";charset=x", where the caller's charset is the one that counts.
	if strings.HasPrefix(head, ";") {
		head = "text/plain" + head
	}
	// The Content-Type is the PARSED type serialized back, not the raw text:
	// that is what lowercases it, normalizes the whitespace around parameters
	// and drops malformed ones. A type that does not parse falls back to the
	// default rather than being passed through.
	if m, ok := parseMIMEType(head); ok {
		head = m.String()
	} else {
		head = defaultDataMIME
	}

	// The data segment is percent-encoded regardless of base64.
	decoded, uerr := url.PathUnescape(data)
	if uerr != nil {
		// A stray "%" is data, not an error, in a data URL.
		decoded = data
	}
	if !isBase64 {
		return head, []byte(decoded), nil
	}
	// Forgiving base64: whitespace is stripped, padding is optional.
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '\f':
			return -1
		}
		return r
	}, decoded)
	if n := len(cleaned) % 4; n == 1 {
		return "", nil, fmt.Errorf("invalid base64 in data URL")
	} else if n != 0 {
		cleaned += strings.Repeat("=", 4-n)
	}
	out, berr := base64.StdEncoding.DecodeString(cleaned)
	if berr != nil {
		// Accept the URL-safe alphabet too, as browsers do.
		out, berr = base64.URLEncoding.DecodeString(cleaned)
		if berr != nil {
			return "", nil, fmt.Errorf("invalid base64 in data URL")
		}
	}
	return head, out, nil
}

func cutSchemePrefix(raw, scheme string) (string, bool) {
	if len(raw) < len(scheme) || !strings.EqualFold(raw[:len(scheme)], scheme) {
		return "", false
	}
	return raw[len(scheme):], true
}

// dataResponse turns a data: URL into the response fetch resolves with: always
// 200 OK, the URL's own media type, and its decoded bytes.
func dataResponse(raw, method string) (*http.Response, error) {
	mime, body, err := parseDataURL(raw)
	if err != nil {
		return nil, err
	}
	u, uerr := url.Parse(raw)
	if uerr != nil {
		return nil, fmt.Errorf("invalid data URL")
	}
	h := http.Header{}
	h.Set("Content-Type", mime)
	// A HEAD gets the headers and no body, as it would over HTTP.
	rc := io.NopCloser(strings.NewReader(string(body)))
	length := int64(len(body))
	if strings.EqualFold(method, "HEAD") {
		rc = io.NopCloser(strings.NewReader(""))
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		Header:        h,
		Body:          rc,
		ContentLength: length,
		Request:       &http.Request{Method: method, URL: u},
	}, nil
}

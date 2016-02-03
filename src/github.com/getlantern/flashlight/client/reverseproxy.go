package client

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/getlantern/detour"
	"github.com/getlantern/flashlight/proxy"
	"github.com/getlantern/flashlight/status"
)

// newReverseProxy creates a reverse proxy that uses the client's balancer to
// dial out.
func (client *Client) newReverseProxy() *httputil.ReverseProxy {
	bal := client.getBalancer()

	transport := &http.Transport{
		TLSHandshakeTimeout: 40 * time.Second,
	}

	// TODO: would be good to make this sensitive to QOS, which
	// right now is only respected for HTTPS connections. The
	// challenge is that ReverseProxy reuses connections for
	// different requests, so we might have to configure different
	// ReverseProxies for different QOS's or something like that.
	if client.ProxyAll() {
		transport.Dial = bal.Dial
	} else {
		transport.Dial = detour.Dialer(bal.Dial)
	}

	allAuthTokens := bal.AllAuthTokens()
	return &httputil.ReverseProxy{
		// We need to set the authentication tokens for all servers that we might
		// connect to because we don't know which one the dialer will actually
		// pick. We also need to strip out X-Forwarded-For that reverseproxy adds
		// because it confuses the upstream servers with the additional 127.0.0.1
		// field when upstream servers are trying to determin the client IP.
		// We need to add also the X-Lantern-Device-Id field.
		Director: func(req *http.Request) {
			req.Header.Del("X-Forwarded-For")
			req.Header.Set("X-LANTERN-DEVICE-ID", client.DeviceID)
			for _, authToken := range allAuthTokens {
				req.Header.Add("X-LANTERN-AUTH-TOKEN", authToken)
			}
		},
		Transport: &errorRewritingRoundTripper{
			withDumpHeaders(false, transport),
		},
		// Set a FlushInterval to prevent overly aggressive buffering of
		// responses, which helps keep memory usage down
		FlushInterval: 250 * time.Millisecond,
		ErrorLog:      log.AsStdLogger(),
	}
}

// withDumpHeaders creates a RoundTripper that uses the supplied RoundTripper
// and that dumps headers is client is so configured.
func withDumpHeaders(shouldDumpHeaders bool, rt http.RoundTripper) http.RoundTripper {
	if !shouldDumpHeaders {
		return rt
	}
	return &headerDumpingRoundTripper{rt}
}

// headerDumpingRoundTripper is an http.RoundTripper that wraps another
// http.RoundTripper and dumps response headers to the log.
type headerDumpingRoundTripper struct {
	orig http.RoundTripper
}

func (rt *headerDumpingRoundTripper) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	proxy.DumpHeaders("Request", &req.Header)
	resp, err = rt.orig.RoundTrip(req)
	if err == nil {
		proxy.DumpHeaders("Response", &resp.Header)
	}
	return
}

// The errorRewritingRoundTripper writes creates an special *http.Response when
// the roundtripper fails for some reason.
type errorRewritingRoundTripper struct {
	orig http.RoundTripper
}

func (er *errorRewritingRoundTripper) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	res, err := er.orig.RoundTrip(req)
	if err != nil {
		var htmlerr []byte

		// If the request has an 'Accept' header preferring HTML, or
		// doesn't have that header at all, render the error page.
		switch req.Header.Get("Accept") {
		case "text/html":
			fallthrough
		case "application/xhtml+xml":
			fallthrough
		case "":
			// It is likely we will have lots of different errors to handle but for now
			// we will only return a ErrorAccessingPage error.  This prevents the user
			// from getting just a blank screen.
			htmlerr, err = status.ErrorAccessingPage(req.Host, err)
			if err != nil {
				log.Debugf("Got error while generating status page: %q", err)
			}
		default:
			// We know for sure that the requested resource is not HTML page,
			// wrap the error message in http content, or http.ReverseProxy
			// will response 500 Internal Server Error instead.
			htmlerr = []byte(err.Error())
		}

		res = &http.Response{
			Body: ioutil.NopCloser(bytes.NewBuffer(htmlerr)),
		}
		res.StatusCode = http.StatusServiceUnavailable
		return res, nil
	}
	return res, err
}
